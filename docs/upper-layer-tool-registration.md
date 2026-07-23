# 上层工具注册指南

面向 **上层应用**(如 `codeloom`)的开发者:如何在 Nasuta 平台上注册自己的
只读工具,让它同时出现在 **Agent QA loop** 和 **MCP** 客户端面前。

本文只覆盖 `tool` 包对外开放的 **read 工具** 契约。写操作(`KindWrite`)不通过
这条路径 —— 它们由平台自己的 `internal/writeaction` 目录管理,不对上层注册器开放。

---

## 1. 平台交给上层的东西:`ReadRegistry`

Nasuta 通过 `app.ExtensionDeps` 把一批稳定的构造入口交给上层工厂,其中工具注册
只暴露一个 **受限的只读注册器**:

```go
// github.com/dekwanlabs/nasuta/app
type ExtensionDeps struct {
    Settings                  config.PlatformSettings
    WorkspaceRoot             string
    Knowledge                 knowledge.API
    ReadTools                 *tool.ReadRegistry   // ← 上层唯一的工具注册入口
}
```

上层拿不到底层的 `*tool.Registry`,也拿不到 auth 句柄 —— 只有 `*tool.ReadRegistry`。
这道边界是刻意的:上层只能发布 **只读** 工具,无法注入写操作、无法覆盖平台内置工具
(除非那个 ID 是自己拥有的)。

`ReadRegistry` 只有一个方法:

```go
func (publisher *ReadRegistry) Reconcile(set ReadToolSet) error
```

---

## 2. 工具的形状:`ReadTool`

上层声明的每个工具都是一个 `tool.ReadTool`:

```go
// github.com/dekwanlabs/nasuta/tool
type ReadTool struct {
    ID          ToolID        // 全局唯一、规范化(无首尾空格)的稳定标识
    Description string        // 给模型看的工具说明,必填
    InputSchema JSONSchema    // JSON Schema,描述入参,必填
    Routing     *RoutingSpec  // 可选:按意图路由,匹配时才对 Agent 可见
    Prefetch    *PrefetchSpec // 可选:标记为可信预取工具 + 自定义超时
    Handler     Handler       // 回调,必填
    MCPHidden   bool          // 置 true 则只对 Agent 可见,不暴露给 MCP
}
```

### 各字段的含义与校验规则

| 字段 | 必填 | 说明 | 校验(注册时) |
|------|------|------|------|
| `ID` | ✅ | Agent 与 MCP 共享的稳定标识,如 `"observe_logs"` | 非空;不能有首尾空格(必须 canonical);同一注册器内不能与他人已拥有的 ID 冲突 |
| `Description` | ✅ | 模型据此决定何时调用本工具,写清楚**做什么、返回什么** | 非空 |
| `InputSchema` | ✅ | JSON Schema(见下节),模型据此构造参数 | 非空且结构合法 |
| `Routing` | ❌ | 设了则工具**只在意图匹配时**对 Agent 可见,减少无关工具干扰 | 设了则 `Intent` 必须非空 |
| `Prefetch` | ❌ | 标记为可在专项调查前被**可信预取**;可覆盖执行超时 | 设了则 `Description` 非空、`Timeout` 不为负 |
| `Handler` | ✅ | 实际执行逻辑(下节详述) | 非空 |
| `MCPHidden` | ❌ | `true` = 仅 Agent 可见,MCP 工具列表里隐藏 | — |

> `ReadTool` 内部会被转换成 `Kind: KindRead` 的 `Tool`,上层无法把它变成 write。

### 一次发布全量:`ReadToolSet`

```go
type ReadToolSet struct {
    Owner string      // 你这批工具的命名空间,如 "codeloom.observe",必填且 canonical
    Tools []ReadTool  // 你期望存在的完整工具集
}
```

**`Reconcile` 是"声明期望的最终状态",不是"追加"**:

- 它会让**你名下**的工具,精确变成 `Tools` 里列出的这些;
- 你上次发过、这次没列的,会被**移除**;
- 所以工具集变化时(如某后端从启用变禁用)可以反复调 `Reconcile`,
  禁用态返回空集就能自动摘除,不用手动 `Unregister`。

