# Nasuta 代码解析架构

## 1. 背景

当前代码扫描器把语言语法、框架语义、服务归属和最终事实写入混在了一层：

- Java 的路由识别主要围绕 Spring MVC 注解；
- Python 的路由识别主要围绕 FastAPI decorator；
- Go 通过 `.GET`、`.Get`、`.HandleFunc` 等方法名和正则猜测路由；
- 多个扫描器各自推断 service/module，结果容易重复或归属错误；
- 动态路径、未知 receiver 和解析失败有被当成根路径或 `ANY` 的风险。

这种实现新增一个框架时，通常需要继续向语言扫描器里增加特判。长期结果是召回率、误报率和可维护性都随框架数量恶化。

本方案的目标不是构造一套跨语言统一 AST，而是把**语言前端**和**框架适配器**解耦，并在写入 domain 之前建立严格的解析状态边界。

## 2. 目标与非目标

### 2.1 目标

1. 同一种语言可以同时支持多个互不相关的框架。
2. 框架识别必须有可追溯的源码证据。
3. 动态值不会被伪造成静态路径、根路径或任意 HTTP method。
4. 新增框架主要通过新增 adapter 完成，不修改语言前端和公共投影逻辑。
5. endpoint 的 service/module 归属可解释、可测试、可复现。

### 2.2 非目标

1. 第一阶段不构造跨语言统一 AST。
2. 第一阶段不修改 SQLite 或 `domain.EndpointRecord` 的持久化 schema。
3. 不承诺在没有类型分析、构建配置或运行时信息时还原所有依赖注入和动态路由。
4. 不为了覆盖单个框架而在公共层加入业务名称、变量名或路径关键词特判。

## 3. 目标处理链路

```text
源码文件
  │
  ▼
ModuleCatalog                统一文件 → repo/module/service 归属
  │
  ▼
Language Frontend             只解析语言结构和 source span
  │  Java IR / Go AST / Python structural tokens
  ▼
Framework Adapter              解释某一个框架的 import、构造和调用语义
  │
  ▼
EndpointCandidate              保留 literal/configReference/unresolved 状态
  │
  ▼
Resolved Projector             只投影完整、无歧义的 candidate
  │
  ▼
domain.EndpointRecord          现有结构化存储契约
```

语言前端不判断“这是 Spring 还是 Gin”。适配器也不重新解析注释、字符串或括号；两者之间只通过语言专属 IR 和框架无关的 candidate 交接。

## 4. 中间模型

### 4.1 值解析状态

```go
type valueKind uint8

const (
    valueUnresolved valueKind = iota
    valueLiteral
    valueConfigReference
)

type valueExpr struct {
    kind  valueKind
    raw   string // 原始表达式，用于诊断
    value string // literal 的值，或 config reference 的 key
}
```

语义约束：

| 状态 | 示例 | 是否可写入 endpoint |
|------|------|---------------------|
| `literal` | `"/users"`、常量拼接后的 `"/v1/users"` | 可以 |
| `configReference` | `os.Getenv("ROUTE_PREFIX")`、配置占位符 | 不可以，除非未来有明确配置解析结果 |
| `unresolved` | 分支合并、函数返回值、动态格式化 | 不可以 |

`literal("")` 表示框架明确表达的根路径。它和 `unresolved` 完全不同，不能在 canonicalization 阶段混淆。

### 4.2 EndpointCandidate

```go
type endpointCandidate struct {
    ServiceName   string
    Repo          string
    ModulePath    string
    Language      string
    Framework     string
    Methods       []valueExpr
    Paths         []valueExpr
    Handler       string
    HandlerMethod string
    Evidence      domain.Evidence
    Confidence    float64
}
```

candidate 只存在于 indexer 内部。只有 method、path 都是 literal，且 receiver、prefix、host 等前置条件没有歧义时，才进入 `domain.EndpointRecord`。

### 4.3 Adapter 注册

第一阶段使用显式函数表，不引入插件系统或复杂 DSL：

```go
type endpointAdapter struct {
    language  string
    framework string
    applies   func(source languageSource) bool
    scan      func(source languageSource) []endpointCandidate
}
```

实现上应优先使用 Java、Go、Python 各自的 typed source/IR 切片；公共 registry 只负责调度，避免用一个大型 `any` 类型掩盖语言差异。

### 4.4 多框架并行调度

