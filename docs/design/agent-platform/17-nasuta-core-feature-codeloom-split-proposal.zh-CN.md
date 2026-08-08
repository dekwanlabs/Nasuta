# Nasuta Core、Feature 与 CodeLoom 拆分方案

[返回设计索引](README.zh-CN.md)

> 状态：暂停。当前实施基线保持单体 Nasuta 和单 MySQL 库，优先合并同生命周期表；本文仅作为后续规模增长时的备选方案。
> 创建日期：2026-08-07
> 适用范围：Nasuta 平台、Feature Delivery、Incident、Review、CodeLoom 应用与 MySQL Schema
> 关联设计：[Feature Delivery Agent](10-feature-delivery.zh-CN.md)
> 关联方案：[研发节点多 Agent 评审](13-development-multi-agent-review-proposal.zh-CN.md)、[QA、研发任务与多 Agent 统一 Execution Trace](16-unified-execution-trace-proposal.zh-CN.md)

## 1. 结论

Nasuta 不应按“每张表一个服务”拆分，也不应因为当前单库有较多表而直接拆成多个独立仓库。当前应先确立两个可复用产品边界和一个应用边界：

```text
nasuta-feature  -> nasuta-core
codeloom        -> nasuta-core
codeloom        -> nasuta-feature   # 仅启用 Feature 自动化时
nasuta-core     -> 不依赖 nasuta-feature 或 codeloom
```

三者分别承担：

| 单元 | 定位 | 是否可独立复用 |
|---|---|---|
| `nasuta-core` | Agent、QA、Workflow、权限和执行事实的通用运行平台 | 是 |
| `nasuta-feature` | 通用的研发交付、代码变更、评审、Incident 与受控写操作能力 | 是 |
| `codeloom` | 面向具体公司知识库、MCP、Dashboard 与外部系统的应用装配 | 否，按客户/公司业务定制 |

数据库按所有权拆为 `nasuta_core`、`nasuta_feature` 和 `codeloom`。第一阶段保留 monorepo，不立即拆 Git 仓库；先以 Go package/module、配置、数据库访问边界和迁移脚本完成解耦。仓库拆分只能在这些边界稳定后再评估。

当前已实施的阶段 1 边界：

1. `NASUTA_CORE_MYSQL_DSN` 仅安装 Core 表；
2. `NASUTA_FEATURE_MYSQL_DSN` 仅安装 Feature 表，未配置时 Feature 能力明确禁用；
3. `CODELOOM_MYSQL_DSN` 由 CodeLoom 自行连接并仅安装 Observe 表；
4. `app.ExtensionDeps` 不再向 CodeLoom 泄露 Core 数据库连接；
5. Nasuta 内部旧的评估聚合和 HTTP 路由已删除，Eva 是评估持久化和评分的唯一所有者。

尚未实施的项目包括：Feature 对 Core 的公开 Port、`documents` 的所有权迁移、历史数据搬迁，以及 Git 仓库的物理拆分。

## 2. 为什么要拆

### 2.1 当前问题

当前总表数不是主要问题。单个成熟开源项目的一个库拥有数十张表是正常的：运行记录、审计、版本、事件、策略和关联事实不能为了减少表数而混成一张宽表。

当前真正的问题是所有逻辑域被无条件安装和共同装配：

```go
dbschema.MigrateMySQL(db, dbschema.AllGroups()...)
```

这使得仅需 QA/Agent 的部署也会创建 Feature Delivery、Review、Incident 等表；同时 `app.Platform` 在同一 `platformDB` 中无条件初始化 Feature Delivery。结果是：

1. 核心能力与可选产品能力无法独立部署、升级和授权；
2. 单库内的跨域 SQL 让后续拆库变成高风险迁移；
3. CodeLoom 的客户系统接入数据与 Nasuta 通用产品数据混在同一数据库；
4. 评估系统容易把执行 trace、评价标签和统计指标混为同一种持久化职责。

### 2.2 不以减少表数为目标

不建议合并以下表：

- `agent_runs`、`agent_steps`、`agent_llm_calls`、`agent_tool_result_artifacts`：分别承担 Run 摘要、步骤时间线、模型调用成本和大结果归档；
- `workflow_runs`、`workflow_node_runs`、`workflow_events`、`handoff_artifacts`：分别支持恢复、节点状态、可回放事件和跨节点交接；
- Review 的 policy、round、report、finding、adjudication、gate、label 等表：分别是策略定义、一次执行、输出、问题、裁决、门禁和人工标签。

这些表的分离是为了有界读取、失败恢复、审计和评估，不是过度设计。应该移除的是没有生产消费者的表和错误的所有权，而不是为了数量把不同生命周期强行合并。

## 3. 目标架构

