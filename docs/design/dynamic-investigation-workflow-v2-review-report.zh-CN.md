# 动态规划多 Agent Workflow v2 实现审查报告

- **审查对象**：`dynamic-investigation-workflow-v2-implementation-tasks.zh-CN.md` 及当前代码实现
- **失败日志**：`/Users/dequan.mac/.codex/attachments/117890a2-f6b4-43bb-9592-eb0709db8aa2/pasted-text.txt`
- **审查日期**：2026-08-22
- **代码分支**：`feat/multi-agent-platform`
- **审查性质**：实现对齐、生产闭环、失败链路和代码质量审查
- **本次变更**：仅新增本审查文档，未修改业务代码，未创建 commit

> 附件日志时间为 `2026-08-23 00:59`，晚于本审查环境日期 `2026-08-22`。该差异可能来自日志回放、机器时钟或时区配置，不能直接作为业务失败原因，但应在运行时日志链路中确认时间来源。

---

## 1. 结论摘要

当前 v2 实现已经完成了以下核心骨架：

- `InvestigationRun`、Contract、Plan、Task、Budget、EvidenceLedger、ClaimLedger、DeliveryResult 等领域对象；
- 任务模板 Catalog、候选生成、DAG 校验和 Scheduler；
- Investigator、Verifier、Composer 三类执行边界；
- Evidence/Claim 的准入和覆盖判断；
- DeliveryGate 和 DeterministicRenderer，能够避免空答案成功返回；
- 部分超时、取消、幂等、TaskAttempt 重试、Replay 和 Resume；
- REST/SSE 读取同一个 v2 `DeliveryResult` 的主要路径。

但是，当前还不能认定为“生产级闭环完成”。最重要的缺口是：

1. **上层 QA 生成的 `TaskGraphProposal` 没有进入 v2 Coordinator 的实际计划编译链路**；
2. **Lease 只有获取和释放，没有生产续租；RunStore 写入没有 fencing token 或 CAS 保护**；
3. **调查任务输入丢失了 source、facet、InputRefs 等 Contract 语义**；
4. **架构概览问题的检索结果缺少高质量、可覆盖目标的 canonical evidence**；
5. **稳定架构问题可能触发实时 Kibana/SkyWalking 观测，造成噪声和成本**；
6. **关键 task、attempt、证据准入、claim 验证和 DeliveryResult 状态没有完整记录到日志**；
7. **真实 MySQL、重启并发恢复、取消和接口回放尚未形成生产验收证据**。

因此，当前应定义为：

> **核心机制已落地，生产闭环未完成；普通单进程和内存/伪执行器路径可用，跨进程恢复和真实知识库端到端能力仍需补齐。**

---

## 2. “部分完成”的判定标准

本报告中的“部分完成”不是“功能不可用”，而是指：

> 主路径已有实现，普通单进程或理想条件下可以运行；但设计要求中的关键异常场景、跨进程一致性、数据质量、接口闭环或生产验收仍然缺失。

| 状态 | 判定含义 |
| --- | --- |
| **已完成** | 代码路径存在，关键不变量有测试，未发现设计文档中明确的主要缺口 |
| **部分完成** | 核心机制已经实现，但至少有一个重要生产场景、异常场景、跨进程场景或验收条件未满足 |
| **未完成** | 设计要求的主要代码路径尚未实现，或当前无法运行 |

例如：

- 有 `RenewLease` 接口和实现，但生产 Coordinator 从不调用它，属于“Lease 机制部分存在”，不是完整 fencing；
- 有 REST/SSE/MCP 结果读取，但 MCP 没有调查发起入口，属于“结果读取完成、三出口调查闭环未完成”；
- 有 p95 成本计算函数，但没有更新 live catalog，属于“成本校准算法完成、运行治理未完成”。

---

## 3. 与实施任务文档的对齐结果

### 3.1 文档自身的状态矛盾

`docs/design/dynamic-investigation-workflow-v2-implementation-tasks.zh-CN.md` 存在以下矛盾：

- 第 3 行：明确写出 `P1-06 仍有 fencing 缺口`；
- 第 25 行：`P0-02 部分完成`，缺口为没有跨进程 fencing/lease；
- 第 39 行：又将 `P1-06` 标记为“已完成”，并声称 MySQL/Memory 跨进程 fencing/lease 已接入。