框架不是项目或语言级的单选项。同一个 module、package 甚至同一个文件都可能同时使用多个框架，因此不能先检测一个“主框架”再排他扫描。

正确的调度方式是：

```text
源码文件
  │ 只解析一次
  ▼
Language Source/IR
  ├─ Spring MVC adapter ─┐
  ├─ JAX-RS adapter ─────┼─ EndpointCandidate[]
  └─ 其他 adapter ───────┘
                              │
                              ▼
                      resolved projector
```

每个 adapter 先根据 import 索引做低成本筛选，再验证具体 annotation、constructor 和 receiver provenance。所有满足证据条件的 adapter 都可以运行，不存在全局 `frameworkMode`。

示例：

| 源码证据 | 参与扫描的 adapter |
|----------|---------------------|
| Java 同时 import Spring MVC 和 JAX-RS | Spring MVC、JAX-RS |
| Go 同时 import `net/http` 和 Gin | net/http、Gin |
| Python 同时构造 FastAPI 与 Flask app | FastAPI、Flask |

不同 adapter 产出的相同 `service + method + path` 最终按稳定规则去重。adapter 不能因为文件中存在某个框架 import，就把所有同名方法都归给该框架；证据必须落实到当前 annotation 或 receiver。

## 5. 证据规则

### 5.1 通用规则

以下内容单独出现时都不是框架证据：

- 变量名 `router`、`r`、`app`；
- 方法名 `.Get`、`.GET`、`.Handle`；
- 注解或 decorator 的简单名称；
- 文件名包含 `route`、`controller`；
- 某个字符串看起来像 URL。

可接受的证据包括：

1. 精确 import path；
2. 框架已知构造函数返回值；
3. 已确认对象的赋值传播和 receiver provenance；
4. 已确认 router 的 group/mount/route 派生关系；
5. 明确的全限定 annotation/decorator。

没有足够证据时，adapter 返回空结果或 unresolved candidate，不进行弱猜测。

### 5.2 静态值求值边界

第一阶段只求值以下形式：

- 普通字符串和 raw string；
- 括号包裹的字符串；
- 已确认的字符串常量；
- 两个已解析字符串的 `+` 拼接；
- 框架明确的 method 常量，例如 `http.MethodGet`。

以下形式保持 unresolved：

- `fmt.Sprintf`、f-string、模板插值；
- 环境变量和配置读取；
- 未知函数返回值；
- 多个 reaching definition 无法唯一确定；
- 动态 host 或动态 group prefix。

## 6. 各语言前端与适配器

### 6.1 Java/JVM

#### 前端

- lexer 屏蔽注释、字符串和 text block 中的结构符号；
- 提取 import、annotation、参数 token；
- 将 annotation 绑定到紧随其后的 class/method declaration；
- Java 和 Kotlin 的声明边界分别处理，不能把 Java declaration parser 当作 JVM 通用 parser。

#### 适配器

**Spring MVC**

- 仅消费能由 Spring annotation import 或全限定名确认的 `Controller`、`RestController`、`RequestMapping` 及派生 mapping；
- 合并 class-level prefix 和 method-level path；
- `RequestMapping` 未指定 method 时才产生 `ANY`；
- `@GetMapping(PATH)` 等动态表达式保持 unresolved。

**JAX-RS**

- 通过 `javax.ws.rs.*` 或 `jakarta.ws.rs.*` import 确认；
- 处理 class/method `@Path` 与 `@GET`、`@POST` 等 HTTP annotation；
- 无参数 `@Path` 不自动解释为根路径，除非该 annotation 的语义明确允许空值。

后续可独立增加 Micronaut、Quarkus REST、Vert.x 等 adapter，不改 Java lexer。

### 6.2 Go

#### 前端

使用标准库 `go/parser` 和 `go/ast`，按 package 聚合文件，记录：

- import alias、dot/blank import 和遮蔽关系；
- const/var/type/function 声明；
- 调用表达式、receiver、参数和 source span；
- router 构造、赋值、group 派生及路由注册。

仅逐行正则扫描不能可靠处理多行调用、嵌套调用、跨文件常量和注释，因此应在切换后删除旧 Go endpoint 正则。

#### 适配器

**net/http**

