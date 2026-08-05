# 研发节点多 Agent 评审方案

[返回设计索引](README.zh-CN.md)

> 状态：专项实施方案
> 更新日期：2026-08-05
> 适用范围：Nasuta Feature Delivery 全流程
> 前置方案：[Nasuta 多 Agent 平台方案](12-multi-agent-platform-proposal.zh-CN.md)
> 依赖基线：模块 08、09、10

## 1. 结论

推荐在 Feature Delivery 每个研发节点引入：

```text
不可变 Subject
  -> 多个独立 Reviewer Agent 并行评审
  -> 结构化 Review Report / Finding
  -> 确定性 Review Gate
  -> 必要时 Adjudicator
  -> 人工批准、拒绝或显式 Waiver
```

核心规则：

1. Reviewer 不能评审自己生成的 Artifact；
2. 第一轮 Reviewer 互相不可见，避免锚定和从众；
3. 不使用简单多数投票；
4. Reviewer 只提交有证据的 Finding，不能修改 Subject 或批准阶段；
5. Gate 由确定性规则计算，不由另一个模型自由决定；
6. Adjudicator 只处理冲突、重复和证据争议，不替代人工批准；
7. 每个新 Artifact、Change Set 或 Validation 版本创建新 Review Round；
8. 人工 Review 必须绑定当前 Review Round 和 Subject Hash；
9. 未完成、失败或缺失的必需 Reviewer 不能被当作通过；
10. Agent 评审默认只读，不获得 Coding、写工具或审批权限。

## 2. 为什么不是“多 Agent 投票”

简单多数投票存在四个问题：

- 多个 Reviewer 可能共享同一模型、Prompt 偏差和错误证据，票数不代表独立性；
- 一个有证据的严重缺陷不能被多个宽松 Reviewer 的“通过”抵消；
- “通过/不通过”无法指导返工；
- 无法区分评审缺失、证据不足、意见冲突和真实质量问题。

因此评审输出的基本单位是 `Finding`，Gate 根据覆盖度、严重级别、证据质量、冲突和策略进行确定性判定，而不是计票。

## 3. 目标与非目标

### 3.1 目标

1. Feature Delivery 每个节点都获得与阶段风险匹配的多视角 Agent 评审。
2. 评审与生成解耦，降低自证偏差。
3. 所有问题可定位、可复核、可跟踪到修复或 Waiver。
4. 高风险问题在进入下游前阻断。
5. 人工 Reviewer 看到聚合后的高信号结果，而不是多份重复长文。
6. 评审成本、延迟、发现率、误报率和人工采纳率可度量。
7. 保持 Feature Artifact 谱系、Coding Provider、独立验证和人工审批的现有边界。

### 3.2 非目标

- 不让 Agent 自动批准需求、设计、代码或发布；
- 不让 Reviewer 直接修改 Artifact 或代码；
- 不用 Reviewer 数量替代独立测试和验证；
- 不审查模型隐藏推理；
- 不把通用 Review Gate 写死为具体业务词汇；
- 不在第一版自动 Push、建 PR、合并或部署；
- 不因一个 Reviewer 失败而静默换 Provider；
- 不允许 Reviewer 自行创建更多 Reviewer。

## 4. 当前基线与差距

Feature Delivery 已经拥有：

- 不可变、版本化 Artifact 和精确父谱系；
- `ArtifactReview` 人工批准/拒绝；
- Generation Run 和 Evidence Snapshot；
- Coding Provider 与隔离 Worktree；
- Implementation Run、Claim、Lease 和 Event；
- Nasuta 独立执行的 Validation；
- Change Set 和交付人工审核。

当前主要差距：

- 一个 Artifact 只有单个人工 Review 结论；
- 没有 Review Round 和多个独立 Assignment；
- 没有统一 Finding Schema、严重级别和证据要求；
- 没有按阶段配置 Reviewer Panel；
- 没有自动 Gate、冲突处理和 Waiver；
- 人工批准未强制绑定一轮完整 Agent 评审及 Subject Hash；
- 无法统计每个 Reviewer 的真实缺陷发现率和误报率。

## 5. 评审对象

`ReviewSubject` 是一次评审的不可变对象：

