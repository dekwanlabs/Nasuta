# Nasuta 需求到代码交付 Agent 设计

## 1. 状态与结论

- 状态：设计稿
- 所属模块：Nasuta
- 产品名称：研发任务
- 第一版交付边界：需求输入、需求分析、技术方案、系统设计、实现计划、代码变更生成、验证和人工审核

本文设计一条由 Nasuta 拥有的“需求到代码”交付链路：

```text
产品需求
  -> 需求分析
  -> 技术方案
  -> 系统设计
  -> 实现计划
  -> 代码实现
  -> 自动验证
  -> 人工审核变更
```

Nasuta 负责流程、知识证据、版本、审核、运行隔离、Provider 调度、审计和 API。Codex 或 Claude Code 只作为代码执行 Provider，在 Nasuta 准备的隔离 Git Worktree 中修改代码。

第一版明确采用以下决定：

1. 能力完全属于 Nasuta；下游应用可以消费 API，但不拥有业务逻辑。
2. 不把该能力放入 QA、Incident 或现有 Incident Approval。
3. 不从零实现代码编辑 Agent，统一调用 Codex CLI 或 Claude Code CLI。
4. Provider 由配置显式选择，失败时直接报错，禁止静默切换。
5. 需求、分析、方案、设计和计划都是不可变、可版本化的 Artifact。
6. 研发任务当前阶段根据 Artifact 谱系和审核记录推导，不保存冗余流程状态。
7. 代码实现 Run 拥有真实状态机，因为它需要并发 Claim、取消、超时、进程退出恢复和终态审计。
8. 每次实现固定系统设计版本、实现计划版本、目标仓库和 Base Commit。
9. 第一版只产出可审核的代码变更集，不自动 Push、创建合并请求、合并、迁移数据库或部署。
10. MySQL 是该能力的必要依赖；没有 MySQL 时整条研发任务能力禁用。
11. 每个需求所属用户复用一个 `<username>-workspace` 父目录；每个实现 Run 在该目录下创建独立 Worktree。

## 2. 背景

Nasuta 已经拥有：

- workspace 代码、文档和服务结构索引；
- `knowledge.API` 提供的代码搜索、服务搜索、依赖追踪和 Runbook 搜索；
- OpenAI/Anthropic LLM 调度；
- Dashboard REST、SSE、认证和用户上下文；
- MySQL 平台存储和分组迁移；
- QA Agent 运行记录、流式事件和 token 统计范式；
- Incident 分析及审批后 Git/VCS 写入范式。

这些能力能够支持研发分析，但当前没有一个持久化领域把产品需求、技术决策、设计版本和实际代码变更绑定起来。直接复用 `/api/qa/ask` 会产生以下问题：

- QA 答案是会话消息，不是可审批、可追溯的工程 Artifact；
- QA Run 不固定上游文档版本和代码 Base Commit；
- QA 写工具只面向 Incident，且不能成为通用代码执行入口；
- 对话回答没有需求、方案、设计和实现之间的谱系约束；
- 无法判断一份实现是否仍基于当前有效设计。

因此本功能建立独立的 Feature Delivery 领域，但复用 Nasuta 的知识、LLM、认证、存储和运行基础设施。

## 3. 目标与非目标

### 3.1 目标

1. 将产品原始需求转换成结构稳定的需求分析。
2. 基于当前代码和架构证据生成多个候选方案并给出明确决策。
3. 将已审核方案展开成可实施的系统设计。
4. 将系统设计转换成按仓库拆分的实现计划。
5. 调用 Codex 或 Claude Code 在隔离 Worktree 中实现代码。
6. 独立执行可信验证命令，不能只相信 Coding Provider 自报的测试结果。
7. 保存 Artifact 谱系、审核结论、代码基线、运行事件、Diff 摘要和验证结果。
8. 在需求或上游决策变化后，准确识别下游产物已经过期。
9. 对用户取消、服务重启、Provider 进程异常和数据库写入失败提供确定行为。
10. 对列表、事件、日志和 Diff 使用有界存储与有界读取。

### 3.2 非目标

第一版不包含：

- 自动 Push 分支、创建合并请求、合并代码或部署；
- 自动执行真实数据库迁移；
- 自动写入生产配置、外部工单或云资源；
- 无人工审核的全自动需求到生产；
- 多个 Coding Provider 同时执行同一个 Run；
- Provider 失败后自动改用另一个 Provider；
- 跨多个仓库的原子提交；
- 长期保留 Provider 的完整隐藏推理；
- 将研发任务暴露成 MCP 写工具；
- 将第三方 CLI 当作多租户安全沙箱。

需求分析和系统设计可以覆盖多个仓库；代码实现以“一个 Run 对应一个仓库”为边界。

## 4. 术语

| 术语 | 含义 |
| --- | --- |
| Feature Request | 一项研发任务的稳定容器 |
| Artifact | 需求、分析、方案、设计或实现计划的不可变版本 |
| Lineage | Artifact 与精确上游 Artifact ID 之间的谱系 |
| Current Lineage | 从最新需求版本开始，逐级选择最新审核通过且父级仍有效的完整链 |
| Review | 对一个不可变 Artifact 或代码变更集的批准或拒绝记录 |
| Generation | 使用 LLM 和知识证据生成一个 Artifact 版本 |
| Implementation Run | 固定设计、计划、仓库和 Base Commit 后的一次代码执行 |
| Change Set | Run 产生的文件变更、Patch、统计和验证结果 |
| Coding Provider | Codex 或 Claude Code 的显式执行后端 |
| User Workspace | 按需求所有者的规范用户名创建并长期复用的 Run 工作目录容器 |
| Worktree | 位于 User Workspace 下、从固定 Base Commit 创建的单 Run 隔离 Git 工作目录 |

## 5. 用户角色与权限

当前 Nasuta RBAC 只有用户、角色和菜单，没有动作级 Permission。第一版不为了本功能顺带建设完整授权系统，采用现有能力能够可靠表达的最小规则：

| 操作 | 普通已认证用户 | 管理员 |
| --- | --- | --- |
| 创建研发任务 | 允许 | 允许 |
| 查看自己的研发任务 | 允许 | 允许 |
| 查看他人的研发任务 | 禁止 | 允许 |
| 创建需求新版本 | 仅自己的任务 | 允许 |
| 生成分析、方案、设计和计划 | 仅自己的任务 | 允许 |
| 审核 Artifact | 禁止 | 允许 |
| 启动代码实现 | 禁止 | 允许 |
| 取消代码实现 | 禁止 | 允许 |
| 审核代码变更 | 禁止 | 允许 |
| 下载 Patch | 仅自己的任务 | 允许 |

后续出现真实的团队分工需求时，再增加 `feature_reviewer`、`feature_implementer` 和 `feature_publisher` 等动作权限。不能用菜单可见性代替服务端授权。

## 6. 用户流程

### 6.1 创建需求

用户提交：

- 标题；
- 原始需求正文；
- 可选业务约束；
- 可选附件引用；
- 可选期望验收条件。

HTTP 入口负责：

1. 裁剪首尾空白；
2. 校验标题、正文和附件数量上限；
3. 规范化业务约束、附件和验收条件；
4. 创建 `FeatureRequest`；
5. 同一事务创建 `requirement` Artifact v1。

原始需求不在 `feature_requests` 中重复保存。需求修改通过创建新的 `requirement` Artifact 版本完成，旧版本永不覆盖。
创建阶段不指定目标仓库。受影响仓库以及是否需要新增服务，必须由后续 Agent 基于代码、依赖和文档证据推导，不能把用户预选仓库当成设计结论。

### 6.2 生成需求分析

需求分析至少包含：

- 背景与目标；
- 用户和使用场景；
- 功能需求；
- 非功能需求；
- 范围与非范围；
- 业务规则；
- 验收条件；
- 假设；
- 阻塞问题和非阻塞问题；
- 基于证据推导的初步影响范围，包括潜在仓库和新增服务必要性；
- 证据与推断。

需求分析引用精确的 `requirement` Artifact ID。若存在阻塞问题，该版本可以保存和展示，但不能审核通过；用户补充需求后创建新的需求版本并重新生成分析。

### 6.3 生成技术方案

技术方案只能基于当前审核通过的需求分析，至少包含：

- 当前系统事实；
- 基于证据确认的受影响服务、模块和仓库；
- 两个以上候选方案，除非只有一个方案在约束上成立；
- 方案收益、成本和风险；
- 数据、接口、兼容性和区域影响；
- 推荐方案及选择理由；
- 发布与回滚方向；
- 待确认决策。

方案不得只输出“重写”或“新增一层”一类没有成本与证据的结论。