`Owner` 的作用只有两点,不用想复杂:

1. 它是你这批工具的命名空间;
2. 它保证你**覆盖不了不属于你的工具** —— 尤其是 Nasuta 的内置工具
   (`search_code`、`get_service` 等)。想复用一个已存在的 ID,那个 ID 必须已经归你
   所有,否则报错 `id is owned by another catalog`。

> 实践中一个上层应用通常只有一个 owner。上面第 1 条的"只增删自己名下的工具",在单
> owner 下的意义就是:你的 `Reconcile` 只动自己的工具,永远不会误删平台内置工具。

### 2.1 快速注册自己的工具

最短路径是三步:拿到 `deps.ReadTools`、声明一个 `ReadToolSet`、在启动或配置变更时调用
`Reconcile`。

#### 第一步:在你的上层模块里写一个 `ReadToolSet`

```go
package mytools

import (
    "context"
    "encoding/json"
    "fmt"
    "strings"

    "github.com/dekwanlabs/nasuta/tool"
)

func ReadToolSet(client *Client) tool.ReadToolSet {
    set := tool.ReadToolSet{Owner: "myapp.runtime"}
    if client == nil || !client.Enabled() {
        return set // 禁用态返回空集,Reconcile 会摘除旧工具。
    }
    set.Tools = []tool.ReadTool{{
        ID:          "myapp_lookup_runtime",
        Description: "Look up read-only runtime status for one service. Returns status, timestamp, and short evidence.",
        InputSchema: tool.JSONSchema{
            "type": "object",
            "properties": map[string]any{
                "service": map[string]any{
                    "type":        "string",
                    "description": "Service key, for example payment-api.",
                },
            },
            "required": []string{"service"},
        },
        Routing: &tool.RoutingSpec{
            Intent: "Use when the request needs current read-only runtime status for a named service.",
        },
        Handler: tool.HandlerFunc(func(ctx context.Context, args tool.Arguments) (tool.Result, error) {
            service := strings.TrimSpace(args.String("service"))
            if service == "" {
                return tool.Result{}, fmt.Errorf("service is required")
            }
            status, err := client.LookupStatus(ctx, service)
            if err != nil {
                return tool.Result{}, fmt.Errorf("lookup runtime status for %q: %w", service, err)
            }
            data, err := json.MarshalIndent(status, "", "  ")
            if err != nil {
                return tool.Result{}, fmt.Errorf("encode runtime status: %w", err)
            }
            return tool.Result{Content: string(data)}, nil
        }),
    }}
    return set
}
```

如果返回值本来就是结构体,可以把 `HandlerFunc` 换成 `JSONHandler`:

```go
Handler: tool.JSONHandler(func(ctx context.Context, args tool.Arguments) (*Status, error) {
    return client.LookupStatus(ctx, args.String("service"))
}),
```

#### 第二步:把 `ReadTools` 传到你的模块

```go
func New(deps app.ExtensionDeps) (app.Extension, error) {
    runtime := NewRuntime(deps.Settings, deps.ReadTools)
    return app.Extension{
        RegisterRoutes: runtime.RegisterRoutes,
        Close:          runtime.Close,
    }, nil
}
```

#### 第三步:启动和配置变更时都调用 `Reconcile`

```go
func NewRuntime(settings config.PlatformSettings, readTools *tool.ReadRegistry) *Runtime {
    rt := &Runtime{settings: settings, readTools: readTools}
    rt.reloadTools()
    return rt
}

func (rt *Runtime) reloadTools() {
    if rt.readTools == nil {
        return
    }
    if err := rt.readTools.Reconcile(mytools.ReadToolSet(rt.client)); err != nil {
        log.Errorf("[myapp] publish read tools: %v", err)
    }
}
```

调试时按这条链路查问题:

1. `ReadToolSet.Owner` 非空且无首尾空格;
2. `Tools` 里每个 `ID`、`Description`、`InputSchema`、`Handler` 都非空;
3. `Reconcile` 没有返回错误;
4. Agent 场景下 `Routing` 是否匹配当前问题;
5. MCP 场景下是否误设了 `MCPHidden: true`。