```go
type ReviewSubject struct {
    Kind          SubjectKind
    ID            string
    Version       int
    ContentHash   string
    ParentHash    string
    EvidenceHash  string
    Repository    string
    BaseCommit    string
    HeadCommit    string
}
```

首期 Subject Kind：

- `requirement_artifact`；
- `requirement_analysis_artifact`；
- `technical_proposal_artifact`；
- `system_design_artifact`；
- `implementation_plan_artifact`；
- `change_set`；
- `validation_bundle`；
- `delivery_bundle`。

任何正文、证据快照、Patch、Base/Head Commit 或验证结果变化，都会产生新的 Subject Hash 和 Review Round。旧 Round 保留历史，但不能批准新 Subject。

## 6. Review Policy 与 Panel

每个阶段使用版本化 `ReviewPolicy`：

```go
type ReviewPolicy struct {
    ID                   string
    Version              int64
    SubjectKind          SubjectKind
    RequiredReviewers    []ReviewerSpec
    OptionalReviewers    []ReviewerSpec
    BlockingSeverities   []Severity
    RequiredCategories   []Category
    EvidenceRequirements EvidencePolicy
    Adjudication         AdjudicationPolicy
    Budget               ReviewBudget
    ContentHash          string
}
```

一次 Review Round 固定 Policy Snapshot。运行期间修改 Panel 或阈值不影响当前 Round。

Reviewer 的独立性至少体现在职责和 Prompt 不同。高风险阶段优先使用不同模型系列或独立配置，但不能把 Provider 自动替换包装成独立性。某个配置的 Provider 失败时，该 Assignment 明确失败。

## 7. 各研发节点 Reviewer Panel

### 7.1 推荐矩阵

| 节点 | 必需 Reviewer | 主要检查 | 典型阻断项 |
|---|---|---|---|
| 需求输入 | 需求清晰度、验收可验证性、业务风险 | 问题、范围、约束、验收和阻塞问题 | 目标矛盾、关键角色/范围缺失、验收不可判定 |
| 需求分析 | 产品一致性、领域规则、可测试性 | Goal/Non-goal、流程、规则、边界和指标 | 与原需求冲突、关键规则遗漏、Blocking Question 未解决 |
| 技术方案 | 架构、安全、可靠性 | 方案比较、依赖、兼容、性能、运维和可逆性 | 无真实备选、越权设计、关键安全/可靠性风险 |
| 系统设计 | 边界与合同、数据并发、可运维性 | 所有权、接口、数据、一致性、恢复、观测 | 循环依赖、数据归属不清、并发/恢复缺口 |
| 实现计划 | 可实施性、测试策略、迁移发布 | 路径、顺序、合同、验证、回滚和完成条件 | 计划无法执行、关键测试/迁移遗漏、修改边界不清 |
| 代码变更 | 正确性、安全、测试证据 | Diff、行为回归、边界、安全、复杂度和测试 | Critical/High 缺陷、越权修改、验证缺失 |
| 独立验证 | 覆盖完整性、回归风险、失败分析 | 命令、结果、未测范围、Flake 和环境差异 | 必需验证失败、未配置却声称通过、关键范围未覆盖 |
| 交付审核 | 发布准备、回滚、残余风险 | Change Set、验证、迁移、监控、回滚和未决项 | 无可执行回滚、风险未披露、Subject/Commit 不一致 |

### 7.2 Panel 规模

推荐默认每阶段 2–3 个必需 Reviewer：

- 低风险文档阶段：2 个；
- 架构、系统设计、代码和交付阶段：3 个；
- 安全敏感、数据迁移或高风险变更：按 Policy 增加专项 Reviewer；
- Optional Reviewer 失败不伪装为已覆盖，报告中明确缺失。

Panel 不是固定越多越好。每个 Reviewer 必须通过 Evaluation 证明能够发现独特问题，否则合并职责或移除。

### 7.3 按变更风险动态增补

动态增补只能基于确定性风险标签，例如：

- 涉及认证、授权、Secret、外部输入：增加 Security Reviewer；
- 涉及 Schema、Migration、并发写：增加 Data/Concurrency Reviewer；
- 涉及公开 API：增加 Compatibility Reviewer；
- 涉及配置、部署、告警：增加 Operations Reviewer。