- 精确识别 `net/http` 的 `Handle`、`HandleFunc`；
- 只接受 `http.NewServeMux()`、`*http.ServeMux` 或 `http.DefaultServeMux` 的 receiver；
- Go 1.22 pattern `"GET /users/{id}"` 拆出 method/path；
- 无 method pattern 才是 `ANY`；
- host-specific pattern 在现有 domain 没有 host 字段时不投影。

**Gin/Echo/Chi/Fiber/Gorilla mux 等**

- 先确认 import path，再确认 root/group receiver provenance；
- group prefix 作为 router graph 传播；
- `Any`、`All`、`Handle` 等只有在具体框架语义明确时才产生 `ANY`；
- 未确认的 `Match`、`Get`、`Handle` 调用不产生 endpoint。

helper 函数和依赖注入若无法通过数据流证明，宁可漏报。后续可在有类型信息时扩展，而不是回退到变量名猜测。

### 6.3 Python

#### 前端

前端只记录：

- import/alias；
- module-level 构造赋值；
- decorator 的 receiver、method、参数 token；
- decorator 所绑定的 function/class 和 source span。

必须屏蔽注释、普通字符串和 triple-quoted string 中的伪 decorator，并支持多行 decorator 参数。

#### 适配器

**FastAPI**

- 只接受由 `FastAPI()` 或 `APIRouter()` 构造、并经过可证明赋值传播的 receiver；
- 支持 `prefix` 和 method decorator；
- `@cache.get("/x")` 即使名字相似也不产 endpoint。

**Flask**

- 只接受 `Flask()` 或 `Blueprint()` receiver；
- 解释 `route(..., methods=[...])`、`get/post` 等明确 API；
- 未解析的 `methods`、prefix 或 dynamic path 不投影。

后续可以增加 Django、Starlette、Sanic 等 adapter，但不应把它们的规则写进 Python 前端。

### 6.4 Kotlin

Kotlin 不是 Java 的另一种文件扩展名。它可以共享 JVM 的注释和字符串词法处理，但 class、function、lambda 和 DSL 的声明边界必须由 Kotlin 前端单独处理。

当前结构化扫描器已经覆盖 Spring annotation、Ktor routing DSL 和 Javalin 的一部分规则。目标 adapter 划分如下：

- Spring MVC/JAX-RS：沿用对应框架的 import 和 annotation 证据；
- Ktor：从 `embeddedServer`、`routing`、`route`、HTTP method DSL 建立 router graph，支持嵌套 prefix；
- Javalin：只消费由 Javalin 构造或类型证明的 app/route receiver；
- Kotlin Feign/HTTP client：作为 client adapter，不和服务端 annotation adapter 混合。

`fun main` 只能证明存在 Kotlin 入口，不能单独把服务标成 Spring Boot；`embeddedServer` 才是 Ktor runtime 证据之一。

### 6.5 C#/.NET

C# 前端需要识别 namespace/import、attribute、class/interface、method、lambda 和 invocation。不能继续用一行一个正则同时解释 ASP.NET、ServiceStack 和 Refit。

首批 adapter：

- ASP.NET Core Controller：`[ApiController]`、`[Route]`、`[HttpGet]` 等 attribute 必须有 Microsoft ASP.NET 证据；
- ASP.NET Core Minimal API：只接受由 `WebApplication`/`IEndpointRouteBuilder` 证明的 `MapGet`、`MapPost` 等 receiver；
- ServiceStack：通过 ServiceStack namespace、request DTO 和 `[Route]` 语义确认；
- Refit/HttpClient：分别抽取 interface method 的 HTTP operation 和直接 client 调用。

class-level route 与 method-level route 先在 candidate 中组合；attribute 参数、配置 base URL 和 lambda 目标无法静态确定时保持 unresolved。

### 6.6 JavaScript/TypeScript

JavaScript 和 TypeScript 共享 ECMAScript 调用模型，但 TypeScript 还需要处理 type annotation、decorator 和 interface。前端应记录 `import`/`require` alias、对象构造、成员调用、decorator 和 object literal，而不是扫描 `.get(` 字符串。

首批 adapter：

- Express：`express()`、`express.Router()`、`use`/method registration 和 router prefix；
- Fastify：`fastify()`、`register`、method/route object；
- Koa：由 Koa/router 构造的 app/router receiver；
- NestJS：`@nestjs/common` 的 `@Controller`、HTTP decorator 和 module/controller prefix；
- Hapi：由 `@hapi/hapi` 创建的 server，以及 `server.route({ method, path })` 对象。