---

## 3. 入参:`InputSchema`(JSON Schema)

`InputSchema` 是 `map[string]any` 形式的 JSON Schema。注册时会做结构校验,**每次调用前**
会用它校验实际参数。支持的类型:`object` / `array` / `string` / `integer` / `number` /
`boolean`,以及顶层的 `oneOf` / `anyOf` 备选。

```go
InputSchema: tool.JSONSchema{
    "type": "object",
    "properties": map[string]any{
        "service": map[string]any{
            "type":        "string",
            "description": "目标服务的 key,例如 hsas-device",
        },
        "minutes": map[string]any{
            "type":        "integer",
            "description": "回看的时间窗口(分钟)",
        },
        "level": map[string]any{
            "type": "string",
            "enum": []any{"error", "warn", "info"},
        },
    },
    "required": []string{"service"},
},
```

调用时的参数校验(`validateArguments`)规则:

- `required` 里列的属性缺失 → 报错;
- 类型不符(如 `integer` 传了小数)→ 报错;
- 声明了 `enum` 而值不在其中 → 报错;
- **未在 `properties` 声明的多余属性会被忽略**,不报错。

在 Handler 里取参用 `Arguments` 的辅助方法,自带类型兜底:

```go
func (args Arguments) String(key string) string          // 取字符串,缺省 ""
func (args Arguments) Int(key string, fallback int) int  // 兼容 int / float64(JSON 数字)
```

---

## 4. 回调:`Handler` 如何执行

### 4.1 Handler 接口

```go
type Handler interface {
    Execute(context.Context, Arguments) (Result, error)
}

// 用函数直接适配:
type HandlerFunc func(context.Context, Arguments) (Result, error)
```

返回的 `Result` 是回传给调用方的**有界内容**:

```go
type Result struct {
    Content    string      `json:"content"`              // 主体文本
    References []Reference `json:"references,omitempty"`  // 可选:证据引用
}

type Reference struct {
    Type   string `json:"type"`
    Label  string `json:"label"`
    Target string `json:"target"`
}
```

### 4.2 推荐:用 `JSONHandler` 返回结构体

如果你的逻辑天然返回一个结构体,用 `tool.JSONHandler` 泛型适配器,它会自动
`json.MarshalIndent` 成 `Result.Content`,省掉手写序列化:

```go
Handler: tool.JSONHandler(func(ctx context.Context, args tool.Arguments) (*ObserveResult, error) {
    return srv.ExecuteTool(ctx, map[string]any(args))
}),
```

### 4.3 执行时序:`Executor` 做了什么

工具不是被直接调用的,而是经过 `tool.Executor`,它在调用 Handler 前后做三件事:

1. **快照查找** —— 从当次运行 pin 住的 `Snapshot` 里按 ID 取工具(取不到即报错,
   保证一次运行内工具定义不变);
2. **参数校验** —— 用 `InputSchema` 校验 `args`,不合法直接返回错误,不会进入 Handler;
3. **超时控制** —— 用 `context.WithTimeout` 包裹执行;超时取 `Prefetch.Timeout`(若设了
   正值)否则取 `Executor.DefaultTimeout`;两者都 ≤0 时不加超时。

所以 **Handler 必须尊重传入的 `ctx`**:长耗时操作要监听 `ctx.Done()`,及时中止。

### 4.4 错误约定