风险标签来自 Artifact 字段、受影响路径和显式分析结果。模型可以提出标签建议，但最终启用规则由 Policy 决定。

## 8. Reviewer 执行隔离

每个 Assignment：

- 固定 Reviewer Agent Definition；
- 固定 Review Policy 和 Subject Hash；
- 使用独立 Agent Run；
- 只接收该角色需要的 Subject、上游合同和有界证据；
- 拥有独立 Tool Snapshot 和预算；
- 不读取其他 Reviewer 的 Report；
- 不读取人工最终 Decision；
- 不修改 Subject、Finding、Gate 或 Approval。

生成 Agent 和 Reviewer Agent 的 Definition ID 必须不同。默认禁止同一次 Generation Run 的模型上下文被直接复用为 Review 上下文。

代码评审读取精确 Base/Head Commit、Diff、必要源码和验证证据。Reviewer 不能只根据 Provider 的改动摘要评审。

## 9. Review Report 与 Finding

### 9.1 Review Report

```go
type ReviewReport struct {
    RoundID       string
    AssignmentID  string
    ReviewerID    string
    SubjectHash   string
    Coverage      []CoverageItem
    Findings      []Finding
    Uncertainties []Uncertainty
    Summary       string
    CompletedAt   time.Time
}
```

Report 必须通过 JSON Schema，且 `SubjectHash` 必须匹配 Assignment。自由文本 Summary 不能覆盖结构化 Finding。

### 9.2 Finding

```go
type Finding struct {
    ID             string
    Category       string
    Severity       Severity
    Claim          string
    Impact         string
    Evidence       []FindingEvidence
    Location       *Location
    Recommendation string
    Confidence     float64
    Fingerprint    string
}
```

字段要求：

- `Claim` 描述具体问题，不写泛化评价；
- `Impact` 说明违反的合同或可能后果；
- `Evidence` 引用 Subject、代码、测试或权威资料；
- `Location` 尽可能定位 Artifact 字段、文件和行；
- `Recommendation` 描述解决条件，不直接修改对象；
- `Confidence` 用于人工排序，不用于抵消 Severity；
- `Fingerprint` 用规范化 Category、Location 和 Claim 生成，用于聚合候选。

### 9.3 Severity

| 级别 | 定义 |
|---|---|
| `critical` | 可能造成严重安全、数据、生产或不可恢复后果，必须阻断 |
| `high` | 明确违反核心合同或高概率造成功能/交付失败，必须阻断 |
| `medium` | 应在进入下游前修复，是否阻断由阶段 Policy 决定 |
| `low` | 有价值但不阻断，可进入改进清单 |
| `info` | 观察、建议或需要人工关注的信息 |

严重级别由影响定义，不由措辞强弱决定。Critical/High Finding 必须包含可复核 Evidence；缺少证据时 Report Schema 可通过，但 Gate 将其标记为 `unsupported` 并要求人工处理，不能自动作为已确认缺陷。

## 10. Finding 聚合与生命周期

### 10.1 聚合

确定性聚合先按 Fingerprint 和位置形成候选组，再保留：

- 所有原始 Finding；
- 各 Reviewer 的独立 Evidence；
- 最高 Severity；
- Severity 分歧；
- Claim 冲突；
- 支持 Reviewer 数量。

聚合不能删除少数派的 Critical/High Finding。文本相似度只用于候选分组，不能自动判定两个 Finding 等价。

### 10.2 生命周期

Finding 本身不可变。状态由追加的 `FindingResolution` 事实推导：

```go
type FindingResolution struct {
    FindingID       string
    Resolution      string
    SubjectHash     string
    ReplacementHash string
    Rationale       string
    ActorID         int64
    ExpiresAt       *time.Time
    CreatedAt       time.Time
}
```

允许的 Resolution：

- `fixed`：新 Subject 版本已修复，并由新一轮评审验证；
- `waived`：授权人工接受风险，必须有理由和可选到期时间；
- `invalidated`：经证据复核确认为误报；
- `superseded`：Subject 已被新版本替代，旧 Finding 不再作为当前 Gate 输入。

当前状态从 Finding、Subject 谱系和 Resolution 推导，不在 Finding 上重复维护可漂移字段。