同一个文件中普通对象的 `get`、数据库 client 的 `query` 或缓存对象的 `route` 都不能仅凭成员名识别为 HTTP endpoint。JS/TS client adapter 还应覆盖 axios、fetch、undici 等 HTTP 调用。

### 6.7 Android 与 iOS/macOS

Android 和 iOS 是平台边界，不应被当成独立语言前端：

- Android 复用 Java/Kotlin 前端，增加 Retrofit、OkHttp、Ktor Client 等 client adapter；Retrofit 的 `@GET/@POST` 应进入 `ClientCallCandidate`，而不是伪装成服务端 endpoint；
- Swift/Objective-C 使用各自前端，增加 `URLSession`、Alamofire、Moya 等 client adapter；
- Swift 服务端项目可以另行增加 Vapor、Hummingbird adapter；
- `Info.plist`、AndroidManifest 和 Xcode/Gradle 文件只用于 module/platform ownership，不作为路由语义来源。

移动端 HTTP 调用通常只有外部 target，无法匹配 workspace endpoint 时仍可形成 external service dependency；未知 URL 不得根据路径片段猜出 `xxx-service`。

### 6.8 Rust、Dart 及其他代码语言

当前 `code.go` 已支持 Scala、Groovy、Rust、Dart、Ruby、PHP、Lua、R、Perl、C/C++、Objective-C 等语言的代码块和语义索引，但这不等于已经有结构化 endpoint/dependency adapter。

建议按实际项目需求分批增加：

| 语言 | 首批服务端 adapter | 首批 client/RPC adapter | 当前阶段 |
|------|--------------------|-------------------------|----------|
| Rust | Axum、Actix Web、Warp、Rocket | reqwest、hyper、tonic | 规划 |
| Dart | shelf、Dart Frog、Conduit、Serverpod | `http`、Dio | 规划 |
| Scala | Play、Akka HTTP、http4s | sttp、Akka HTTP client | 规划 |
| Groovy | Spring MVC、Ratpack | Spring HTTP client | 规划 |
| Ruby | Rails、Sinatra、Roda | Net::HTTP、Faraday | 规划 |
| PHP | Laravel、Symfony、Slim | Guzzle | 规划 |
| C/C++ | 具体 HTTP server library | libcurl、gRPC/C++ | 按需求 |
| Lua/R/Perl | 暂不承诺通用路由识别 | 按具体库增加 | 语义索引 |

这些语言的共同规则仍然不变：先建立语言前端，再由框架 adapter 解释调用语义。没有 adapter 时只提供代码检索，不生成未经证明的 endpoint 或 dependency。

### 6.9 结构化覆盖矩阵

| 语言/平台 | 当前结构化扫描 | 目标 adapter 方向 | 说明 |
|-----------|----------------|-------------------|------|
| Java | Spring MVC（迁移中）、JAX-RS（新增） | Spring、JAX-RS、Micronaut 等 | JVM 注释前端与 Java 声明前端分离 |
| Kotlin | Spring、Ktor、Javalin（旧规则） | Kotlin frontend + typed adapters | 不能直接复用 Java declaration parser |
| Go | 旧正则 + 新 AST 迁移中 | net/http、Gin、Echo、Chi、Fiber 等 | 按 package 解析 receiver provenance |
| Python | FastAPI（迁移中） | FastAPI、Flask、Django、Starlette 等 | decorator 前端框架无关 |
| C# | ASP.NET、Minimal API、ServiceStack（旧规则） | .NET AST + adapters | attribute 与 invocation 分开 |
| JavaScript/TypeScript | Express/Fastify/Koa/Nest/Hapi（旧规则） | JS/TS AST + adapters | import/require 和对象流分析 |
| Android | Retrofit/OkHttp 依赖（旧规则） | Java/Kotlin client adapters | 平台不是语言 |
| Swift/Objective-C | iOS HTTP 依赖（旧规则） | URLSession/Alamofire/Moya、Vapor | 移动端主要是 client |
| Rust/Dart/其他 | 代码块/语义索引 | 按需求增加 adapter | 不把语义索引误称为接口解析 |

## 7. HTTP 与 RPC 接口依赖

### 7.1 服务端定义与客户端调用分离

接口依赖不能只靠服务端 endpoint，也不能只从 URL 字符串直接生成 dependency。需要分别抽取服务端定义和客户端调用，再进行关联：