- Handler 返回 `error` 即代表本次工具调用失败,错误会原样上抛给 Agent/MCP;
- 遵循平台约定 `fmt.Errorf("... %q: %w", x, err)`,让上层能 `errors.Is`/`errors.As`;
- **不要**在 Handler 里静默吞错返回空 `Result` —— 失败要显式报错(与平台"no silent
  fallback"约定一致)。

---

## 5. 完整接线示例(取自 codeloom 的 `observe_logs`)

### 5.1 声明工具集(`internal/observe/readtool.go`)

```go
func (srv *Service) ReadToolSet() tool.ReadToolSet {
    set := tool.ReadToolSet{Owner: "codeloom.observe"}
    if srv == nil || !srv.LogsEnabled() {
        return set // 后端未启用 → 返回空集,Reconcile 会移除旧工具
    }
    spec := srv.ToolSpec()
    set.Tools = []tool.ReadTool{{
        ID:          "observe_logs",
        Description: spec.Description,
        InputSchema: tool.JSONSchema(spec.Parameters),
        Routing:     &tool.RoutingSpec{Intent: spec.Description},
        Prefetch: &tool.PrefetchSpec{
            Description: "Load time-bounded runtime logs and traces before a dedicated investigation.",
            Timeout:     90 * time.Second,
        },
        Handler: tool.JSONHandler(func(ctx context.Context, args tool.Arguments) (*ObserveResult, error) {
            return srv.ExecuteTool(ctx, map[string]any(args))
        }),
    }}
    return set
}
```

### 5.2 拿到注册器并发布(`internal/transport/handler.go`)

```go
func New(cfg config.Config, db *sql.DB, readTools *tool.ReadRegistry) *Runtime {
    handler := &Handler{cfg: cfg, readTools: readTools, /* ... */}
    handler.reloadObserve()
    return &Runtime{Handler: handler, IncidentEvidence: handler}
}

func (handler *Handler) reloadObserve() {
    service := observe.NewWithStoreAndDirectory(/* ... */)
    handler.obs.Store(service)
    // 配置变化时可反复调用;Reconcile 声明式对齐,不会重复堆叠。
    if err := handler.readTools.Reconcile(service.ReadToolSet()); err != nil {
        log.Errorf("[app] publish observe tool: %v", err)
    }
}
```

### 5.3 通过 `app.Extension` 接入平台生命周期

```go
// internal/runtime/runtime.go —— ExtensionFactory
func New(deps app.ExtensionDeps) (app.Extension, error) {
    // ...
    appRuntime := transport.New(cfg, db, deps.ReadTools) // ← 把 ReadTools 传下去
    return app.Extension{
        RegisterRoutes:   appRuntime.Handler.RegisterRoutes,
        IncidentEvidence: appRuntime.IncidentEvidence,
        Close:            db.Close,
    }, nil
}

// cmd 入口
func main() { nasutaapp.MustRun(runtime.New) }
```

---

## 6. 预算、注意事项和清单

### 6.1 每个工具的上下文预算

注册工具时没有单独的 `ContextBudget` 字段。Nasuta 统一按**使用场景**分配预算,所以工具
作者要控制 `Result.Content` 的大小,而不是依赖注册器替你做业务级分页。

| 场景 | 单工具可进入模型/上下文的预算 | 说明 |
|------|-------------------------------|------|
| Agent 普通工具调用 | 约 `10_000` estimated token/次工具结果 | `Result.Content` 先完整写入运行记录。模型侧内容超预算时按 JSON、JSONL 或文本结构分块,根据当前问题和调用参数选择相关块,再生成带覆盖元数据的合法 JSON envelope。 |
| Agent 可信预取(`Prefetch`) | 所有预取块合计最多 `16_000` rune | 预取结果先变成 `PreloadedContext`,再和检索上下文合并。这个 `16_000` 是所有预取工具共享,不是每个工具各有 `16_000`。 |
| 检索+预取总上下文 | 默认 `48_000` rune,可由平台 `context_budget` 调整 | 预取内容会优先写入,剩余预算留给普通检索上下文。若 `context_budget < 16_000`,预取上限也会被压到该总预算。 |
| MCP 调用 | Nasuta 不做模型上下文裁剪 | MCP 返回完整 `Result.Content`;真正能放进模型多少由 MCP 客户端决定。Nasuta 只负责执行超时和参数校验。 |
| 工具 schema/description | 没有单工具硬上限 | 工具定义会进入模型的工具列表。描述和 schema 越长,越挤占模型输入窗口,也越影响工具选择稳定性。 |

执行时间预算也按场景统一处理:

- Agent 普通工具调用默认 `15s` 超时;
- MCP 调用默认 `30s` 超时;
- 如果工具设置了 `Prefetch.Timeout > 0`,该超时会覆盖默认执行超时;
- 超时通过 `context.Context` 传给 Handler,Handler 必须把 `ctx` 传给下游 HTTP/DB/RPC 调用。

工具结果大小建议:

- 普通工具尽量返回 `2_000` 到 `5_000` token 以内的高信号摘要,必要时提供 `limit`、`cursor`、`since` 等参数;
- 日志、搜索、链路类工具返回排序后的 Top N,并说明 `truncated`、`next_cursor`、`total` 等边界;
- 工具仍应在查询边界限制读取规模并返回 `total`、`truncated`、`next_cursor` 等业务边界;Runtime 压缩不能替代分页或 Top N;
- 大 JSON 超出统一预算时由 Nasuta Runtime 做通用结构化压缩,工具作者不应感知模型预算或构造 `_nasuta` envelope;
- 证据链接放 `References`,正文只放模型回答需要阅读的内容;
- 如果工具经常需要超过 `10_000` token 才有用,优先拆成“搜索/列表工具 + 精读工具”。

### 6.2 注册工具要注意什么

#### 命名与所有权

- `Owner` 用稳定命名空间,建议 `"<app>.<module>"`,例如 `"codeloom.observe"`;
- `ID` 是 Agent 和 MCP 共享的全局稳定标识,发布后不要随意改名;
- 上层工具最好带应用前缀,例如 `"myapp_lookup_runtime"`,避免和内置工具或其他上层冲突;
- 同一个 `Owner` 下反复 `Reconcile` 是覆盖式发布,不要把其他模块的工具漏掉;
- 不能注册 `KindWrite`,写操作走平台 `writeaction` 体系。

#### 描述、路由与暴露面

- `Description` 要写清“什么时候用、输入是什么、返回什么”,不要写实现细节或营销文案;
- `Routing.Intent` 适合窄领域工具,能减少 Agent 每轮看到的无关工具;
- 没有 `Routing` 的 read 工具会更容易进入 Agent 工具列表,只适合通用工具;
- `MCPHidden: true` 只影响 MCP 列表,不影响 Agent 使用;
- 不要把密钥、内部 URL token、用户隐私放进 description、schema 或工具结果。

#### Schema 与参数

- 顶层通常使用 `type: object`,并明确 `properties` 和 `required`;
- 为可枚举参数写 `enum`,例如日志级别、方向、模式;
- 给大结果工具提供 `limit`、`max_lines`、`minutes`、`cursor` 等收敛参数;
- Handler 里仍要做业务校验,例如空字符串、时间窗口过大、服务不存在;
- 未声明的多余参数会被忽略,不要依赖它们做隐藏开关。

#### Handler 行为

- Handler 必须是只读、幂等、可重试的;
- 显式配置的后端失败时返回 `error`,不要静默降级或返回空结果;
- 所有网络、数据库、RPC 调用都要使用传入的 `ctx`;
- 返回内容要有边界,不要把原始全量日志、全量表、全量文件直接塞进 `Content`;
- 错误信息要便于定位,但不要泄露密钥、连接串或完整敏感请求体。

### 6.3 最终检查清单

注册一个上层工具时,确认:

- [ ] `Owner` 用了自己的命名空间(如 `"<app>.<module>"`),且 canonical;
- [ ] `ID` 稳定、全局唯一、无首尾空格;
- [ ] `Description` 说清了做什么、返回什么(模型据此选择工具);
- [ ] `InputSchema` 声明了所有入参与 `required`,类型正确;
- [ ] `Handler` 尊重 `ctx`,失败显式返回 `error`(不静默吞错);
- [ ] 长耗时工具设了合理的 `Prefetch.Timeout`;
- [ ] 后端可能禁用时,`ReadToolSet` 在禁用态返回空集,靠 `Reconcile` 自动摘除;
- [ ] 只想给 Agent 用、不暴露 MCP 的工具,置 `MCPHidden: true`。