## 11. Review Round、Assignment 与 Gate

### 11.1 Review Round

```go
type ReviewRound struct {
    ID            string
    Subject       ReviewSubject
    Policy        ReviewPolicyRef
    Status        ReviewRoundStatus
    CreatedBy     int64
    CreatedAt     time.Time
    CompletedAt   *time.Time
}
```

Review Round 需要最小状态机，因为它包含并行 Assignment、重试、取消、Gate 计算和服务重启恢复：

```text
created -> running -> evaluating -> completed
任意活动状态 -> failed | cancelled
```

`completed` 表示本轮自动评审和 Gate 已形成不可变结果，不等于 Artifact 已被人工批准。

### 11.2 Review Assignment

```text
queued -> running -> succeeded
                 \-> failed | cancelled
```

重试创建新的 Attempt，不覆盖失败记录。必需 Assignment 未成功时 Gate 不得返回 `pass`。

### 11.3 Gate 结果

```go
type ReviewGateResult struct {
    RoundID       string
    SubjectHash   string
    Decision      GateDecision
    ReasonCodes   []string
    BlockingIDs   []string
    ConflictIDs   []string
    CoverageGaps  []string
    PolicyHash    string
}
```

Gate Decision：

- `pass`：必需评审完整且没有未解决阻断项；
- `revise`：存在有效阻断 Finding，应创建新 Subject 版本；
- `human_required`：存在证据冲突、Waiver 请求或 Policy 指定高风险；
- `incomplete`：必需 Reviewer、Schema、证据或覆盖度不完整；
- `failed`：评审基础设施或 Policy 无法可靠执行。

### 11.4 确定性 Gate 规则

默认顺序：

1. 校验 Round、Policy 和 Subject Hash；
2. 校验所有必需 Assignment 成功；
3. 校验 Report Schema 和必需 Category 覆盖；
4. 校验 Finding Evidence 最低要求；
5. 聚合重复 Finding；
6. 检查未解决 Critical/High；
7. 检查 Policy 指定的 Medium 阈值；
8. 检查 Reviewer 间高严重级别冲突；
9. 输出 Decision 和明确 Reason Code。

任何 Critical/High 的“通过票”都不能抵消另一个有效阻断 Finding。

## 12. Adjudicator

Adjudicator 不是常驻 Reviewer，只在以下情况触发：

- 两个 Reviewer 对同一 Claim 给出相反事实判断；
- Severity 跨越阻断阈值；
- Finding 疑似重复但 Evidence 指向不同问题；
- Critical/High Finding 的 Evidence 充分性存在争议；
- Policy 要求对特定高风险类别进行二次核验。

Adjudicator 输入包含 Subject、冲突 Finding 和原始 Evidence，不包含无关 Reviewer 身份或“多数意见”。输出：

```text
confirmed
not_supported
distinct_findings
needs_human
```

Adjudicator 不能：

- 修改原始 Finding；
- 把无证据问题判为通过；
- 生成 Artifact 新版本；
- 批准阶段；
- 执行写操作；
- 在配置 Provider 失败时切换 Provider。

Adjudication 结果作为附加证据进入 Gate。Critical 安全、数据损失、不可逆迁移等 Policy 指定问题即使经过 Adjudicator，仍可强制 `human_required`。

## 13. 与人工审核的关系

Agent Gate 是人工审核的前置质量门，不替代 `ArtifactReview` 或 `ChangeReview`。

人工 Decision 必须保存：

```text
subject_id
subject_hash
review_round_id
gate_result_id
decision
comment
reviewer
created_at
```

规则：

1. 只能审核当前 Subject Hash；
2. `incomplete` 或 `failed` 不能普通批准；
3. `revise` 需要新建版本，或由授权人员对每个阻断 Finding 创建 Waiver；
4. `human_required` 必须记录明确处置；
5. Subject 变化后旧人工 Decision 不适用于新版本；
6. 生成者和最终人工审批者的权限应分离；
7. UI 必须区分 Agent Gate 通过和人工批准，不能显示成同一种“Approved”。

## 14. 节点集成流程

### 14.1 文档 Artifact