### 6.4 生成系统设计

系统设计只能基于当前审核通过的技术方案，至少包含：

- 架构边界与依赖方向；
- 模块和职责；
- 复用现有服务或新增服务的明确决策及理由；
- 关键请求时序；
- API 契约；
- 数据模型和迁移；
- 一致性、幂等和并发；
- 权限与安全；
- 配置与 Provider 行为；
- 错误和降级；
- 可观测性；
- 测试策略；
- 发布、回滚和兼容性；
- 明确不采用的方案。

设计中的事实必须引用代码、服务、依赖或文档证据；设计决策和推断必须明确标记，不能伪装成现有实现。

### 6.5 生成实现计划

实现计划只能基于当前审核通过的系统设计，按仓库输出：

- 从已审核系统设计推导出的目标仓库；
- 预计修改的包、模块和文件；
- 新增或修改的契约；
- 数据库迁移；
- 实施顺序；
- 每一步完成条件；
- 验证命令；
- 风险检查；
- 明确不应修改的范围。

文件路径是线索，不是硬编码白名单。Coding Provider 可以发现设计遗漏的必要文件，但必须在结果中报告偏离原因。

### 6.6 实现代码

管理员选择：

- 当前有效的系统设计；
- 当前有效的实现计划；
- 实现计划中的一个目标仓库；
- 已启用列表中的显式 Coding Provider；
- 可选模型；
- 是否允许网络；
- Base Ref，默认使用当前本地默认分支引用。

Nasuta 在启动 Run 前：

1. 再次验证 Artifact 谱系仍然有效；
2. 将 Base Ref 解析为不可变 Commit SHA；
3. 创建 Run 并原子 Claim；
4. 根据 `FeatureRequest.created_by` 获取需求所有者的 User Workspace，不存在则创建，存在则校验归属后复用，并在其下创建 Run Worktree；
5. 固定 Provider、模型、执行选项和配置快照；
6. 生成只包含当前 Run 所需内容的任务包；
7. 启动 Coding Provider。

Provider 结束后，Nasuta：

1. 检查进程退出状态；
2. 检查 Worktree 边界；
3. 计算变更文件和 Diff；
4. 执行独立验证；
5. 保存 Change Set；
6. 将 Run 进入终态；
7. 等待管理员审核代码变更。

### 6.7 重新实现

代码审核拒绝后，不恢复旧 Provider 会话，也不修改旧 Run。用户提供反馈并创建一个新 Run：

- `parent_run_id` 指向被拒绝 Run；
- 使用同一套或更新后的有效 Artifact；
- 默认仍从原 Base Commit 创建新的 Run Worktree，但复用同一 User Workspace 父目录；
- 将审核反馈和上一份变更摘要加入新任务包。

新 Run 不直接继承上一 Run 的工作目录状态。上一 Run 的 Patch、日志和验证结果先进入
Artifact 目录；新 Run 在同一 User Workspace 下使用新的 Run ID 目录，以保证可重复性。

## 7. Artifact 模型

### 7.1 Artifact 类型

```text
requirement
requirement_analysis
technical_proposal
system_design
implementation_plan
```

类型是闭集。HTTP 入口将外部字符串规范化并校验，领域层不接受别名或大小写 fallback。

### 7.2 不可变性

Artifact 创建后以下字段不可修改：

- 类型；
- 版本；
- 内容；
- 结构化 JSON；
- 上游 Artifact ID；
- 证据快照；
- 创建来源；
- 内容哈希。

修订必须创建同类型新版本。审核也不能修改 Artifact 内容。

### 7.3 谱系

每个 Artifact 保存精确父级：

```text
requirement vN
  <- requirement_analysis vN
       <- technical_proposal vN
            <- system_design vN
                 <- implementation_plan vN
```

`parent_artifact_id` 足以表达当前线性流程，不引入通用 DAG。若未来一个设计需要组合多个独立方案，再基于真实需求扩展父级集合。

当前 Artifact 的选择规则：

1. `requirement`：版本号最大的版本；
2. 其他类型：父级是当前上游 Artifact，且审核通过的最高版本；
3. 一个 Artifact 即使曾审核通过，只要父级不再是当前上游，就派生为 `stale`；
4. 下游生成、审核和实现入口都重新执行该判断。

研发任务的展示阶段由该谱系推导，不在 `feature_requests` 保存 `current_stage`。

### 7.4 审核

每个非 `requirement` Artifact 最多有一条终态审核记录：

```text
undecided -> approved
undecided -> rejected
```

审核是一次性决定。需要撤销时创建新 Artifact 版本，不回写历史决定。审核通过前必须满足：

- Artifact Schema 有效；
- 没有未解决的阻塞问题；
- 父级仍是当前有效版本；
- 审核人是管理员；
- 审核意见满足长度和内容限制。

审核接口使用条件写入保证并发下只有一个终态决定成功。

## 8. Artifact 生成

### 8.1 不复用 QA HTTP

Feature Delivery 不调用 `/api/qa/ask`，也不把生成过程保存成 QA Session。二者的产物、状态和权限不同。

第一版使用独立的有界生成流水线：

```text
当前 Artifact 谱系
  -> 生成 Evidence Query Plan
  -> 并发执行有界 knowledge.API 查询
  -> 组装证据
  -> 一次结构化 LLM 生成
  -> Schema 校验
  -> 确定性 Markdown 渲染
  -> 原子保存 Artifact
```

生成请求第一版在 HTTP 请求上下文内同步执行，并受独立生成超时约束。`feature_generation_runs`
只记录 `running/succeeded/failed/interrupted` 审计结果，不引入队列、Worker、Claim、租约或
可恢复状态机。进程异常退出后，启动修复把遗留的 `running` 记录标记为 `interrupted`；
Artifact 只有在完整校验和事务提交后才可见。

### 8.2 Evidence Query Plan

查询计划是结构化结果，包含：

- 代码查询词；
- 服务查询词；
- 需要追踪依赖的服务候选；
- Runbook 查询词；
- 每类查询的数量和结果上限。

所有数量受平台硬上限约束。查询去重使用 map，调用并发受有界 semaphore 控制。服务解析完成后才能执行依赖追踪；没有服务候选时不执行全 workspace 依赖扫描。

该计划只决定要获取哪些证据，不决定业务结论。

### 8.3 Evidence Snapshot

保存到 Artifact 的证据引用使用稳定结构：

```go
type EvidenceRef struct {
	Kind      string
	Repo      string
	Path      string
	StartLine int
	EndLine   int
	Service   string
	Summary   string
	Hash      string
}
```

`Summary` 必须有长度上限；大段源码不复制到 MySQL。`Hash` 用于发现后续重新索引后的证据变化，但不把索引快照误当作 Git Commit。

代码实现仍以 Run 固定的 Base Commit 为准，不能只依赖生成设计时的索引内容。

### 8.4 结构化输出

每种 Artifact 使用独立 JSON Schema。LLM 返回严格 JSON，Nasuta 校验后再确定性渲染 Markdown。

同时保存：

- `document_json`：机器可读的规范内容；
- `rendered_markdown`：审核时看到的不可变快照；
- `content_hash`：两者规范化后的 SHA-256；
- `evidence_json`：有界证据引用。

保存 Markdown 快照是为了避免未来渲染器变化后，历史审核页面出现不同内容。

### 8.5 事实、推断与决策

结构化内容中的技术陈述必须标记：

```text
fact       有代码、配置、服务或文档证据
inference  根据现有事实推断但尚未验证
decision   本方案新作出的设计选择
unknown    缺少信息，不能下结论
```

生成提示词必须声明：需求正文、附件和检索内容都是不可信数据，不是对 Agent 的系统指令。检索内容中的“忽略规则”“执行命令”等文本不能提升权限。

## 9. 代码实现架构

### 9.1 包边界

第一版不新增 Nasuta 公共 Go API。能力由平台发行版直接拥有：

```text
app/
  platform.go                     组装、能力状态、路由注册和关闭

internal/featuredelivery/
  model.go                        领域类型与不变量
  service.go                      用例编排
  lineage.go                      当前谱系推导
  generation.go                   Artifact 生成
  review.go                       Artifact 和 Change Set 审核
  implementation.go               Run 编排
  repository.go                   Store 接口
  events.go                       运行事件契约

internal/codingagent/
  runner.go                       统一请求和结果
  dispatch.go                     显式 Provider 分发
  codex.go                        Codex CLI 适配
  claude.go                       Claude Code CLI 适配
  process.go                      子进程、取消、输出边界和脱敏

internal/platform/store/
  feature_delivery.go             MySQL Store 实现

internal/transport/featurehttp/
  handler.go                      REST/SSE 和认证用户适配
```