```text
                         +-----------------------+
                         |    codeloom 应用层      |
                         | MCP / Dashboard        |
                         | 企业知识与外部系统接入   |
                         +-----------+-----------+
                                     |
                 +-------------------+--------------------+
                 |                                        |
                 v                                        v
       +---------------------+                 +---------------------+
       |     nasuta-core     | <---------------|   nasuta-feature    |
       | QA / Agent Runtime  |   公共 API/Port  | Delivery / Review   |
       | Workflow / Approval |                 | Incident / Writes   |
       | Execution Facts     |                 +---------------------+
       +----------+----------+
                  |
                  v
          +---------------+
          | codeloom-eva  |
          | 评估、标注评分 |
          +---------------+
```

执行 trace 的边界如下：

```text
nasuta-core：产生并查询 Agent / Workflow 的执行事实和关联 ID
codeloom-eva：持久化评估样本、评审标签、评分和报表
codeloom：按请求启用 SSE / MCP trace exporter，并接入企业观测证据
```

因此 Core 不复制 Eva 的评估 trace 存储。Core 只保证 Eva 可稳定关联执行事实，例如 `trace_id`、`agent_run_id`、`workflow_run_id`、`parent_run_id`、`workflow_node_id`、事件序号和时间。

## 4. 职责与表归属

### 4.1 `nasuta_core` 数据库

Core 的表服务于通用身份、QA、Agent 和 Workflow 运行时。它不能依赖 Feature、Incident 或任何 CodeLoom 客户系统表。

| 能力 | 表 |
|---|---|
| 身份、配置与 RBAC | `users`、`sessions`、`settings`、`rbac_roles`、`rbac_user_roles`、`rbac_menus`、`rbac_role_menus`、`rbac_mcp_keys` |
| QA 会话与历史 | `qa_sessions`、`qa_messages`、`qa_turns`、`qa_turn_contexts`、`qa_session_history_terms`、`qa_session_history_index_outbox` |
| QA 长期记忆 | `qa_memories` |
| Agent 定义与执行 | `agent_definitions`、`agent_definition_audit`、`agent_definition_rollouts`、`agent_definition_rollout_audit`、`agent_runs`、`agent_steps`、`agent_tool_result_artifacts`、`agent_llm_calls` |
| Workflow 定义与执行 | `workflow_definitions`、`workflow_definition_audit`、`workflow_definition_rollouts`、`workflow_definition_rollout_audit`、`workflow_runs`、`workflow_node_runs`、`handoff_artifacts`、`workflow_events`、`workflow_approvals`、`gate_decisions` |

`workflow_approvals` 是**通用审批契约**：一个 Workflow 节点要求人工决定时，Core 记录“等待什么决定、由谁决定、决定结果和时间”。它不是 Feature 专属表，也不新增第二套审批产品。

Core 对外提供的最小稳定能力包括：

1. Agent 和 Workflow 定义发布、版本选择、启动、取消、恢复和查询；
2. QA 会话、消息、历史和记忆；
3. Workflow 人工审批的待办和决策；
4. 按 Run ID、父 Run、Workflow、节点和时间查询执行 trace；
5. 受限的工具注册、权限和运行时配置读取。

### 4.2 `nasuta_feature` 数据库

Feature 是可选的通用业务自动化产品。它可以调用 Core 的 Agent/Workflow Runtime，但不能直接读写 Core 的业务表。

| 能力 | 表 |
|---|---|
| Incident 与受控写操作 | `incident_records`、`pending_actions` |
| Feature Delivery | `feature_user_workspaces`、`feature_requests`、`feature_artifacts`、`feature_artifact_reviews`、`feature_generation_runs`、`feature_implementation_runs`、`feature_run_events`、`feature_change_sets`、`feature_change_reviews` |
| Review 策略 | `review_policies`、`review_policy_audit`、`review_policy_rollouts`、`review_policy_rollout_audit` |
| Review 执行与结果 | `review_rounds`、`review_assignments`、`review_round_events`、`review_reports`、`review_report_reuses`、`review_findings`、`review_finding_evidence`、`review_adjudications`、`review_gate_results`、`finding_resolutions`、`review_evaluation_labels` |

`pending_actions` 与 `workflow_approvals` 不重复：

| 项目 | `workflow_approvals` | `pending_actions` |
|---|---|---|
| 所有者 | Core | Feature |
| 表示什么 | Workflow 等待人工继续、拒绝或选择 | Incident/交付流程请求执行一个有副作用的写操作 |
| 处理结果 | 推进或终止 Workflow | 执行、拒绝或过期一项具体 Action |
| 适用范围 | 所有通用 Workflow | Feature/Incident 的受控写操作 |

Feature 使用 `workflow_run_id`、`agent_run_id` 等稳定字符串 ID 关联 Core，不建立跨库外键，也不直接 Join Core 表。