```text
创建不可变 Artifact
  -> 创建 Review Round
  -> 并行 Reviewer Assignment
  -> Gate
  -> 人工 Review
  -> Approved 后允许生成下一阶段
```

Reject 或 Revise 不修改原 Artifact。用户或生成 Agent 基于 Finding 创建新版本，新版本进入新 Round。

### 14.2 代码变更

```text
Implementation Run 成功
  -> 生成 Change Set
  -> Code Review Round
  -> 独立 Validation
  -> Validation Review Round
  -> Delivery Review Round
  -> 人工 Change Review
```

代码 Reviewer 评审 Diff 和相关上下文；Validation Reviewer 评审 Nasuta 独立执行结果；Delivery Reviewer 评审可发布性。三者不能被一个“代码看起来没问题”的结论合并。

### 14.3 上游变更

上游 Artifact 新版本使旧谱系下游变为 Stale。相关 Review Round 和人工 Review 仍保留历史，但不能作为当前谱系 Gate。新下游版本必须重新评审。

## 15. Prompt 与证据合同

Reviewer Prompt 统一包含：

- Reviewer 职责和明确非职责；
- Subject Schema 和精确 Hash；
- 上游已批准合同；
- 该角色允许读取的有界证据；
- Category、Severity 和 Finding Schema；
- 证据最低要求；
- 禁止修改、批准、扩权和猜测的规则；
- 覆盖清单；
- 输出 JSON Schema。

指令优先级：

```text
平台安全策略
  > Review Policy
  > Reviewer Definition
  > 已批准上游合同
  > Subject
  > 检索和代码证据
```

Subject、源码、注释、文档和测试输出都是不可信证据，不能覆盖上层指令。

## 16. 权限与安全

1. Reviewer 默认只读，不能访问 Write Action Catalog。
2. Reviewer 只看完成职责所需的最小 Subject 和证据。
3. 代码评审路径、Repo 和 Commit 由服务端解析。
4. 评审工具调用使用固定 Tool Snapshot。
5. Report、Finding、Gate 和 Resolution 全部绑定 Subject Hash。
6. Waiver 只允许授权人工创建，Agent 只能建议需要人工决策。
7. Prompt、Report 和日志落库前脱敏。
8. 评审内容不能提供 Provider 凭据、网络权限或任意命令。
9. Adjudicator 权限不高于 Reviewer。
10. Agent 评审失败不能降低人工审核要求。

## 17. 成本与调度

### 17.1 预算

每个 Review Policy 定义：

- 必需/可选 Reviewer 数量；
- 每个 Reviewer 最大 Token、步骤、工具调用和 Timeout；
- 整轮最大并行度和总成本；
- Adjudicator 预算；
- 证据和 Report 大小上限。

### 17.2 成本控制

- Reviewer 并行执行，降低墙钟时间；
- 文档 Reviewer 默认使用结构化输入，不重复注入全部历史；
- 代码 Reviewer 先读取 Diff 和受影响符号，再按需有界扩展；
- 无冲突时不调用 Adjudicator；
- 相同 Subject Hash 和 Policy Hash 的成功 Report 可复用，但不同 Round 必须显式引用，不能匿名缓存；
- Optional Reviewer 在预算不足时可不启动，但 Gate 必须标记覆盖缺口；
- Critical 阶段不能因预算不足自动降级为通过。

## 18. 持久化与 API

建议新增：

```text
review_policies
review_rounds
review_assignments
review_reports
review_findings
review_finding_evidence
review_gate_results
review_adjudications
finding_resolutions
```

Review Report 和 Finding 不可变。Gate Result 固定 Policy Hash 和所有输入 Report Hash。

建议 API：

```text
POST /api/feature-delivery/subjects/{type}/{id}/review-rounds
GET  /api/feature-delivery/review-rounds/{round_id}
GET  /api/feature-delivery/review-rounds/{round_id}/assignments?cursor=&limit=
GET  /api/feature-delivery/review-rounds/{round_id}/findings?severity=&cursor=&limit=
GET  /api/feature-delivery/review-rounds/{round_id}/events?after_seq=&limit=
POST /api/feature-delivery/review-rounds/{round_id}/cancel
POST /api/feature-delivery/findings/{finding_id}/waivers
POST /api/feature-delivery/artifacts/{artifact_id}/reviews
POST /api/feature-delivery/change-sets/{change_set_id}/reviews
```

