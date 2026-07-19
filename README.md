# Nasuta Core

这是 Nasuta 的独立核心 Go module：

```text
github.com/dekwanlabs/nasuta
```

目标分层是：

```text
Nasuta/
├── app/           对外装配 API 与标准发行版
├── cmd/nasuta/    独立启动入口
├── knowledge/     对外查询契约
├── tool/          对外工具扩展契约
├── config/        对外配置契约
├── internal/      不承诺兼容的实现
└── docs/
```

外部依赖方优先使用上面的稳定入口；`internal/` 下的包只服务于 core 自身装配，不作为兼容承诺。例如：

```go
import (
    "github.com/dekwanlabs/nasuta/app"
    "github.com/dekwanlabs/nasuta/config"
    "github.com/dekwanlabs/nasuta/knowledge"
    "github.com/dekwanlabs/nasuta/tool"
)
```

`llm`、`log`、`platform/httpclient`、`platform/httputil`、`writeaction`
这些包目前仍然保留，是给场景层和适配层复用的辅助能力；但业务实现、检索、
索引、传输编排都应继续往 `internal/` 收口。身份认证（`internal/auth`）属于
平台内部装配能力：上层通过 `app.Extension` 拿到的是已套好鉴权边界的 `APIRegistrar`，
不需要也不暴露 auth 句柄。

发布前，旁边的 `codeloom` 项目通过 `go.work` 和本地 `replace` 联调。为本项目创建独立
Git 仓库、推送并打 tag 后，依赖方删除本地 `replace`，改用正式版本：

```bash
go get github.com/dekwanlabs/nasuta@v0.1.0
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

## 独立启动

直接运行时，使用 `NASUTA_*` 配置 Nasuta；已有上层应用的 `CODELOOM_*` 环境变量仍兼容：

```bash
GOWORK=off go run ./cmd/nasuta
```

Docker Compose 自带 Qdrant，并默认把 Nasuta 连接到 `qdrant:6334`。上层环境变量或同目录
`.env` 会覆盖这些默认值：

```bash
cp .env.example .env
docker compose up --build
```

语义存储统一使用一组 Provider 配置；默认 Compose 无需配置，切换到已有 Milvus 时覆盖：

```bash
SEMANTIC_PROVIDER=milvus \
SEMANTIC_ENDPOINT=milvus.example:19530 \
SEMANTIC_COLLECTION=knowledge \
docker compose up --build
```

直接运行 `cmd/nasuta` 时必须提供有效的 `SEMANTIC_PROVIDER` 和 `SEMANTIC_ENDPOINT`；
`SEMANTIC_COLLECTION` 默认 `knowledge`。旧 `QDRANT_HOST`、`QDRANT_PORT`、`QDRANT_API_KEY`、
`QDRANT_COLLECTION` 仍会在配置入口规范化为 Qdrant；不会静默禁用语义存储或自动换用另一后端。

服务启动后，MCP 地址为 `http://localhost:8201/mcp`，Dashboard API 位于
`http://localhost:8201/api/`。默认扫描宿主机的 `./workspace`，可通过
`NASUTA_WORKSPACE_PATH` 修改。Embedding 和 MySQL 没有凭据时按能力边界禁用，已显式
配置的后端发生错误时不会静默切换到其他 Provider。