根据当前代码，P1-06 不应标记为“已完成”，建议改为“部分完成”。

### 3.2 重新评估表

| 任务 | 重新评估 | 主要依据 |
| --- | --- | --- |
| P0-01 领域契约与 Schema | 基本完成 | v2 对象和状态已建立，但 adapter 转换时会丢失部分执行语义 |
| P0-02 Run 生命周期与持久化 | **部分完成** | 有快照、事件、Replay、Resume；没有跨进程 fencing 写保护 |
| P0-03 BudgetLedger | 基本完成 | 有分阶段预算、Reserve/Settle 和指标 |
| P0-04 Evidence/Claim Ledger | **部分完成** | 有准入、覆盖和冲突；多来源证据合并较浅 |
| P0-05 DeliveryGate/Renderer | 基本完成 | 有空答案保护和 deterministic fallback |
| P0-06 Template Catalog | 基本完成 | 有版本、启停、废弃和内存 audit；治理能力不完整 |
| P0-07 PlanCompiler/DAG | **部分完成** | DAG、schema、权限校验存在，但不消费上层 TaskGraphProposal |
| P0-08 Scheduler | 基本完成 | 有并行和任务限制；真实跨进程调度未验收 |
| P0-09 Investigator/Verifier/Composer | **部分完成** | 三类执行边界存在，但 Investigator 输入没有完整保留 Contract 语义 |
| P0-10 最小端到端回归 | 测试层面完成 | 主要依赖内存 Store、伪执行器或本地 fixture |
| P1-01 Gap 驱动 Replan | 基本完成 | 已完成发现类型匹配、确定性收益评分、预算约束选择和依赖闭合；运行时风险概率和历史校准仍留在 P2 |
| P1-02 更多模板 | 基本完成 | 配置、API、文档、运行时模板已注册 |
| P1-03 多执行器并行 | 基本完成 | Scheduler 已有 Agent/Tool 并发维度，但实际 Proposal 未进入 v2 计划 |
| P1-04 完整证据验证与冲突 | **部分完成** | 已完成跨任务 provenance union、冲突自动降级、引用/置信度 canonicalization 和缺口公开；来源权威性合并、事实审计和完整冲突视图仍不完整 |
| P1-05 REST/MCP/SSE 统一交付 | **部分完成** | 结果读取基本统一；MCP 没有独立调查发起入口 |
| P1-06 恢复/超时/取消/幂等 | **部分完成** | lease 获取存在；没有续租、fencing token 和完整 attempt 恢复 |
| P1-07 预算 profile 和运行指标 | 基本完成 | run 内指标和聚合指标已有 |
| P1-08 生产级故障注入 | **部分完成** | 已有工具不可用、推理截断、provider timeout 专项；重启并发不足 |
| P2-01 成本模型优化 | 部分完成 | p95 校准函数存在，但未更新运行 catalog |
| P2-02 并发/上下文优化 | 部分完成 | Evidence pruning 已进入 Composer；provider/model 路由缺失 |
| P2-03 模板生命周期治理 | 部分完成 | 内存级废弃、List/Resolve 过滤和 audit 已有；发布、兼容、权限和成本审计缺失 |
| P2-04 历史迁移和旧链路删除 | 部分完成 | 旧链路删除较多；普通 QA recovery 边界需继续验证 |
| P2-05 评估集和持续回归 | 部分完成 | EvaluationSuite 可 JSON 持久化；未接入 CI/部署回放 |

---

## 4. 最高优先级实现问题

### 4.1 TaskGraphProposal 与 v2 Coordinator 断链

#### 现状

QA 提交流程会生成并传递：

```go
InvestigationRequest{
    Contract: contract,
    Proposal: proposal,
}
```

位置：

- `internal/agent/qa/submission.go:124-142`
- `internal/agent/qa/investigation.go:71-80`

但是 `app/qaInvestigator.Start` 只将 `TaskContract` 转为 `InvestigationContract`，然后调用：

```go
coordinator.Execute(ctx, contract)
```

位置：

- `app/investigation_runner.go:42-58`
- `app/investigation_runner.go:304-350`

`InvestigationContract` 没有 Proposal 字段，Coordinator 也没有接收 Proposal 的入口：

- `internal/agent/investigation/contract.go:136-152`
- `internal/agent/investigation/coordinator.go:520-528`

Coordinator 最终调用：

```go
compiler.CompileGenerated(contract)
```