```text
Language IR
  ├─ Server Adapter → EndpointCandidate → EndpointRecord
  │                                      │
  └─ Client Adapter → ClientCallCandidate┼→ Dependency Resolver
                                         │          │
ModuleCatalog + Config Resolver ─────────┘          ▼
                                           DependencyEdge
```

Server adapter 和 client adapter 可以复用同一份语言 IR，但不能混成一个 adapter。前者解释路由注册，后者解释 HTTP/RPC client 的调用语义。

### 7.2 ClientCallCandidate

客户端调用先进入内部 candidate，不直接写入 `DependencyEdge`：

```go
type clientCallCandidate struct {
    CallerModule string
    Protocol     domain.EdgeType
    Client       string
    Target       valueExpr
    Method       valueExpr
    Path         valueExpr
    RPCService   valueExpr
    RPCMethod    valueExpr
    Evidence     domain.Evidence
    Confidence   float64
}
```

解析状态允许分级投影：

| 已解析信息 | 可产生的事实 |
|------------|--------------|
| target service、method、path 全部确定 | 服务级依赖；未来可关联到具体 HTTP endpoint |
| 只有 target service 确定 | 服务级 `DependencyEdge` |
| RPC service/method 确定，但网络 target 未确定 | 保留 candidate，等待 provider/interface 关联 |
| target 也未确定 | 不写入 dependency，保留诊断信息 |

### 7.3 HTTP client

每个 HTTP client 使用独立 adapter，例如 Java `RestTemplate`/`WebClient`、Go `net/http`/Resty、Python requests/httpx。框架身份仍然需要精确 import、类型或 receiver 来源，不能只在文件中搜索 client 名称。

RestTemplate 属于 Spring Framework，Spring Boot 项目通常通过 Bean 使用。RestTemplate adapter 至少需要解释：

| 调用 | HTTP method 来源 |
|------|------------------|
| `getForObject`、`getForEntity` | `GET` |
| `postForObject`、`postForEntity` | `POST` |
| `put`、`delete` | 固定 method |
| `exchange(url, HttpMethod.X, ...)` | 第二个参数 |
| `execute(url, HttpMethod.X, ...)` | method 参数 |

URL 求值按以下顺序处理：

1. 求值字符串、常量、确定的字符串拼接和 URI builder；
2. 使用通用 config resolver 解析 `${service.url}` 等配置引用；
3. 将 `http://order-service/...`、`lb://order-service/...` 或 `@LoadBalanced` URI 的 authority 解析成服务候选；
4. 拆分 authority、path 和 query，保留 path template；
5. 使用 `target service + HTTP method + normalized path` 匹配目标服务的 endpoint。

`/users/42` 可以按路由模板规则匹配 `/users/{id}`。匹配存在多个同等候选时，不强行选择具体 endpoint，只保留服务级依赖。

现有 `scanJVMAndPythonDependencies` 只是在文件出现 RestTemplate/WebClient 等关键词后抓取 literal URL。它可以提供粗粒度服务依赖，但没有解析 receiver、HTTP method、path 或目标 endpoint，迁移时应由 client adapter 替换，而不是继续扩充 URL 正则。

现有 Feign 配置解析可以抽成通用 value/config resolver，供 RestTemplate、WebClient 和其他 client adapter 复用；不能让通用配置求值继续依赖 Feign 类型。

### 7.4 RPC client

RPC 没有统一的 HTTP method/path，必须按协议抽取 operation identity：

**gRPC**

- 从 `ManagedChannelBuilder.forAddress`、`grpc.Dial` 等调用解析网络 target；
- 从 generated stub/client 类型解析 proto service；
- 从 stub 方法调用解析 RPC method；
- 从 `.proto`、generated server registration 或实现类型识别 provider；
- 使用 `proto package + service + method` 作为 operation key。

**Dubbo**

- 从 `@DubboReference`、XML 或构造配置解析 interface FQN；
- 从 receiver 调用解析 interface method；
- 从 `@DubboService`、provider 配置或实现关系识别提供方；
- 使用 `interface FQN + method` 作为 operation key。

其他 RPC 框架各自增加 client/provider adapter，公共 dependency resolver 只处理 protocol、target、operation 和 evidence。

### 7.5 依赖关联规则

dependency resolver 按证据强度逐层关联：