### 4.3 `codeloom` 数据库与本地索引

CodeLoom 是上层的公司业务定制接入。它拥有企业观测来源、快照和知识库应用数据：

| 能力 | 表/存储 |
|---|---|
| 观测源配置 | `observe_sources` |
| 保留的日志证据 | `trace_log_records` |
| 保留的调用链证据 | `trace_chain_records` |
| 知识文档元数据 | `documents`，前提是只服务于 CodeLoom 索引 |
| 已废弃候选 | `observe_history` |
| 检索、本体和代码图 | CodeLoom 管理的 SQLite、Qdrant、BM25 文件和 codegraph 数据 |

`documents` 当前在 Nasuta Schema 中创建，但它若只被 CodeLoom 知识索引使用，应随索引能力迁到 CodeLoom；Core 不应因此携带知识库应用表。若未来确有不依赖 CodeLoom 的通用文档产品，再以独立的 Core 文档接口重新评估，而不是保留模糊归属。

`observe_history` 目前没有生产读写消费者，应先检查线上数据、报表和外部依赖；确认无消费者后删除，不迁移到新库。

## 5. 依赖和访问规则

### 5.1 包与模块规则

1. `nasuta-core` 不 import `nasuta-feature` 或 `codeloom`；
2. `nasuta-feature` 仅通过 Core 的公开 API 或显式 Port 使用 Agent、Workflow、审批和运行事实；
3. `codeloom` 只做应用装配和企业系统 Adapter，不把 Kibana、SkyWalking、Apollo、Backstage 或客户系统逻辑下沉到 Core；
4. Write Action Catalog 由 Core 定义授权边界，Feature 定义具体业务 Action，CodeLoom 注入客户系统执行 Adapter；
5. 所有公共请求、事件和查询对象都应以稳定 ID 和显式版本描述，不能泄露 Store/SQL 类型。

### 5.2 数据库规则

1. 每个数据库只有所属模块的 Store 能访问；
2. 禁止跨库 Join、跨库外键和分布式事务；
3. 需要 Core 事实时，Feature 调用 Core Query API；需要 Feature 事实时，Eva 或 CodeLoom 调用 Feature Query API；
4. 大列表和事件时间线必须在所属 Store 侧使用字段投影、游标和 `LIMIT`；
5. 跨边界操作使用幂等请求 ID、可重试的 outbox/event 或可重放查询，不靠两个库同时提交。

此前 Nasuta 的 `internal/evaluation/store.go` 会对 `agent_*`、`workflow_*` 与 `review_*` 做跨域 SQL Join。该旧实现和 HTTP 路由已删除；Eva 后续必须使用下列方式聚合：

```text
Core 执行事实查询
        +
Feature Review 事实查询
        ->
Eva 评估聚合读模型或 API 层组合
```

不能把现有 SQL 改写成跨库 SQL 后继续长期运行。

## 6. Execution Trace 与评估边界

### 6.1 Core 保留什么

Core 持久化运行时原始事实：

- Agent/Workflow Run 生命周期与状态；
- 节点、Tool、LLM 调用和结果引用；
- 父子 Run 关联，包括 `parent_run_id`；
- `trace_id`、`workflow_run_id`、`workflow_node_id`、开始/结束时间和 Sequence；
- 用户触发的 Workflow 审批事实。

这些数据既用于问题排查，也用于执行 trace 查询。它们是 Core 运行时正确恢复和审计所需的数据，而不是一份额外的评估副本。

### 6.2 Eva 保留什么

`codeloom-eva` 持久化和管理：

- 评估数据集、样本和版本；
- trace 采集结果或不可变引用；
- 人工/自动评分、指标、对比结果和报表；
- Review 标签用于评估时的投影或同步副本。

Eva 通过稳定的 Core/Feature 查询接口读取事实或保存引用。Eva 的评分失败不能影响 QA、Agent 或 Workflow 的主执行链路。

## 7. 迁移计划

### 阶段 0：清理和定界

1. 盘点 `observe_history` 的线上数据量、Dashboard 查询、报表和外部依赖；
2. 无消费者则先停止创建，再按数据保留策略删除；
3. 为三个数据库确定独立 DSN、Schema 名称、备份、权限和迁移账号；
4. 固化 Run 关联字段和 Execution Trace 查询合同，避免迁移期间 ID 语义漂移。

验收：没有生产路径依赖 `observe_history`；三套数据库有独立只读/读写账号。

### 阶段 1：先拆 Schema Installer 和应用装配

1. 将 `dbschema.AllGroups()` 改为模块显式选择的 Group 集合；
2. Core 启动仅安装 Core 表，Feature 启动仅安装 Feature 表；
3. Feature Delivery 仅在 Core 与 Feature 数据库均可用时启用，并输出明确的禁用原因；
4. 新增配置开关和启动日志，明确某个可选能力没有启用，而不是静默回退。