而不是根据 QA 已生成的 Proposal 编译计划。

#### 影响

- 上层多 Agent 规划结果不会决定实际执行计划；
- 日志中的 `effective=multi_agent` 不能证明预期任务真的执行；
- Proposal 的 task、edge、InputRefs、预算和并行组可能全部丢失；
- 用户的“针对三个核心业务”没有映射成三个可独立验证的业务任务；
- QA route 和 v2 execution plan 可能出现表面一致、实际不一致。

#### 修复要求

建立明确的 server-side 编译链：

```text
TaskGraphProposal
  -> server registry validation
  -> TaskSpec / TaskEdge 转换
  -> TaskCandidate / ExecutableTask
  -> PlanCompiler.Compile
  -> 持久化 PlanRevision
```

不能直接信任 LLM 指定的 tool 或 executor，仍应由服务端 capability/template registry 决定实际实现。

#### 必须增加的测试

- 两个不同 Proposal 必须生成不同 PlanRevision；
- Proposal 的 edge 必须进入 DAG；
- Proposal 的 InputRefs 必须进入 task 输入；
- Proposal 的 budget、attempt 和 stop policy 必须生效；
- 持久化计划必须能回放出原始 Proposal 的核心身份/hash。

---

### 4.2 Lease 不是完整 fencing

#### 现状

Lease 接口和实现存在：

- `internal/agent/investigation/lease.go:18-24`
- `internal/agent/investigation/mysql_lease.go:25-95`

Coordinator 获取和释放 lease：

- `internal/agent/investigation/coordinator.go:143-165`
- `internal/agent/investigation/coordinator.go:398-403`
- `internal/agent/investigation/coordinator.go:187-192`

但生产代码没有调用 `RenewLease`。当前 RunStore 写接口也没有 owner/fencing token：

- `internal/agent/investigation/persistence.go:20-37`
- `internal/agent/investigation/mysql.go:157-222`

数据库 schema 没有 version/fencing 字段：

- `internal/platform/dbschema/mysql.go:888-907`

#### 风险

lease 过期后，旧进程仍可能写回：

- task result；
- evidence；
- claims；
- report；
- delivery；
- failure state。

因此新 worker 的恢复结果可能被旧 worker 覆盖。

#### 修复要求

至少需要：

1. Coordinator 后台续租；
2. 续租失败时取消当前执行；
3. 数据库维护单调递增 fencing token/version；
4. 所有 RunStore mutation 携带 token；
5. 使用条件更新或等价 CAS；
6. 旧 token 写入明确失败并记录；
7. 增加 lease 过期、旧 worker 晚到写入和恢复并发测试。

---

### 4.3 Investigator 输入没有保留完整调查契约

`internal/agent/investigation/runtime_executor.go:181-210` 当前会：

- 用 `goalID` 充当 facet；
- 将 source 固定为 `internal`；
- 将 freshness 固定为 `stable`；
- 没有完整携带 InputRefs、任务证据分配和上游交接内容。

这会造成任务执行依赖隐含约定，无法可靠表达：

- 自定义 goal ID 与 facet 的差异；
- runtime/web/memory 等非 internal 证据来源；
- 多来源和时效要求；
- 上游任务已经发现的实体和证据身份。

应直接从经过服务端校验的 Contract/TaskAssignment 构造 Investigator 输入，而不是重新根据 task ID 猜测 facet。

---

## 5. 失败日志分析

### 5.1 日志证明的执行过程

日志关键节点如下：

1. 上一轮结果已经是 `evidence_status="unavailable"`，且 7 个目标都没有 verified claim；
2. 本轮 `execution_task_audit` 返回 `candidate_tasks=[]`；
3. 服务端记录 `recovered=true tasks=2`；
4. canonical query plan 生成 7 个 required facets；
5. route 从 `single_agent` 提升到 `multi_agent`；
6. 预检索得到 `hitCount=5 contextLen=2284`；
7. 随后执行多次 `search_code`；
8. 执行 Kibana 和 SkyWalking 实时观测；
9. 最后保存 run，但日志没有给出完整 DeliveryResult 和每个 task 的终态。

### 5.2 直接失败原因

本次失败不是进程崩溃，而是证据交付失败：

- 没有识别出三个具体核心业务；
- 检索结果主要是 README、空模块、技术栈和不完整文档；
- 没有形成能覆盖 `business_domain`、`core_flow`、`data_and_state` 等目标的可信 claim；
- 最终只能返回调查限制或证据不可用。