列表查询只返回摘要列；Finding Evidence、Report 正文和大 Diff 按 ID 单独读取。所有查询在 SQL 边界使用 Cursor/Limit。

## 19. 失败语义

| 失败 | Gate 行为 |
|---|---|
| 必需 Reviewer Provider 失败 | `incomplete` 或 `failed`，不替换 Provider |
| Optional Reviewer 失败 | 记录 Coverage Gap，按 Policy 决定是否 `human_required` |
| Report Schema 无效 | Assignment 失败；允许同 Provider 有限重试 |
| Subject Hash 不匹配 | 安全失败，Report 作废 |
| Evidence 不可用 | Finding/Report 标记证据不足，不能声称已验证 |
| Adjudicator 失败 | `human_required`，不自动忽略冲突 |
| Review Store 写失败 | 不产生 Gate Pass |
| Reviewer 超时 | Assignment 失败或有限重试 |
| Round 取消 | 活动 Run 取消，已完成 Report 保留审计 |
| 新 Subject 版本出现 | 旧 Round 不再作为当前 Gate，但不删除 |

## 20. 可观测性

### 20.1 Trace

```text
Feature Request
  -> Subject Version
    -> Review Round
      -> Reviewer Assignment
        -> Agent Run / Step / Model / Tool
        -> Review Report / Findings
      -> Adjudication
      -> Gate Result
    -> Human Review / Waiver
```

### 20.2 指标

至少记录：

- 每阶段 Round 完成率、失败率和 P50/P95 延迟；
- Reviewer Token、工具调用和成本；
- 每个 Reviewer 的 Finding 数、独特 Finding 数和重复率；
- Critical/High 的人工确认率；
- 人工判定误报率和遗漏回溯率；
- Finding 修复率、Waiver 率和平均关闭时间；
- Reviewer 间冲突率；
- Adjudicator 触发率和人工升级率；
- Gate 导致的返工次数；
- 进入下游后才发现的逃逸缺陷；
- 多 Agent 评审对总交付周期的影响。

## 21. 评估方法

上线前建立脱敏历史样本集，包含已知：

- 需求歧义；
- 架构边界问题；
- 安全和数据风险；
- 实现计划遗漏；
- 代码缺陷；
- 测试覆盖缺口；
- 发布和回滚问题。

按 Reviewer 评估：

- Precision：Finding 被人工确认的比例；
- Recall：已知缺陷被发现的比例；
- Unique Yield：只有该 Reviewer 发现的有效问题；
- Evidence Quality：引用是否充分、准确、可定位；
- Severity Calibration：严重级别是否合理；
- Actionability：建议是否能指导修复；
- Cost per Confirmed Finding；
- Latency。

按 Panel 评估：

- 相比单 Reviewer 的 Recall 提升；
- 重复率和冲突率；
- 对逃逸缺陷的降低；
- 人工审核时间是否下降；
- 总成本和交付周期是否可接受。

未达到最低 Precision 或长期没有 Unique Yield 的 Reviewer 应调整或移除。

## 22. 分阶段实施

### 阶段 0：规则和样本基线

- 固化各 Artifact Schema 和现有人工 Review 语义；
- 建立 Review Category、Severity 和历史缺陷样本；
- 记录当前人工评审时间、返工率和逃逸缺陷。

### 阶段 1：Review 数据模型

- 增加 Review Policy、Round、Assignment、Report、Finding 和 Gate；
- 人工 Review 增加 `review_round_id` 和 `subject_hash`；
- 先使用模拟 Reviewer 验证状态、API、并发和恢复。

### 阶段 2：设计 Artifact 试点

- 从 Technical Proposal 和 System Design 开始；
- 上线 Architecture、Security、Reliability/Data Reviewer；
- Gate 只提供建议和阻断提示，人工 Review 仍是唯一批准入口；
- 评估 Precision、Recall、成本和人工采纳率。

### 阶段 3：代码与验证

- 接入 Change Set、Validation Bundle；
- Reviewer 读取精确 Diff、Commit 和独立验证结果；
- Critical/High 和验证失败进入强制阻断；
- 引入 Finding Resolution 和 Waiver。