验收：只部署 Core 时不创建任何 Feature/Review/Incident 表；只部署 CodeLoom 的 Observe 时不要求 Feature 表存在。

### 阶段 2：建立跨模块公开 Port

1. 抽取 Core 的 Workflow 启动、Run 查询、审批和 Execution Trace Query Port；
2. Feature 用 Port 替换对 Core Store 的直接依赖；
3. 为所有跨库命令加入幂等键、超时和错误可观测字段；
4. 定义 Feature 到 Core 的必要事件/回调，例如交付任务启动、Review 子 Workflow 完成、审批继续。

验收：Feature 单元测试可用假的 Core Port 运行，不需要 Core MySQL 表。

### 阶段 3：先迁移 Feature 和 Incident

1. 新建 `nasuta_feature` 数据库并仅安装 Feature Schema；
2. 迁移 `incident_records`、`pending_actions`、Feature Delivery 和 Review 表；
3. 采用短暂双读校验或一次性停写迁移，按实际可接受停机窗口选择，不长期保留双写；
4. 将 Core/Feature 关联改为稳定 ID 查询，删除跨库 Join。

验收：Feature 数据库可以单独备份、恢复和清理；Core 不再创建或查询任何 Feature 表。

### 阶段 4：迁移 CodeLoom 应用数据

1. 将 `observe_sources`、`trace_log_records`、`trace_chain_records` 迁到 `codeloom`；
2. 将只服务 CodeLoom 索引的 `documents` 迁到 `codeloom`；
3. 保持 Observe 作为 `incident.EvidenceProvider` 的反向依赖：CodeLoom 向 Feature/Incident 注入证据，不让 Feature import CodeLoom。

验收：Nasuta 在没有 CodeLoom 数据库和企业系统凭证时仍能启动 Core/Feature；CodeLoom 可按客户接入独立配置观测源。

### 阶段 5：迁移评估聚合并关闭旧库路径

1. 在 Eva 中实现 Core/Feature 查询的读模型或 API 聚合；
2. 校验同一时间窗口的指标与已归档历史数据一致；
3. 不恢复 Nasuta 的旧单库评估读写路径或临时兼容配置；
4. 在稳定一个发布周期后再评估是否拆 Git 仓库。

验收：Eva 是唯一的评估持久化和评分所有者；Core/Feature/CodeLoom 之间没有生产跨库 SQL。

## 8. 风险、回滚与非目标

### 8.1 主要风险

| 风险 | 控制方式 |
|---|---|
| 关联查询数量增加 | 提供批量 Query API、游标和专用聚合读模型，不在调用方做 N+1 查询 |
| 迁移时 Run/Review 关联丢失 | 迁移前后校验 ID、行数、状态分布和时间窗口统计 |
| Feature 依赖 Core 失败 | 幂等命令、可重试事件、清晰错误码和恢复任务 |
| 评估指标短期不一致 | 新旧读模型并行校验，Eva 切换后才删除旧路径 |
| 过早拆仓造成发布成本上升 | 首期只拆模块和数据库，不拆 Git 仓库 |

### 8.2 回滚原则

1. 每个阶段先完成可观测的读路径，再切换写路径；
2. 每次迁移保留经过验证的源库备份和明确的回切窗口；
3. 不在长期生产路径维持双写；双写只可作为有截止时间的迁移工具；
4. Core 的 QA/Agent 主路径不依赖 Eva、Feature 或 CodeLoom 的可用性。

### 8.3 非目标

- 不为了“表少”合并具有不同恢复、审计或查询生命周期的表；
- 不在本方案中增加新的审批表或新的 trace 存储；
- 不把企业系统 Adapter 下沉到 `nasuta-core`；
- 不要求所有能力立即改为微服务；
- 不在数据库拆分前先拆 Git 仓库。

## 9. 完成标准

该拆分完成时应满足：

1. 三个数据库各自只包含所属模块的表和迁移历史；
2. Core 可以在没有 Feature 和 CodeLoom 数据库的情况下提供 QA、Agent 和 Workflow；
3. Feature 可以在没有 CodeLoom 的情况下运行通用 Delivery、Review、Incident 和审批型写操作；
4. CodeLoom 可替换或新增企业系统 Adapter，而不修改 Core；
5. Core、Feature、CodeLoom 和 Eva 之间没有跨库 Join 或分布式事务；
6. `workflow_approvals` 继续承担通用 Workflow 人工决策，`pending_actions` 继续承担 Feature/Incident 写操作审批；
7. Eva 不需要 Nasuta 再复制一份评估 trace 持久化，仍能以稳定的 Execution Trace 查询完成评分和报表；
8. 除已确认删除的 `observe_history` 外，现有表都保留其明确业务职责。