### 5.3 结构性失败原因

#### 原因一：多 Agent 只是被路由出来，没有证明有效分工

日志记录：

```text
proposed=single_agent effective=multi_agent
 tasks=2 independent_tasks=2 capabilities=3
```

但是日志没有记录这两个 task 的完整内容，且代码路径表明上层 Proposal 没有进入 Coordinator。因此不能确认多 Agent 实际执行了预期的 capability 分工。

#### 原因二：任务没有先做业务实体发现

用户要求的是“三个核心业务”，但任务只包含两个抽象目标：

- `architecture_overview`
- `core_business_details`

没有产生：

- 三个业务实体；
- 每个业务对应的服务集合；
- 每个业务对应的入口和流程；
- 每个业务的独立证据覆盖要求。

#### 原因三：检索质量不够

日志中出现：

```text
AirBase/README.md trust=35
HSBase/README.md trust=35
specs/framework-docs/spec.md trust=35
IoTBusiness.h trust=100
```

同时，业务文档中的 Core Business Flows 为空或只有技术栈信息。5 个结果、2284 字符上下文不足以支撑七个证据维度和三个业务详述。

#### 原因四：稳定架构问题引入了实时运行噪声

Contract 指定的是：

```text
sources=["internal"]
freshness="stable"
```

但后续查询了 Kibana 和 SkyWalking，得到的是当前流量、设备、认证、Redis、MySQL 等运行样本。这些信息没有经过业务归类，反而挤占预算和上下文。

### 5.4 环境因素

真实 workspace 的调用链和 MCP fixture 存在索引数据缺失，测试中出现：

```text
sql: no rows in result set
```

这会降低真实链路的召回能力，是当前失败的放大因素。但它不能解释全部问题，因为即使索引完整，Proposal 断链、实体分解不足和 stable/live 路由问题仍然存在。

---

## 6. 其他代码质量和闭环风险

### 6.1 Resume 不是完整的 TaskAttempt 恢复

`Coordinator.Resume` 可以恢复已持久化结果，并将没有结果的任务重设为 pending：

- `internal/agent/investigation/coordinator.go:167-240`

但没有明确恢复：

- 正在执行的 attempt 所属 worker；
- 旧 worker 是否仍然运行；
- 重试原因；
- 新旧 worker 的写冲突；
- fencing token。

当前属于 snapshot-level resume，不是完整的 attempt-level recovery。

### 6.2 跨进程读取和跨进程等待没有统一

`qaInvestigator` 使用进程内 `runs` map 保存状态。进程重启后：

- `LoadRun` 和 `LoadDelivery` 可以从 Store 读取；
- `AwaitTerminal` 找不到内存状态时会返回 run not found；
- `LoadTerminal` 才能作为持久化读取补偿。

因此需要明确区分“跨进程读取”“跨进程继续执行”“跨进程等待”“跨进程订阅”和“跨进程取消”。

### 6.3 后台执行使用 `context.WithoutCancel`

`app/investigation_runner.go:55-58` 使用 `context.WithoutCancel(ctx)` 启动后台执行。若这是有意设计，需要配套：

- run-level timeout；
- 持久化取消；
- worker 感知取消；
- 资源和成本回收；
- 客户端断开后的状态可查询。

### 6.4 对外 Round 被硬编码为 1

`app/investigation_runner.go:364-368` 中 `InvestigationTerminal.Round` 固定为 1。动态 replan 或多轮 Resume 后，对外结果不能正确反映实际轮次。

### 6.5 失败路径中存在被忽略的持久化错误

`internal/agent/investigation/coordinator.go:484-500` 中：

- `SaveBudget` 错误被忽略；
- metrics 保存失败可能阻止最终失败状态写入；
- 失败指标写入错误可能覆盖原始业务失败原因。

这会降低故障可见性和恢复可靠性。

---

## 7. 已执行验证

### 7.1 `go vet ./...`

通过。

### 7.2 覆盖率

```text
internal/agent/investigation     74.7%
internal/agent/qa                73.8%
app                              22.6%
internal/transport/dashboard     21.4%
internal/transport/mcp           66.4%
```

核心包覆盖率较好，但 app、dashboard 的覆盖率较低，且覆盖率不能替代真实 MySQL、重启和多进程验收。