未来只有下游应用确实需要在 Go 代码中嵌入、替换或扩展该能力时，才评估公共包。不能为了“可能复用”提前扩大稳定 API。

### 9.2 依赖方向

```text
app
  -> featurehttp
      -> featuredelivery
          -> knowledge.API
          -> internal llm client
          -> Store interface
          -> CodingRunner interface

platform/store
  -> featuredelivery domain types

codingagent
  -> featuredelivery run request/event contracts
```

`featuredelivery.Service` 保存自己真正需要的依赖，不保存通用 `Deps` 容器。Transport 只负责输入规范化、认证身份、HTTP 状态和序列化。

### 9.3 Coding Runner

领域侧只需要一个最小接口：

```go
type CodingRunner interface {
	Run(context.Context, CodingRequest, EventSink) (CodingResult, error)
}
```

取消通过 `context.Context` 完成，不增加第二套 Cancel 通道。第一版不暴露 Provider Session Resume，因此接口不包含无法统一保证的恢复方法。

`CodingRequest` 包含：

- Run ID；
- Provider 和可选模型；
- Worktree 路径；
- Base Commit；
- 任务包；
- 网络策略；
- 执行超时；
- Provider 配置快照。

`CodingResult` 包含：

- Provider Session ID，若 Provider 提供；
- 退出码；
- 最终摘要；
- Provider 报告的测试摘要；
- token 或费用数据，若 Provider 提供；
- 规范化事件统计。

### 9.4 显式 Provider 分发

```text
codex  -> runCodex
claude -> runClaude
其他   -> unsupported coding provider
```

不使用动态插件发现，不接受前端传入可执行文件路径，也不在一个 Provider 失败后调用另一个 Provider。

### 9.5 Codex

Codex 第一版通过非交互 CLI 接入：

```text
codex exec --ephemeral --ignore-user-config --ignore-rules
  --json --sandbox workspace-write --output-schema <schema> -
```

适配器负责：

- 使用参数数组启动进程，不经过 shell；
- 将工作目录固定为 Run Worktree；
- 从标准输入传入任务包，不把需求正文拼接进命令行；
- 请求结构化事件输出；
- 使用原生 `--output-schema` 将最终结果约束为 Nasuta 定义的 JSON Schema；
- 使用临时 `CODEX_HOME` 和 ephemeral session，禁止读取宿主用户配置和执行策略；
- 只开放 Worktree 写入权限；
- 根据 Run 的 `network_enabled` 显式设置 `sandbox_workspace_write.network_access`；
- 通过受控配置把模型执行命令的环境变量继承限制为最小集合；
- 解析 JSONL，拒绝超过单事件上限的输出；
- 将 Provider Session ID 作为诊断信息保存，但第一版不依赖它恢复。

Codex App Server 或 SDK 只有在 CLI 无法满足动态审批、细粒度工具回调或高并发进程管理时再评估。

### 9.6 Claude Code

Claude Code 第一版通过 Headless CLI 接入：

```text
claude -p --output-format stream-json --no-session-persistence ...
```

适配器负责：

- 使用参数数组启动进程，不经过 shell；
- 将工作目录固定为 Run Worktree；
- 请求流式 JSON；
- 使用 Nasuta 生成的临时设置文件，排除宿主用户和本地设置；
- 显式限制内置工具、禁用未声明 MCP，并配置权限模式；
- 禁止命令脱离 Sandbox 重试，并用严格域名白名单执行 Run 的网络开关；
- Provider 版本支持时使用原生 `--json-schema` 校验最终结果；
- 解析流式事件并映射到 Nasuta 事件；
- 保存 Provider Session ID 仅用于诊断；
- 禁止使用跳过全部权限检查的危险模式。

Claude Agent SDK 当前提供 TypeScript/Python 集成。Nasuta 是 Go 服务，第一版不为了 SDK
增加 Sidecar；只有 CLI 生命周期或协议不足时再引入。

两个适配器都在能力探测时执行 `--version`，保存 Run 使用的 CLI 版本，并通过 Provider
Contract Test 验证当前版本的事件协议和安全参数。CLI 参数属于 Adapter 细节，不能散落到
Feature Delivery 领域服务。

Claude Code 最低兼容版本为 `2.1.219`，该版本开始支持 `sandbox.network.strictAllowlist`。
网络关闭时白名单为空；显式开启时使用全域名白名单。无论网络是否开启，
`allowUnsandboxedCommands` 都固定为 `false`，避免命令绕过 Sandbox。

### 9.7 Provider 凭据

Coding Provider 凭据通过进程环境或部署密钥挂载注入：

```text
CODEX_API_KEY
ANTHROPIC_API_KEY
```

Nasuta 不把个人订阅 OAuth 凭据代理给其他用户，不把凭据写入 Artifact、Run 事件、日志、任务包或前端响应。

子进程环境使用 allowlist 构造，只继承运行所需的基础变量和当前 Provider 凭据。不能直接继承 Nasuta 服务进程的完整环境。
Provider 凭据只对 CLI 主进程短时可见；Provider 启动的仓库命令必须通过 Provider
支持的环境过滤机制移除该凭据。无法证明凭据不会被仓库命令继承时，Coding Capability
保持禁用，而不是依赖提示词保密。生产部署优先使用短时、限额、可撤销的机器凭据。

## 10. Worktree 与执行边界

### 10.1 Base Commit

Run 创建时将用户选择的 Base Ref 解析为完整 Commit SHA，并保存到数据库。后续所有操作使用 SHA，不重新解释分支名。

默认不执行网络 Fetch。若管理员显式要求更新远端引用，Fetch 必须作为独立、可观察的前置操作，并受网络配置约束。

### 10.2 Worktree

目录结构：

```text
<coding-work-root>/worktrees/<username>-workspace/<run-id>
```

部署根目录为 `/coding-work` 时，示例为：

```text
/coding-work/worktrees/username-workspace/run-123
```

`<username>` 是 Nasuta 在可信身份边界生成并持久化的目录 Key，不是客户端传入的路径片段。
目录归属用户始终是 `FeatureRequest.created_by`；管理员启动 Run 时，
`feature_implementation_runs.requested_by` 只记录操作者，不改变目录归属。

User Workspace 初始化规则：