1. 通过 ModuleCatalog 确认 caller service；
2. 通过 URI authority、服务发现名称、配置值或 RPC interface 确认 target service；
3. HTTP 使用 method/path 匹配 `EndpointRecord`；
4. RPC 使用 service/interface + method 匹配 provider operation；
5. 无法唯一确认具体 operation 时降级为服务级依赖；
6. target service 都无法确认时不进入结构化依赖图。

“降级为服务级依赖”只适用于 target service 已有明确证据的情况，不能把解析失败转换成猜测目标。

## 8. SQLite 兼容边界

### 8.1 第一阶段不改 schema

多框架 endpoint adapter 和服务级 HTTP/RPC dependency 都可以投影到现有结构：

- `endpoints` 保存 `service_key + method + path`；
- `dependencies` 保存 caller、target 和 protocol；
- `dependency_evidence` 保存源码或配置证据；
- framework 和 unresolved candidate 只保留在 indexer 内部。

因此第一阶段保持 `structureSchemaVersion = 4`。扫描后 endpoint/dependency 的**行数据可能增加或纠正**，但表、列、索引和 schema version 不变化。

当前 `endpoints` 的唯一键是 `service_key + method + path`。两个框架暴露同一个规范接口时会合并为一个 endpoint 事实，这是当前 domain 的预期；如果未来要求保留每个框架的独立来源，需要另建 endpoint evidence，而不是复制相同 endpoint。

当前 `dependencies` 的唯一粒度是 `caller + target + protocol`。同一个 caller 通过 RestTemplate 调用目标服务的多个接口时，会聚合为一条服务级 dependency，并在 `dependency_evidence` 中保留多个调用位置。

### 8.2 哪些需求需要扩表

| 需求 | 是否修改 SQLite |
|------|-----------------|
| 增加 Spring/JAX-RS/Gin/Flask adapter | 否 |
| 增加 HTTP、Feign、gRPC、Dubbo 服务级依赖 | 否 |
| 按 framework 查询 endpoint | 可选，需要 framework/evidence 字段或表 |
| 保存“调用方方法 → 目标 HTTP endpoint” | 是 |
| 保存 gRPC/Dubbo service/method | 是 |

如果产品需要回答“`A.foo` 调用了 `B` 的 `GET /users/{id}`”或“调用了哪个 gRPC method”，建议新增 operation 层，而不是把 HTTP/RPC 专属字段塞进 `dependencies`：

```text
operations
  operation_id
  service_key
  protocol
  operation_key
  http_method / http_path       nullable
  rpc_service / rpc_method      nullable

operation_calls
  call_id
  caller_service_key
  target_service_key            nullable
  target_operation_id           nullable
  protocol
  file_path / line / symbol
  confidence
```

`dependencies` 继续作为稳定的服务级聚合边，`operation_calls` 承载接口级调用。该需求确认后再将 schema version 升级；SQLite 是派生快照，可以按现有机制丢弃旧版本并全量重建，不需要在第一阶段提前迁移。

## 9. Service 与 Runtime 识别

服务发现和 endpoint 解析使用同一份 module ownership，但语义分开：

- `main`、`func main`、`__main__` 只说明存在入口，不说明具体框架；
- Spring Boot 需要 `@SpringBootApplication`、Spring import 或 `SpringApplication.run` 等证据；
- FastAPI/Flask 需要相应构造函数或启动调用证据；
- Python 不默认附加 `ai` 等业务标签；
- Go 版本信息和 HTTP 框架身份不要混写成同一个字段。

第一阶段保持现有 domain schema，先修正错误归类；框架作为 candidate/证据内部属性保留。

## 10. 迁移阶段

### 阶段 0：基线

- 保留现有测试并补充误报反例；
- 明确当前旧扫描器的输出快照；
- 不在生产路径同时合并新旧结果。

### 阶段 1：公共 candidate/projector

- 固化 `literal`、`configReference`、`unresolved` 语义；
- 将显式根路径转换为 `/`；
- 让 unresolved candidate 在投影前被丢弃；
- 加入重复 endpoint 的稳定去重。

### 阶段 2：Java

- 完成结构化 JVM 前端；
- Spring MVC 与 JAX-RS 各自接入；
- 通过现有 Spring 回归测试后删除旧 Java route regex。

### 阶段 3：Go