### 7.3 Race 目标测试

以下核心包通过：

- `internal/agent/investigation`
- `app`
- `internal/agent/qa`
- `internal/transport/dashboard`

MCP 测试仍受到真实 workspace fixture 的索引缺失影响，没有发现 race detector 报告。

### 7.4 `go test ./...`

大部分核心包通过。失败主要集中在真实 workspace callchain/MCP fixture，例如：

- Feign chain 无法闭合；
- HTTP WebClient chain 为空；
- RestTemplate evidence 未索引；
- MCP `trace_calls` / `trace_deps` 相关断言失败；
- 部分测试返回 `sql: no rows in result set`。

这些失败与已知索引环境问题一致，但也说明当前还没有真实端到端验收证据。

---

## 8. 修复优先级和验收顺序

### P0：先修会导致结果不可信或数据被覆盖的问题

1. Proposal 接入 v2 Coordinator；
2. 修复 Investigator 输入契约，保留 facet/source/freshness/InputRefs/entity；
3. 增加业务实体发现和每个业务独立调查机制；
4. stable evidence 与 live runtime 路由隔离；
5. 增加 required goal 的 evidence coverage gate；
6. 完善 DeliveryResult 的失败、限制和 coverage 持久化。

### P1：完成生产恢复安全

1. Lease renewal；
2. fencing token/version；
3. 所有 RunStore mutation 条件写；
4. 旧 worker 晚到结果拒绝；
5. 重启、取消、重复执行、并发 Resume 测试；
6. 完善 task/attempt/evidence/claim/delivery structured logging。

### P2：完成运营和持续治理

1. 将 p95 cost calibration 接入 catalog 更新和版本审计；
2. 完成 provider/model 路由；
3. 持久化模板生命周期 audit；
4. 增加 schema compatibility 和权限/成本审计；
5. 将 EvaluationSuite 接入 CI 和部署后 replay；
6. 补齐真实 workspace 的索引 fixture 和 MCP/callchain 测试。

---

## 9. 最终判定

当前实现不能简单归类为“失败”或“未实现”。更准确的判断是：

```text
v2 核心代码骨架：已形成
普通单进程调查流程：基本可运行
证据和交付保护：已形成
上层多 Agent 规划到执行：存在断链
跨进程 fencing：未闭环
真实重启并发恢复：未验收
架构/业务类问题的知识召回：当前不足
生产级多 Agent 回答：暂不能认为完成
```

本次日志暴露的不是单个模型调用失败，而是整个系统在以下环节之间没有形成闭环：

```text
用户问题
  -> 业务实体发现
  -> TaskGraphProposal
  -> v2 PlanRevision
  -> 受约束的证据检索
  -> EvidenceLedger
  -> ClaimLedger
  -> Coverage / Replan
  -> DeliveryResult
  -> REST / SSE / MCP
```

在 Proposal 接入、证据质量、fencing 和真实恢复验收完成之前，实施状态建议保持为：

> **P0 核心机制已实现但 P0-02 仍部分完成；P1 大部分完成但 P1-06、P1-08 尚未完成生产闭环；P2 处于收尾阶段。**

---

## 10. 按本报告修复后的复核结论（2026-08-22）

本轮已按上述问题对代码进行修复，主要变化如下：