1. 从需求所有者的认证身份读取用户名；当前展示名为空时使用邮箱本地部分，仍为空时使用 `user`；
2. 在入口执行 Unicode NFKC、大小写折叠和首尾空白清理；
3. 在字符替换前拒绝 `/`、`\`、NUL 和其他控制字符，不能把路径攻击清理成一个看似合法的名字；
4. 仅保留 Unicode 字母、数字、`.`、`_`、`-`，其他连续字符折叠为单个 `-`；
5. 去除首尾的 `.`、`_`、`-`；空结果回退为 `user`，并拒绝 `.`、`..` 或超过 96 UTF-8 字节的结果；
6. 首次使用时将目录 Key 与稳定 `user_id` 写入 `feature_user_workspaces`；用户名之后变化不自动移动目录；
7. 若规范用户名已被其他 `user_id` 占用，将基础名按 UTF-8 边界缩短后追加 `-u<base36-user-id>`，使最终 Key 仍不超过 96 字节，再重试唯一键写入；该候选仍冲突时按数据一致性错误失败；
8. 创建 `<username>-workspace` 时写入只允许 Nasuta 管理的 `.nasuta-owner.json`，至少包含格式版本、`user_id` 和目录 Key；
9. 目录已存在时必须同时匹配数据库映射和 owner 文件；缺失、损坏或归属不一致时安全失败，禁止接管或复用。

这里的 `<username>` 指最终持久化的目录 Key，因此冲突用户的目录仍符合
`<username>-workspace` 结构。映射一旦建立便以稳定用户 ID 判断所有权，不能依赖可能改名
或重名的展示名。

创建流程：

1. 将仓库标识解析为 workspace 内已索引 Git 仓库；
2. 获取真实路径并确认仍位于 Workspace Root；
3. 从 Request 所有者解析或创建 User Workspace，并把 `workspace_user_id` 和 `workspace_username` 固定到 Run；
4. 由服务端生成并校验 Run ID，拼出尚不存在的 `<username>-workspace/<run-id>`；
5. 拒绝根目录、User Workspace 或任一已有路径组件的符号链接逃逸；
6. 使用 `git worktree add --detach <run-path> <base-commit>`；
7. 验证 Worktree HEAD 等于固定 Base Commit；
8. 才允许启动 Provider。

不在原工作目录 Checkout 分支，因此不要求原工作目录干净，也不影响开发者当前分支。
User Workspace 只是可复用父目录，不是 Git Worktree；真正被 Provider 修改和被 TTL 清理的是
其下的单 Run 目录。同一用户的不同 Run 使用不同子目录，不共享未提交文件。

### 10.3 任务包

任务包包含：

- 原始需求当前版本；
- 已审核需求分析；
- 已审核技术方案；
- 已审核系统设计；
- 已审核实现计划；
- 当前仓库对应的任务切片；
- 证据引用；
- Base Commit；
- 仓库中的 `AGENTS.md`、`CLAUDE.md` 或同类规范位置；
- 允许和禁止的操作；
- 期望结果 Schema。

任务包只包含本 Run 需要的内容，不能加入整个 QA 会话、其他用户记忆或无关 Provider 凭据。

任务包优先通过标准输入传递。若 Provider 必须读取文件，则文件放在执行环境的只读任务目录，不作为仓库变更。不能把任务文件留在最终 Diff 中。

### 10.4 网络

默认 `network_enabled=false`。

允许网络是 Run 的持久化配置，只有管理员可以开启。开启后仍不能把 Nasuta 的 VCS Token、MySQL DSN、Webhook Secret 或其他平台凭据传入 Provider。

依赖下载、远端文档读取和外部 API 调用都属于网络行为。Provider 不能自行把网络失败解释为允许切换镜像或服务商。

### 10.5 本地执行的安全定位

CLI 自带 sandbox 是第一层约束，不是 Nasuta 的多租户安全边界。

第一版适合可信内部团队部署，至少使用：

- 独立系统用户；
- 独立 Worktree；
- 精简环境变量；
- workspace 写入限制；
- 进程超时；
- 子进程组终止；
- 文件和输出大小上限。

面向不可信租户前，必须增加容器或虚拟机隔离。没有这个隔离时，产品文案不能声称提供强多租户代码执行安全。

## 11. 独立验证

Provider 可以自行运行测试，但 Nasuta 必须再次执行平台信任的验证命令。

### 11.1 验证配置

每个仓库可以在 Base Commit 中提供：

```text
.nasuta/delivery.json
```

示例：

```json
{
  "validation": [
    {"argv": ["go", "build", "./..."], "timeout": "5m"},
    {"argv": ["go", "test", "./..."], "timeout": "10m"},
    {"argv": ["go", "vet", "./..."], "timeout": "5m"}
  ]
}
```

规则：

1. 配置从 Base Commit 读取并在 Run 开始时固定；
2. Provider 对该文件的修改不能改变当前 Run 的验证命令；
3. 命令使用 argv 数组，不通过 shell；
4. 每条命令有独立超时和输出上限；
5. 命令数量、参数数量和参数长度有硬上限；
6. 缺少配置时记录 `validation_not_configured`，不能伪装成验证成功；
7. 配置错误直接导致 Run 验证失败。

第一版不根据语言猜测命令，也不从自然语言 `AGENTS.md` 自动提取 shell 命令。确定性仓库配置优先于猜测式 fallback。

验证命令运行在与 Provider 相同的执行隔离边界中，但使用不含 Provider 和平台凭据的独立
最小环境。网络策略不能比 Run 更宽。若本地主机模式无法强制阻断网络，Capability Status
必须明确报告弱隔离，生产环境需使用阶段 4 的容器或 VM Executor。

### 11.2 验证结果

每条验证保存：

- 序号；
- argv；
- 状态；
- 退出码；
- 耗时；
- 有界输出摘要；
- 完整输出文件的相对路径和哈希；
- 是否超时。

完整输出保存在 Run Artifact 目录，不写入 MySQL 大字段。列表接口只返回摘要，详情按需读取有界内容。

### 11.3 Run 成功语义

Run 的 `succeeded` 只表示：

- Coding Provider 正常结束；
- Worktree 和 Diff 检查通过；
- 已配置的所有独立验证成功；
- Change Set 和终态已持久化。

它不表示代码已经人工审核、提交、推送、合并或发布。

## 12. Change Set

Change Set 包含：

- Base Commit；
- Worktree HEAD；
- 修改、删除、新增文件列表；
- additions/deletions；
- 二进制文件列表；
- Patch 文件；
- Patch SHA-256；
- Provider 最终摘要；
- 独立验证汇总；
- 与实现计划的偏离说明。

Patch 使用 Git 生成并设置总大小上限。超过上限时：

1. Run 进入失败终态；
2. Worktree 暂时保留；
3. 返回明确的文件数量和大小错误；
4. 不在 MySQL 保存截断后可能无法应用的伪 Patch。

变更集不能只使用默认 `git diff`，因为它会漏掉未跟踪文件。Nasuta 使用独立临时 Git
Index：从 Base Commit 执行 `read-tree`，把当前 Worktree 执行 `add -A` 到临时 Index，
再生成相对 Base Commit 的 `--cached --binary` Patch。该过程不修改 Worktree 自身 Index，
并同时覆盖已提交、已暂存、未暂存和未跟踪变更。第一版拒绝包含嵌套 Git 仓库或发生内容
变更的 Submodule，避免生成不能独立应用的伪 Patch。

代码审核同样是一次性记录：

```text
undecided -> approved
undecided -> rejected
```

代码审核通过不自动执行 Git 远端写入。后续发布能力必须另行设计审批、幂等和 VCS 错误恢复。

## 13. Implementation Run 状态机

### 13.1 为什么需要状态机

代码执行不是可以即时推导的标签。它涉及：

- 数据库 Claim；
- 长时间外部进程；
- 并发限制；
- 用户取消；
- 服务重启；
- Worktree 清理；
- Provider 和验证的不同失败位置；
- SSE 重连后的终态恢复。

因此 Run 使用持久状态机：

```text
queued
  -> preparing
       -> running
            -> validating
                 -> succeeded

queued/preparing/running/validating
  -> cancelled
  -> failed
  -> interrupted
```

### 13.2 状态定义

| 状态 | 含义 |
| --- | --- |
| `queued` | Run 已创建，等待并发槽位 |
| `preparing` | 正在验证已固定 Commit、创建 Worktree 和任务包 |
| `running` | Coding Provider 进程正在执行 |
| `validating` | Provider 已结束，正在生成 Diff 并独立验证 |
| `succeeded` | Change Set 和验证结果已完整保存 |
| `failed` | 确定性失败，错误已保存 |
| `cancelled` | 管理员取消，进程和子进程已终止 |
| `interrupted` | 服务退出或租约过期，无法证明进程仍受当前实例控制 |

### 13.3 允许转换

```text
queued      -> preparing | cancelled
preparing   -> running | failed | cancelled | interrupted
running     -> validating | failed | cancelled | interrupted
validating  -> succeeded | failed | cancelled | interrupted
```

终态不可重新进入活动状态。重试创建新 Run。

### 13.4 Claim 与租约

Worker 使用条件更新 Claim：

```sql
UPDATE feature_implementation_runs
SET status='preparing', worker_id=?, lease_expires_at=?,
    started_at=COALESCE(started_at, CURRENT_TIMESTAMP)