### 阶段 4：覆盖全部节点

- 扩展到 Requirement、Analysis、Plan 和 Delivery；
- 按风险标签动态增补 Reviewer；
- 人工批准强制绑定完整且当前的 Review Round。

### 阶段 5：Adjudicator 与优化

- 只对历史数据显示的高价值冲突启用 Adjudicator；
- 根据 Evaluation 合并低收益 Reviewer；
- 调整 Panel、预算和 Gate 阈值；
- 建立 Reviewer/Policy 版本对比和灰度发布。

每阶段执行最窄领域测试、`GOWORK=off go test ./...`、`GOWORK=off go build ./...`；并行 Assignment、Round Gate 和重试恢复必须执行 Race Test。

## 23. 测试策略

1. **Subject 绑定**：任意内容或 Commit 变化都会使旧 Round 失效。
2. **独立性**：第一轮 Reviewer 输入不包含其他 Report。
3. **Schema**：无效 Report、Finding、Severity、Evidence 必须被拒绝或明确降级。
4. **Gate 决定性**：相同 Policy/Reports 产生相同结果，与完成顺序无关。
5. **非多数投票**：单个有效 Critical/High 不能被多个 Pass 抵消。
6. **权限**：Reviewer、Adjudicator 无法调用写工具、批准或修改 Subject。
7. **失败矩阵**：必需/可选 Reviewer、Provider、Store、Adjudicator 和 SSE 失败。
8. **并发与恢复**：重复 Claim、超时、取消、服务重启和重复事件。
9. **有界读取**：Round、Assignment、Finding、Event 和 Evidence 查询都有 Limit。
10. **端到端**：Artifact -> Round -> Gate -> Human Review -> 新版本 -> Finding Resolution。

## 24. 风险与控制

| 风险 | 控制 |
|---|---|
| 多 Reviewer 产生大量重复噪声 | 严格 Finding Schema、候选聚合、评估 Unique Yield |
| 同模型造成伪独立 | 不同职责与上下文；高风险阶段评估模型多样性 |
| 误报阻塞研发 | Evidence 要求、Adjudicator、人工 invalidated/Waiver |
| Agent 互相从众 | 第一轮隔离，完成后再聚合 |
| 评审成本过高 | 2–3 个必需 Reviewer、风险增补、按需证据和预算 |
| Gate 规则失控 | 版本化 Policy、确定性 Reason Code、离线回放 |
| 评审后 Subject 被替换 | Round、Gate 和人工 Review 全部绑定 Hash |
| Reviewer 变成写 Agent | Tool Snapshot 只读，禁止 Action Catalog |
| 人工把 Agent Pass 当成批准 | UI 和数据模型区分 Gate 与 Human Decision |
| 状态漂移 | Finding 状态从不可变 Resolution 事实推导 |

## 25. 验收标准

1. Feature Delivery 的八类 Subject 都可创建版本绑定的 Review Round。
2. 每轮至少两个独立 Reviewer，第一轮互相不可见。
3. Reviewer 输出严格的 Review Report 和 Finding Schema。
4. 所有 Critical/High Finding 有可复核 Evidence 和位置，或被明确标为证据不足。
5. Gate 按版本化确定性 Policy 计算，不使用简单多数票。
6. 必需 Reviewer 缺失、失败或 Schema 无效时不能返回 `pass`。
7. Adjudicator 只在冲突策略触发时运行，不能批准或修改 Subject。
8. Reviewer、Adjudicator 不拥有写工具、Coding 和审批权限。
9. 新 Subject 版本自动使旧 Round 不再适用于当前 Gate。
10. 人工 Review 绑定当前 Subject Hash、Review Round 和 Gate Result。
11. Finding 可追溯到 `fixed`、`waived`、`invalidated` 或 `superseded` Resolution。
12. Code Review 使用精确 Diff/Base/Head Commit，Validation Review 使用 Nasuta 独立验证结果。
13. Provider 失败可见且不静默替换。
14. Round、Assignment、Finding 和 Event 查询在存储边界有界。
15. 上线前通过历史样本证明 Panel 相比单 Reviewer 提高有效缺陷发现率，且成本和周期在配置预算内。
