# Astris Core

这是 Astris 的独立核心 Go module：

```text
github.com/dekwanlabs/astris
```

目标分层是：

```text
astris/
├── application/   对外装配 API
├── knowledge/     对外查询契约
├── tool/          对外工具扩展契约
├── config/        对外配置契约
├── auth/          场景确实需要的身份契约
├── internal/      不承诺兼容的实现
└── docs/
```

外部依赖方优先使用上面的稳定入口；`internal/` 下的包只服务于 core 自身装配，不作为兼容承诺。例如：

```go
import (
    "github.com/dekwanlabs/astris/application"
    "github.com/dekwanlabs/astris/auth"
    "github.com/dekwanlabs/astris/config"
    "github.com/dekwanlabs/astris/knowledge"
    "github.com/dekwanlabs/astris/tool"
)
```

`llm`、`log`、`platform/httpclient`、`platform/httputil`、`writeaction`
这些包目前仍然保留，是给场景层和适配层复用的辅助能力；但业务实现、检索、
索引、传输编排都应继续往 `internal/` 收口。

发布前，旁边的 `codeloom` 项目通过 `go.work` 和本地 `replace` 联调。为本项目创建独立
Git 仓库、推送并打 tag 后，依赖方删除本地 `replace`，改用正式版本：

```bash
go get github.com/dekwanlabs/astris@v0.1.0
```

如果依赖方需要提交依赖快照或离线构建，由依赖方自己生成 `vendor/`：

```bash
go mod vendor
go build -mod=vendor ./...
```

不要把本项目本身“发布到 vendor”。Go module 的发布源是 Git 仓库和语义化 tag；
`vendor/` 只是每个依赖项目自己的副本。

独立验证：

```bash
GOWORK=off go build ./...
GOWORK=off go test ./...
```