WHERE id=? AND status='queued';
```

只有影响一行的 Worker 可以执行。

每个进程启动使用新的 `worker_id`，活动 Run 定期续租。实例启动和后台恢复只处理租约已经
过期的活动 Run：

- `queued` 保持等待；
- 租约过期的 `preparing/running/validating` Run 条件更新为 `interrupted`；
- 租约尚未过期且属于其他实例的 Run 不得修改；
- 不自动重新启动 Provider；
- 保留 Worktree 和现有事件供人工诊断。

不能在服务重启后假设第三方 CLI 可以安全续跑。

### 13.5 取消

取消接口只接受活动状态。`queued` Run 直接条件更新为 `cancelled`；已经被 Claim 的 Run
先条件写入 `cancel_requested_at`，再通知持有该 Run 的 Worker。Worker 在续租时也检查
取消意图，避免跨实例通知丢失。成功写入运行中 Run 的取消意图后：

1. 取消 Run Context；
2. 终止 Provider 进程组；
3. 等待有限宽限期；
4. 必要时强制终止；
5. 保存最后事件；
6. 条件更新为 `cancelled`。

如果 Run 已进入终态，取消返回冲突而不是覆盖结果。

## 14. 并发与资源控制

### 14.1 全局并发

`coding_max_concurrency` 限制当前实例同时运行的 Coding Provider 数量。并发槽位使用固定容量 semaphore，不为每个等待 Run 创建无界 goroutine。

第一版部署模式固定为单实例，启动日志显式输出 `deployment=single_instance`。多实例部署时，仅本地 semaphore 不足；支持多实例前必须增加数据库级全局活动 Run 配额 Claim，不能只复制当前进程。

### 14.2 仓库并发

不同 Worktree 可以并行修改同一仓库，因此不需要为了减少实现复杂度而全仓库串行化。
同一用户的不同 Run 也不串行化；User Workspace 父目录复用不代表共享 Run 状态。

但以下操作需要短临界区：

- 同一用户首次创建 User Workspace 和 owner 文件；
- `git worktree add/remove`；
- 清理共享 Worktree 元数据；
- 同一 Run 的终态保存。

进程运行期间不持有仓库全局锁。

### 14.3 输出上限

必须配置：

- 单 Provider 事件最大字节数；
- 单 Run 事件数量；
- stdout/stderr 总字节数；
- 单验证输出字节数；
- Patch 总字节数；
- 修改文件数量；
- Run 总超时；
- Worktree 保留时间。

达到上限时明确失败。不能无限缓冲后再截断。

## 15. 持久化设计

新增 MySQL Schema Group：

```go
GroupFeatureDelivery MySQLGroup = "feature_delivery"
```

该 Group 加入 `AllGroups()`。已有安装的列修改使用 `docs/sql` 显式迁移；新表继续由启动迁移创建。

### 15.1 `feature_user_workspaces`

```sql
CREATE TABLE feature_user_workspaces (
    user_id           BIGINT PRIMARY KEY,
    username_key      VARCHAR(128) NOT NULL,
    username_snapshot VARCHAR(128) NOT NULL,
    created_at        TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE KEY uniq_workspace_username (username_key)
);
```

该表是用户到可读目录 Key 的稳定映射。`username_snapshot` 只用于审计首次分配时的身份输入，
路径始终由 `username_key` 推导，不保存客户端路径。并发首次创建依赖主键和唯一键裁决；
冲突重试必须重新读取已提交映射，不能在内存中覆盖其他实例的结果。

### 15.2 `feature_requests`

```sql
CREATE TABLE feature_requests (
    id          VARCHAR(64) PRIMARY KEY,
    title       VARCHAR(512) NOT NULL,
    created_by  BIGINT NOT NULL,
    archived_at TIMESTAMP NULL,
    created_at  TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at  TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
                ON UPDATE CURRENT_TIMESTAMP,
    KEY idx_owner_updated (created_by, updated_at, id),
    KEY idx_updated (updated_at, id)
);
```

创建 Artifact、Review、Generation Run 或 Implementation Run 的事务必须同时显式
更新所属 Request 的 `updated_at`，不能只依赖 Request 行自身的 `ON UPDATE`。

### 15.3 `feature_artifacts`

```sql
CREATE TABLE feature_artifacts (
    id                VARCHAR(64) PRIMARY KEY,
    request_id        VARCHAR(64) NOT NULL,
    kind              VARCHAR(32) NOT NULL,
    version           INT NOT NULL,
    parent_artifact_id VARCHAR(64) NOT NULL DEFAULT '',
    origin            VARCHAR(16) NOT NULL,
    document_json     JSON NOT NULL,
    rendered_markdown MEDIUMTEXT NOT NULL,
    evidence_json     JSON NOT NULL,
    content_hash      CHAR(64) NOT NULL,
    created_by        BIGINT NOT NULL,
    created_at        TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE KEY uniq_request_kind_version (request_id, kind, version),
    KEY idx_request_kind_parent_version (request_id, kind, parent_artifact_id, version),
    KEY idx_request_created (request_id, created_at, id),
    KEY idx_parent (parent_artifact_id)
);
```

版本号在事务中按 `(request_id, kind)` 分配。不能先查询最大值后无保护插入；依赖唯一键冲突重试或显式锁定对应 Request 行。

### 15.4 `feature_artifact_reviews`

```sql
CREATE TABLE feature_artifact_reviews (
    artifact_id VARCHAR(64) PRIMARY KEY,
    decision    VARCHAR(16) NOT NULL,
    comment     TEXT NOT NULL,
    reviewer    BIGINT NOT NULL,
    created_at  TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    KEY idx_reviewer_created (reviewer, created_at)
);
```

主键保证一个 Artifact 只有一个终态审核。

### 15.5 `feature_generation_runs`

```sql
CREATE TABLE feature_generation_runs (
    id             VARCHAR(64) PRIMARY KEY,
    request_id     VARCHAR(64) NOT NULL,
    artifact_kind  VARCHAR(32) NOT NULL,
    parent_artifact_id VARCHAR(64) NOT NULL,
    status         VARCHAR(16) NOT NULL,
    provider       VARCHAR(32) NOT NULL,
    model          VARCHAR(128) NOT NULL,
    requested_by   BIGINT NOT NULL,
    input_tokens   BIGINT NOT NULL DEFAULT 0,
    output_tokens  BIGINT NOT NULL DEFAULT 0,
    error_summary  VARCHAR(2048) NOT NULL DEFAULT '',
    started_at     TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    ended_at       TIMESTAMP NULL,
    KEY idx_request_started (request_id, started_at, id),
    KEY idx_status_started (status, started_at, id)
);
```

Generation Run 只记录生成审计和用量，不保存完整模型推理。

### 15.6 `feature_implementation_runs`

```sql
CREATE TABLE feature_implementation_runs (
    id                     VARCHAR(64) PRIMARY KEY,
    request_id             VARCHAR(64) NOT NULL,
    client_request_id      VARCHAR(128) NOT NULL,
    request_hash           CHAR(64) NOT NULL,
    design_artifact_id     VARCHAR(64) NOT NULL,
    plan_artifact_id       VARCHAR(64) NOT NULL,
    parent_run_id          VARCHAR(64) NOT NULL DEFAULT '',
    repo                   VARCHAR(512) NOT NULL,
    base_ref               VARCHAR(255) NOT NULL,
    base_commit            VARCHAR(64) NOT NULL,
    workspace_user_id      BIGINT NOT NULL,
    workspace_username     VARCHAR(128) NOT NULL,
    provider               VARCHAR(32) NOT NULL,
    model                  VARCHAR(128) NOT NULL DEFAULT '',
    provider_version       VARCHAR(64) NOT NULL DEFAULT '',
    network_enabled        TINYINT(1) NOT NULL DEFAULT 0,
    status                 VARCHAR(16) NOT NULL,
    worker_id              VARCHAR(128) NOT NULL DEFAULT '',
    lease_expires_at       TIMESTAMP NULL,
    cancel_requested_at    TIMESTAMP NULL,
    provider_session_id    VARCHAR(255) NOT NULL DEFAULT '',
    exit_code              INT NULL,
    error_summary          VARCHAR(2048) NOT NULL DEFAULT '',
    requested_by           BIGINT NOT NULL,
    started_at             TIMESTAMP NULL,
    ended_at               TIMESTAMP NULL,
    retain_until           TIMESTAMP NULL,
    worktree_cleaned_at    TIMESTAMP NULL,
    cleanup_error          VARCHAR(2048) NOT NULL DEFAULT '',
    created_at             TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE KEY uniq_requester_client_request (requested_by, client_request_id),
    KEY idx_request_created (request_id, created_at, id),
    KEY idx_status_created (status, created_at, id),
    KEY idx_lease (status, lease_expires_at)
);
```

`base_commit` 接受规范化的 40 或 64 位十六进制 Object ID。Git 能力探测必须验证仓库
Object Format，不能截断或接受其他长度。
`workspace_user_id` 必须等于 Request 的 `created_by`，`workspace_username` 是创建 Run 时
固定的目录 Key。清理器因此不需要根据可能变化的展示名重新计算路径。

### 15.7 `feature_run_events`

```sql
CREATE TABLE feature_run_events (
    run_id      VARCHAR(64) NOT NULL,
    seq         BIGINT NOT NULL,
    kind        VARCHAR(32) NOT NULL,
    summary     TEXT NOT NULL,
    detail_json JSON NULL,
    created_at  TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (run_id, seq)
);
```

事件使用 Run 内单调序号。写入批量化，但终态前必须 Flush。事件读取使用 `seq > after_seq ORDER BY seq LIMIT ?`。

### 15.8 `feature_change_sets`

```sql
CREATE TABLE feature_change_sets (
    run_id             VARCHAR(64) PRIMARY KEY,
    worktree_head      VARCHAR(64) NOT NULL,
    patch_rel_path     VARCHAR(1024) NOT NULL,
    patch_sha256       CHAR(64) NOT NULL,
    patch_bytes        BIGINT NOT NULL,
    files_changed      INT NOT NULL,
    additions          INT NOT NULL,
    deletions          INT NOT NULL,
    files_json         JSON NOT NULL,
    plan_deviations_json JSON NOT NULL,
    validation_results_json JSON NOT NULL,
    provider_summary   TEXT NOT NULL,
    created_at         TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);