- QA 产生的 `TaskGraphProposal` 已通过 `Coordinator.ExecuteWithProposal` 进入 `PlanCompiler.CompileProposal`，并持久化 proposal hash、任务目标绑定和受服务端控制的输出 schema；
- `EvidenceGoal`、`ExecutableTask` 和 Investigator runtime input 保留 sources、required sources、freshness、facets、minimum coverage、InputRefs、capability 和 entity；
- BudgetVector、reservation settle、runtime total-token projection 和 cost calibration 已统一 token 维度语义，未声明的 per-task token grant 不再误拒绝真实用量，但仍受 Run/Stage hard limit 约束；
- 失败路径会尽力保存 metrics/budget 并写入终态，持久化错误会和原始业务失败一起返回；Resume 的 deadline、任务持久化和 delivery 失败也会尝试写入明确终态；
- Lease 现在支持单调 `fencing_token`：Memory 实现用于测试和单进程，MySQL RunStore 在事务内校验 owner/token/expiry，并用条件更新拒绝晚到 worker；新增 migration：`docs/sql/migration_add_investigation_fencing_token_20260822.sql`；
- Coordinator 会续租，`AwaitTerminal` 在进程内状态不存在或不完整时回退到 durable polling，跨进程等待不再依赖进程内 map；
- Run event 增加 plan、task start/completion、evidence snapshot 和 delivery completion 的结构化 JSON message，Execute 与 Resume 都至少保留 task、executor、attempt、usage、failure code、proposal hash、coverage 等运行证据；
- Resume 在任务恢复、deadline/cancel 和 delivery 持久化后会保存 metrics/budget，并在 delivery 已进入终态后将 event、metrics、budget 的失败作为可观测的 post-delivery persistence error 返回，不再把已交付结果改写为失败；
- 对外 terminal round 不再固定为 1，而是使用持久化 metrics 的实际 round；
- EvidenceLedger 的完整 identity 已统一为 `SourceKind + Target + Section + Version + TimeRange`，Evidence ID 也纳入 Version/TimeRange；不同来源、版本和时间范围的证据不会被错误去重或误报冲突；
- EvidenceRef 现在校验完整 provenance 字段和 ContentHash，阻止 claim 以错误的 section、版本或时间范围引用已存在的 Evidence ID；新增定向测试覆盖工具证据和 identity-only seed；
- Claim ingress 现在在准入边界 canonicalize GoalID/Text、confidence、EvidenceRef 和 ConflictRef；EvidenceRef 会回填 admitted unit 的完整 identity；同一 Claim 的跨任务/跨来源证据引用会做稳定 union，并在合并后重新检查冲突，支持 claim 遇到显式或 identity 冲突会自动降级；
- Coverage 聚合 supporting evidence 的 Facets/SourceKind 并生成 `MissingFacets`/`MissingSources`，BuildReport 与 Composer limitations 会公开缺口；pruning 同时保留 support/conflict provenance；
- Gap-driven Replan 新增 `DiscoveryTypes` 模板能力声明：实体和依赖边会优先匹配适用的已注册模板，重复 discovery 被去重，候选按稳定 ID 排序，未知 discovery 形状不会进入计划；默认 code/API/runtime 模板已经接入该匹配；
- Replan 候选选择已接入确定性 bounded score 和 `StageExecution` 剩余预算：required/high-risk/多 Goal 覆盖、Goal 所需来源与模板 `SourceKinds` 匹配、`Provides/RequiredInputs` 依赖解锁、独立可执行性和模板成本都会影响排序；候选超出 `maxTasks` 时选择可覆盖 required Goal 的最高收益集合，而不是整轮返回空计划；
- 选择以依赖闭合为边界，已执行依赖视为满足，未执行依赖整体纳入本轮；按稳定 Task ID 输出，并对已执行候选过滤。没有历史统计依据时，失败概率和证据重复风险不被伪造，后者继续由 Evidence admission/policy 的运行时指标处理。

### 修复后的剩余验收项

以下内容仍不能仅凭单元测试宣称生产完成：

1. 已存在的 MySQL 实例必须先执行 fencing token migration，再部署 token-aware RunStore；
2. 需要真实 MySQL 做两个进程的 lease takeover、旧 worker 晚到写入、续租失败、重启 Resume、取消和接口回放验收；
3. 真实 workspace 的 callchain/MCP fixture 仍有索引数据缺失，相关 `sql: no rows in result set` 失败不能归因于本轮调查 workflow 修复；
4. 稳定架构问题与实时观测路由、证据来源权威性/优先级及事实级审计、provider/model 路由和 CI replay 仍属于后续收尾项；本轮已补齐 discovery-to-template 匹配、候选收益选择和预算/依赖闭合，并完善了 Claim provenance union 与 coverage 缺口公开；后续仍需把历史成本、失败率和重复证据率校准接入运行时 catalog，并完成真实生产验收。

本轮补充的定向验证包括：Memory lease takeover 的 token 单调性、MySQL lease takeover 的 token 递增、MySQL stale/current 条件写，以及无进程内状态时 `AwaitTerminal` 的 durable polling。上述测试不替代真实 MySQL、多进程和重启验收。

因此当前状态应更新为：

> **v2 普通单进程路径和核心安全机制已形成；MySQL token-aware fencing、durable AwaitTerminal 和失败终态修复已落地；生产级重启/并发验收、真实知识库召回质量和部分治理能力仍未完成。**