- package-level AST frontend；
- 先迁移 `net/http` 和 Gin；
- 再逐个加入其他框架，并为每个 adapter 建独立测试。

### 阶段 4：Kotlin

- 建立独立 Kotlin declaration/DSL frontend；
- 迁移 Spring、Ktor、Javalin；
- 将 Retrofit/OkHttp 迁移到 client candidate pipeline。

### 阶段 5：Python

- 完成通用 decorator/import frontend；
- FastAPI 与 Flask 并行验证；
- 删除旧 FastAPI-only route scanner。

### 阶段 6：C# 与 JavaScript/TypeScript

- C# 迁移 ASP.NET Core、Minimal API、ServiceStack 和 Refit；
- JS/TS 迁移 Express、Fastify、Koa、NestJS 和 Hapi；
- client adapter 同步抽取 HttpClient、axios、fetch 等调用。

### 阶段 7：移动端与其他语言

- Android 迁移 Retrofit/OkHttp/Ktor Client；
- Swift/Objective-C 迁移 URLSession/Alamofire/Moya；
- 根据真实项目需求选择 Rust、Dart、Ruby、PHP 等 adapter，不为语言名单本身制造空实现。

### 阶段 8：归属与服务识别

- 抽出 ModuleCatalog；
- 修正 runtime 和无依据标签；
- 验证同名服务跨 repo/module 的归属稳定性。

### 阶段 9：依赖解析

- endpoint 稳定后，再把 Feign、RestTemplate、WebClient、Go/Python HTTP client、gRPC、Dubbo 等迁移到独立 client candidate pipeline；
- 先投影现有服务级 `DependencyEdge`，不修改 SQLite；
- 接口级调用查询需求确认后，再设计 operation domain contract 和 schema v5；
- 不把某个框架的 client 规则塞回语言前端。

## 11. 测试与验收矩阵

| 类别 | 必测场景 |
|------|----------|
| 语法前端 | 多行调用、注释、raw/triple string、嵌套参数、import alias、局部遮蔽 |
| 框架证据 | 同名普通对象不识别；无 import 不识别；构造函数和 receiver 传播可识别 |
| 多框架共存 | Spring/JAX-RS、Gin/net/http、FastAPI/Flask 同项目工作 |
| 多语言共存 | Kotlin、C#、JS/TS 与 Java/Go/Python 在同一 workspace 中归属不串线 |
| 路径求值 | 字符串常量和拼接可解析；环境变量、格式化、分支合并为 unresolved |
| 根路径 | 明确空路径生成 `/`；未知路径不生成 `/` |
| method | 明确 `ANY` API 才生成 `ANY`；解析失败不回退 |
| 归属 | 多 repo、嵌套 module、无 runtime 的 library module 均可解析 endpoint |
| 投影 | unresolved candidate 不进入 `EndpointRecord`；重复 candidate 稳定去重 |
| HTTP client | RestTemplate 固定 method、`exchange`、配置 URL、load-balanced service name |
| RPC client | gRPC target/stub/method、Dubbo interface/method、未知 target 不误报 |
| 移动端 client | Retrofit/OkHttp、URLSession/Alamofire/Moya 只产生 client dependency |
| 未覆盖语言 | Rust/Dart/Ruby/PHP 等未注册 adapter 时只进入语义索引，不产生结构化路由 |
| 依赖关联 | 精确 endpoint、模板 path、歧义时只保留服务级依赖 |
| SQLite | 第一阶段 schema version 保持 4，新增框架只改变快照行数据 |

验证命令：

```bash
GOWORK=off go test ./internal/indexing/indexer
GOWORK=off go test ./...
GOWORK=off go vet ./...
```

## 12. 设计决策总结

1. 不做跨语言统一 AST，只统一 candidate 和投影契约。
2. 不凭变量名、方法名、文件名猜框架。
3. 不把 unresolved 值转换成根路径或 `ANY`。
4. 不在第一阶段修改持久化 schema。
5. 新框架通过独立 adapter 扩展，语言前端保持框架无关。
6. 先完成 endpoint 事实链路，再迁移 service/runtime 和 dependency 解析。
7. 服务端 endpoint adapter 和客户端 call adapter 使用同一语言 IR，但职责分离。
8. 第一阶段复用现有 SQLite schema，只保存服务级 dependency。
9. 只有接口级 HTTP/RPC 调用需要持久化时，才引入 operation 层并升级 schema。