```

Patch 和完整验证输出保存在：

```text
<coding-work-root>/artifacts/<run-id>/
```

数据库只保存相对路径、哈希、大小和摘要。

### 15.9 `feature_change_reviews`

```sql
CREATE TABLE feature_change_reviews (
    run_id     VARCHAR(64) PRIMARY KEY,
    decision   VARCHAR(16) NOT NULL,
    comment    TEXT NOT NULL,
    reviewer   BIGINT NOT NULL,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    KEY idx_reviewer_created (reviewer, created_at)
);
```

## 16. 存储读取

所有在线读取必须有界：

- 研发任务列表使用 `(updated_at, id)` keyset cursor；
- Artifact 列表使用 `(kind, version)` keyset cursor；
- Generation Run 使用 `(started_at, id)` cursor，Implementation Run 使用 `(created_at, id)` cursor；
- 事件使用 `seq > after_seq LIMIT page_size`；
- 验证命令元数据作为有界 JSON 随 Change Set 读取，完整输出按一条命令流式读取；
- Patch 使用流式下载和响应大小保护；
- 列表接口不读取 `rendered_markdown`、`document_json`、Patch 或完整日志。

任务详情可以批量查询当前谱系所需的固定少量 Artifact 和 Review，不能为每个 Artifact 发 N 次数据库查询。

## 17. REST API

所有路由由 `internal/transport/featurehttp` 注册到 `platform.AuthenticatedAPI`。

### 17.1 Feature Request

```text
POST   /api/features
GET    /api/features?cursor=&limit=
GET    /api/features/{id}
POST   /api/features/{id}/requirements
POST   /api/features/{id}/archive
```

归档是幂等的只读边界。归档后仍可查看已有 Artifact、Generation、Run、Patch 和验证输出，
但创建需求/Artifact 新版本、启动 Generation 或 Implementation 返回 `409`；已经启动的 Run
继续按固定输入完成，不因归档被隐式取消。

`POST /api/features` 在一个事务内创建 Request 和 requirement v1。

### 17.2 Artifact

```text
GET    /api/features/{id}/artifacts?cursor=&limit=
GET    /api/features/{id}/artifacts/{artifact_id}
POST   /api/features/{id}/artifacts/{kind}/generate
POST   /api/features/{id}/artifacts/{kind}
POST   /api/features/{id}/artifacts/{artifact_id}/review
```

手工创建 Artifact 用于人工修订，仍必须通过同一 JSON Schema 和谱系校验。
`requirement` 只能通过专用需求版本接口创建，通用 Artifact 创建接口拒绝该 Kind。

第一版生成请求同步返回 Artifact 和 Generation Run 摘要，不提供 Generation SSE。
Artifact 插入与 Generation Run 成功状态在同一事务中提交。调用方未得到确定结果时应先按
Request 重新读取 Generation Run 和 Artifact 列表，不能直接假定生成失败。

### 17.3 Generation 审计

```text
GET    /api/features/{id}/generations?cursor=&limit=
GET    /api/feature-generations/{run_id}
```

### 17.4 Implementation

```text
POST   /api/features/{id}/implementations
GET    /api/features/{id}/implementations?cursor=&limit=
GET    /api/feature-implementations/{run_id}
POST   /api/feature-implementations/{run_id}/cancel
GET    /api/feature-implementations/{run_id}/events?after_seq=
GET    /api/feature-implementations/{run_id}/patch
GET    /api/feature-implementations/{run_id}/validations/{sequence}/output
POST   /api/feature-implementations/{run_id}/review
```

### 17.5 幂等

Implementation 创建必须携带稳定 `client_request_id`。表中保存
`requested_by + client_request_id + request_hash` 并建立唯一键：同一用户和 Key 的同一
规范请求返回原 Run，不同请求返回 `409`，避免网络重试启动两次代码执行。

Feature 和 Generation 第一版不承诺通用 `Idempotency-Key` 语义。需要覆盖所有写接口时，
再独立设计带过期、请求哈希和响应恢复的通用幂等存储，不能只缓存不完整 HTTP 响应。

### 17.6 HTTP 状态

| 场景 | 状态 |
| --- | --- |
| 输入非法 | `400` |
| 未认证 | `401` |
| 无权限 | `403` |
| 资源不存在或不属于用户 | `404` |
| Artifact 谱系过期、重复审核、Run 已终态 | `409` |
| LLM、MySQL、Git 或 Coding Provider 能力不可用 | `503` |
| 外部 Provider 运行失败 | `502` 或 Run `failed` 终态 |

异步 Run 已成功创建后，后续失败通过 Run 终态和 SSE 表达，不把原始 POST 保持到 Provider 完成。

### 17.7 Web 工作台

下游 Web 只实现 Feature Delivery 的展示和输入适配，不拥有业务状态或流程规则。工作台覆盖：

- Feature 创建、归档和需求新版本；
- 五阶段当前谱系、Artifact 版本、人工修订、审核和有界证据引用；
- Generation Run 审计；
- Implementation 创建、取消、事件、Change Set、验证输出、审核和被拒绝 Run 的重新实施；
- Coding Provider 实际能力状态与平台默认 Provider。

Coding 与 Delivery 设置保存后需要重启服务生效。当前 Worker 生命周期由平台进程拥有，不能在只热重载 QA 的设置回调中遗留旧 Worker 后再启动新 Worker。

## 18. SSE 与事件

Feature Delivery 使用独立 Hub，不复用 QA `RunHub`。QA 事件包含 token、reasoning、tool step 等对话语义；Coding Run 事件需要进程、文件、验证和租约语义。只有出现足够重复后才抽取通用 Hub。

Implementation 事件闭集：

```text
run_queued
run_preparing
provider_started
provider_message
command_started
command_finished
file_changed
provider_finished
validation_started
validation_finished
change_set_ready
run_failed
run_cancelled
run_interrupted
run_succeeded
```

规则：

1. Provider 原始事件先转换成平台事件；
2. 不持久化隐藏推理；
3. 命令参数、输出和文件路径经过凭据与敏感内容脱敏；
4. 高频文本合并成有界批次；
5. 重要状态事件先持久化再广播；
6. SSE 支持 `Last-Event-ID` 或 `after_seq` 重放；
7. 重放先从 MySQL 有界读取，再订阅 Live Hub，避免连接窗口丢事件；
8. 慢订阅者不能阻塞 Provider 进程读取，非关键进度事件允许合并，终态事件必须可恢复查询。

## 19. 配置

### 19.1 平台设置

新增平台设置：

```text
coding_enabled_providers       codex | claude | codex,claude
coding_default_provider        "" | codex | claude
coding_codex_model
coding_claude_model
feature_generation_timeout
coding_timeout
coding_max_concurrency
coding_allow_network
coding_worktree_ttl
```

这些字段加入 `PlatformSettings`、设置 Key 表、`Values`、`Apply` 和
`CanonicalPlatformSetting`。入口一次性完成 trim、去重、枚举、范围、duration 和默认值
校验，下游代码直接信任规范值。Provider 列表规范化后顺序固定，成员检查使用 map。

建议默认：

```text
coding_enabled_providers       ""
coding_default_provider        ""
coding_codex_model             ""
coding_claude_model            ""
feature_generation_timeout     5m
coding_timeout                 30m
coding_max_concurrency         1
coding_allow_network           false
coding_worktree_ttl            72h
```

空 Provider 列表表示代码实现能力未配置，不表示自动选择 Codex。Run 请求必须显式提交
Provider；`coding_default_provider` 只用于前端默认选中，并在入口解析后保存为非空规范值。
默认 Provider 必须为空或属于已启用集合；Run 未提交模型时，按所选 Provider 解析对应默认
模型。
`coding_allow_network=false` 是平台上限，Run 不能自行开启网络。

### 19.2 部署配置

可执行文件路径和 Artifact 根目录属于部署边界：

```text
NASUTA_CODEX_BIN
NASUTA_CLAUDE_BIN
NASUTA_CODING_WORK_ROOT=/coding-work
```

默认 Binary Name 分别为 `codex` 和 `claude`，由 `exec.LookPath` 解析。Coding Work Root
默认为 `/coding-work`，因此 Run 路径默认为
`/coding-work/worktrees/<username>-workspace/<run-id>`。数据库设置不能指定任意可执行文件路径。

### 19.3 Capability Status

系统状态接口增加：

```json
{
  "feature_delivery": {
    "persistence": {"enabled": true},
    "generation": {"enabled": true},
    "coding": {
      "enabled": true,
      "git_found": true,
      "isolation": "local_process",
      "providers": {
        "codex": {
          "enabled": true,
          "binary_found": true,
          "binary_version": "<detected-version>",
          "contract_compatible": true,
          "credential_isolated": true
        },
        "claude": {
          "enabled": false,
          "reason": "not_configured"
        }
      }
    }
  }
}
```

状态检查不执行付费模型请求，只检查规范配置、Binary、静态协议契约、凭据隔离能力和必要目录。

## 20. 能力降级与错误

| 缺失或失败 | 行为 |
| --- | --- |
| MySQL 未配置 | Feature Delivery 整体禁用并记录 Warn |
| 主 LLM 未配置 | 可以查看已有任务和 Artifact；生成端点返回 `503` |
| Knowledge 语义后端缺失 | 使用当前 Knowledge 已支持的确定性降级，并在证据中标记能力 |
| Git 不存在 | 分析与设计可用；Implementation 返回 `503` |
| Coding Provider 列表为空 | 分析与设计可用；Implementation 返回 `503` |
| 请求的 Provider 未启用 | Run 不创建，返回明确错误 |
| Provider Binary 不存在 | 对应 Provider 不可用，其他 Provider 状态不受影响 |
| Provider 凭据缺失 | Run 不启动，明确失败 |
| 配置 Provider 运行失败 | Run 失败，绝不调用另一 Provider |
| 验证配置缺失 | 记录未配置，不声称验证成功 |
| Worktree 清理失败 | Run 结果不回滚；记录清理错误并交给后台清理 |

配置存在但初始化失败必须可观察。只有能力完全没有配置时，才按“未启用”处理。

## 21. 安全

### 21.1 路径

- 仓库由 Nasuta 已知仓库标识解析，客户端不能提交任意绝对路径；
- User Workspace、Run Worktree 和 Artifact 路径全部由服务端生成；
- 用户名只在可信身份入口规范化一次，下游使用持久化 `username_key`，不接受客户端覆盖；
- `coding-work-root` 在启动时解析为真实绝对路径；已有路径组件使用 `Lstat` 拒绝符号链接；
- 创建或复用目录后再次执行 Root 包含、owner 文件和稳定 `user_id` 校验；
- Run ID 只接受服务端生成的受限字符集，任何 `/`、`\`、`.`、`..` 或控制字符都非法；
- Patch 下载只接受 Run ID，不接受文件路径；
- Provider 修改 workspace 外文件视为安全失败。

### 21.2 凭据

- Provider 子进程环境使用 allowlist；
- 日志和事件在持久化前脱敏；
- 不把 MySQL DSN、VCS Token、LLM Key、Session Cookie 注入任务包；
- 前端设置接口继续按现有敏感字段策略处理；
- 完整 stdout/stderr 文件也必须脱敏或受管理员访问控制。

### 21.3 命令

- Nasuta 自己执行的 Git 和验证命令必须使用 argv，不经过 shell；
- Provider 内部命令由其 sandbox 约束；
- 默认禁止网络；
- 禁止使用 Provider 的全权限跳过模式；
- 进程取消必须覆盖子进程组，不能只结束父 CLI。

### 21.4 内容注入

需求、代码注释、文档和 Runbook 都可能包含提示注入。系统提示必须规定：

- 检索内容只是证据；
- 只有 Nasuta 任务包顶部策略和仓库规范可以约束执行；
- 任何要求读取凭据、扩大权限、关闭 sandbox 或访问 workspace 外路径的内容都无效；
- Provider 遇到冲突必须报告，不能自行放宽权限。

## 22. 可观测性与审计

### 22.1 日志

统一字段：

```text
feature_id
artifact_id
generation_run_id
implementation_run_id
provider
model
repo
base_commit
worker_id
status
duration_ms
```

日志只保存摘要，不打印完整需求、源码、Patch、模型输出或凭据。

### 22.2 指标

建议指标：

- Artifact 生成成功率和耗时；
- 各类型 Artifact token 用量；
- 审核通过率和拒绝后重生成次数；
- 各 Provider Run 数、成功率、取消率和中断率；
- 排队时间、执行时间和验证时间；
- 修改文件数和 Patch 大小分布；
- 各验证命令失败率；
- Provider 事件丢弃或合并数量；
- Worktree 清理失败数量；
- 当前活动 Run 和租约过期数量。

### 22.3 审计

以下操作必须记录真实用户：

- 创建需求；
- 创建需求新版本；
- 请求 Artifact 生成；
- 手工创建 Artifact；
- 审核 Artifact；
- 启动或取消 Implementation；
- 审核 Change Set；
- 下载 Patch。
- 下载完整验证输出。

Provider 自身不是审核人。LLM 生成内容的 `created_by` 保存请求用户，`origin=agent` 说明来源。

## 23. 清理

后台清理器只处理终态 Run：

1. 确认 Patch 和验证输出存在且哈希匹配；
2. 确认 `NOW >= retain_until`；`retain_until` 在 Run 进入终态时由终态时间加 `coding_worktree_ttl` 得到；
3. 根据 Run 固定的 `workspace_username` 定位 `<username>-workspace/<run-id>`，并重新校验 Root 和 owner；
4. 执行 `git worktree remove --force <run-path>`，只删除该 Run Worktree；
5. 保留 `<username>-workspace` 父目录和 `.nasuta-owner.json`，供该用户后续 Run 复用；
6. 必要时执行有界 `git worktree prune`；
7. 更新 `worktree_cleaned_at` 或 `cleanup_error`；
8. 保留 Artifact 目录和数据库审计。

活动 Run 不自动删除。失败但无 Patch 的 Worktree 默认保留到 `retain_until` 供诊断，
到期后同样可以清理；人工延长保留期属于后续管理 API，不在第一版 REST 范围。

TTL 只作用于 `<run-id>` 子目录，不作用于 User Workspace 父目录。清理器分页查询到期 Run，
不能一次加载全部历史记录；删除操作必须幂等，Run 目录已经不存在时先核对 Git Worktree
元数据，再决定标记已清理或记录异常。

## 24. 与现有模块的关系

### 24.1 QA Agent

复用：

- LLM Provider 和调用统计基础；
- Knowledge 能力；
- SSE、超时和终态设计经验。

不复用：

- QA Session；
- QA Prompt；
- QA Run 状态；
- QA `RunHub`；
- QA 工具 Snapshot；
- QA Memory。

Feature Delivery 产物是工程事实和决策记录，不能被长期 Memory 覆盖。

### 24.2 Incident

Incident 继续负责告警、根因分析和事件修复。Feature Delivery 不引用 Incident ID，也不把需求任务伪装成 Incident。

Incident 当前直接在原仓库 Checkout 分支的实现不能作为本功能 Worktree 执行器复用。只有未来两个领域都切换到相同的隔离 Git 操作，并产生真实重复时，才抽取共享 Git 包。

### 24.3 Approval 与 Write Action

现有 `pending_actions`、`Proposal.IncidentID` 和 `IncidentFixer` 明确绑定 Incident。Artifact Review 和 Change Review 不属于 Agent 写工具审批，不复用这套表和服务。

第一版没有远端写操作，因此不修改 Write Action Catalog。未来增加 Push/MR 时，需要单独设计：

- 通用 Subject；
- Exactly-Once Claim；
- 平台封闭 Catalog；
- 显式执行 Dispatcher；
- VCS 幂等；
- 失败恢复。

不能直接给 Coding Provider VCS Token 让它自行 Push。

### 24.4 MCP

第一版不增加 MCP 写工具。只读查询是否需要暴露 `get_feature_design` 等工具，等真实 Agent 消费需求出现后再设计。

## 25. 测试策略

### 25.1 领域测试

- Artifact 类型和父级约束；
- 最新有效谱系推导；
- 新需求版本使旧分析及下游过期；
- 新方案版本使旧设计及计划过期；
- 阻塞问题禁止审核通过；
- 一个 Artifact 只能终态审核一次；
- 过期 Artifact 不能启动 Implementation；
- Change Review 只能审核成功 Run。

### 25.2 Store 测试

- Schema Group 包含全部表；
- 用户工作区映射按 `user_id` 幂等读取，规范用户名冲突时稳定分配唯一 Key；
- Artifact 版本并发分配；
- Review 条件写入；
- Run Claim 只有一个 Worker 成功；
- 租约续期和过期扫描；
- 事件 seq 单调且分页有界；
- 任务列表不加载大字段；
- Change Set 元数据和终态事务一致；
- 时间列使用时间类型而不是字符串。

### 25.3 Provider Contract 测试

使用 Fake CLI 进程覆盖：

- 正常 JSONL/stream-json；
- 非 JSON 输出；
- 单事件过大；
- stdout/stderr 超限；
- CLI 非零退出；
- 子进程超时；
- Context 取消；
- Provider Session ID；
- 凭据脱敏；
- Run 网络开关映射到 Provider 原生 Sandbox 策略；
- 配置 Codex 失败时没有执行 Claude，反之亦然。

真实 Codex/Claude 调用是凭据和费用相关集成测试，默认跳过，必须通过显式环境开关运行。

### 25.4 Worktree 测试

使用本地临时 Git 仓库和 Fake Coding Runner 覆盖完整实施链路，禁止依赖真实 Provider、凭据或网络。

- 固定 Base Commit；
- 原工作目录有未提交修改时仍不受影响；
- 首次 Run 创建 `<username>-workspace`、owner 文件和 Run 子目录；
- 同一用户后续 Run 复用父目录并创建新的 Run 子目录；
- 同一用户或同一仓库的多个 Run 可以并行；
- 管理员启动 Run 时仍使用 Feature Request 所有者的 User Workspace；
- 两个同名用户得到不同且稳定的目录 Key；
- 用户展示名变化后继续复用原映射；
- owner 文件与数据库用户不一致时拒绝复用；
- 并发首次创建只产生一个用户映射和一个父目录；
- 路径逃逸和符号链接；
- Provider 修改文件后 Patch 正确；
- Fake Runner 修改、独立验证、Change Set 原子保存和成功终态形成完整闭环；
- 大 Patch 明确失败；
- 取消后子进程组退出；
- TTL 到期后只清理 Run Worktree，保留 User Workspace 父目录；
- 清理失败可重试。

### 25.5 HTTP/SSE 测试

- 所有权和管理员权限；
- `409` 谱系冲突；
- Implementation 按 `client_request_id` 幂等创建；
- SSE 断线后按 seq 重放；
- 订阅窗口不丢状态事件；
- 终态可以通过 GET 恢复；
- 列表 cursor 稳定；
- Patch 流式下载和访问控制。

### 25.6 验证命令

实现每个阶段后运行：

```bash
GOWORK=off go build ./...
GOWORK=off go test ./...
GOWORK=off go vet ./...
```

涉及 Run、Hub、Store 或并发 Claim 的阶段还应运行：

```bash
GOWORK=off go test -race -count=1 ./...
```

## 26. 分阶段实施

### 阶段 1：Artifact 主链

范围：

- Schema Group；
- Feature Request；
- requirement 版本；
- 需求分析、技术方案、系统设计和实现计划生成；
- Artifact 谱系；
- Artifact 审核；
- REST API；
- Generation 审计。

验收：

- 能完成需求到已审核实现计划；
- 上游新版本会使下游准确过期；
- 每个技术事实可追踪到有界证据；
- MySQL 或 LLM 不可用时行为明确。

### 阶段 2：单 Provider 实现

建议先实现 Codex 或 Claude Code 中实际部署环境已经具备凭据和 Binary 的一个，不在第一阶段同时承担两个 CLI 协议风险。

范围：

- Implementation Run 状态机；
- 用户工作区映射和 owner 校验；
- Worktree；
- 一个 Coding Provider；
- Run 事件和 SSE；
- Diff、Patch 和独立验证；
- Change Review；
- 取消、租约和清理。

验收：

- 固定设计、计划和 Base Commit；
- 首次 Run 创建用户父目录，后续 Run 复用父目录但使用独立子目录；
- 不修改原工作目录；
- Provider 失败、取消和服务重启均有确定终态；
- TTL 只删除到期 Run Worktree，不删除用户父目录；
- 成功 Run 有完整 Patch 哈希和验证记录。

### 阶段 3：第二 Provider

范围：

- 增加另一个显式 Provider；
- 共用统一事件和结果契约；
- Provider 能力状态；
- 双 Provider Contract 测试。

验收：

- 配置选择准确；
- 无任何自动 Provider 替换；
- 两个 Provider 产物都进入相同 Change Review 流程。

### 阶段 4：生产隔离和扩展权限

范围：

- 容器或 VM Executor；
- 多实例全局并发；
- 动作级 RBAC；
- 更完整的资源配额；
- 管理员运行策略。

### 阶段 5：发布能力

单独评审后再实现：

- 创建本地分支和 Commit；
- Push；
- 创建合并请求；
- 发布审批；
- VCS 幂等和重试；
- 多仓库发布编排。

## 27. 不采用的方案

### 27.1 直接让 QA Agent 修改代码

不采用。QA 会话不固定 Artifact 谱系和 Base Commit，也不具备代码执行恢复与审核语义。

### 27.2 在 CodeLoom 中实现

不采用。该能力是可复用的平台研发能力，放在下游应用会导致其他 Nasuta 消费者无法使用，并破坏依赖方向。

### 27.3 复用 Incident 状态和 Approval

不采用。Incident 的主题、参数、执行器和 VCS 行为都与 Feature Delivery 不同，强行复用只会隐藏耦合。

### 27.4 自己实现代码编辑 Agent

不采用。文件编辑、命令执行、代码理解和迭代修复已经由 Coding Provider 提供。Nasuta 的价值在流程、知识、约束、隔离和审计。

### 27.5 Provider 自动 fallback

不采用。不同 Provider 的模型、权限、成本、协议和结果不可视为等价。配置失败必须可见。

### 27.6 直接在原仓库 Checkout 和修改

不采用。它会影响开发者工作目录，难以并发，也无法稳定绑定 Base Commit。

### 27.7 自动从语言猜测试命令

不采用。猜测会造成不一致和错误成功。使用 Base Commit 中的结构化验证配置。

### 27.8 保存一个研发任务大状态机

不采用。需求、方案和设计阶段可以从不可变 Artifact 和 Review 推导。只对确实需要运行恢复的 Implementation 建立状态机。

## 28. 验收标准

功能完成必须同时满足：

1. 所有业务实现位于 Nasuta。
2. 下游应用不需要提供 Feature Delivery 业务服务。
3. 每个 Artifact 不可变、可版本化、可审核并带父级 ID。
4. 当前流程阶段无需持久化字段即可确定性推导。
5. 实现 Run 固定有效设计、计划、仓库和 Base Commit。
6. Coding Provider 只在隔离 Worktree 工作。
7. 路径遵循 `<coding-work-root>/worktrees/<username>-workspace/<run-id>`，用户父目录按 Request 所有者创建并复用。
8. TTL 只清理单 Run Worktree，保留用户父目录。
9. Provider 选择显式，失败不 fallback。
10. Provider 结果经过 Nasuta 独立验证。
11. 原仓库工作目录和分支不受修改。
12. Run 可取消，服务重启后活动 Run 进入可解释的 `interrupted`。
13. 列表、事件、日志、Patch 和验证输出全部有界。
14. 普通用户不能审核或启动代码执行。
15. 不持久化隐藏推理和凭据。
16. 成功 Run 只表示变更集生成与验证成功，不表示已发布。
17. Standalone build、test、vet 和关键并发 race 测试通过。

## 29. 实施前需确认

以下产品决定不阻塞总体架构，但应在阶段 1 开发前确认默认值：

1. 普通用户是否只能查看自己的研发任务；
2. Artifact 是否允许管理员手工创建修订版本；
3. 阻塞问题是否绝对禁止审核，还是允许管理员带理由强制批准；
4. 第一 Coding Provider 选择 Codex 还是 Claude Code；
5. Worktree 和 Patch 默认保留时间；
6. 单 Run 最大时长、Patch 大小和文件数量；
7. 第一版是否只部署单实例；
8. 是否需要在阶段 2 就支持跨多个仓库分别启动 Run。

在这些问题确认前，推荐默认采用本文最严格、最简单的行为：仅管理员审核和执行、阻塞问题不可批准、单实例、一个 Run 一个仓库、不允许网络、不自动发布。

## 30. Provider 参考

- [OpenAI Codex 非交互模式](https://developers.openai.com/codex/non-interactive-mode)
- [OpenAI Codex CLI 命令参考](https://developers.openai.com/codex/developer-commands)
- [OpenAI Codex 环境变量](https://developers.openai.com/codex/config-file/environment-variables)
- [Anthropic Claude Code CLI 参考](https://code.claude.com/docs/en/cli-reference)
- [Anthropic Claude Code 认证](https://code.claude.com/docs/en/authentication)
- [Anthropic Claude Agent SDK](https://code.claude.com/docs/en/agent-sdk/overview)
