# Package Layout

`Astris` 核心模块（Go module: `github.com/dekwanlabs/astris`）的边界约定：

- `application`：标准发行版和场景发行版共用的装配入口。
- `knowledge`：给场景层和工具层消费的只读查询契约。
- `tool`：工具注册、快照、执行等稳定扩展契约。
- `config`：环境配置和平台设置的公共入口。
- `auth`：场景层确实需要接入的身份与会话契约。
- `internal`：索引、检索、记忆、存储、传输等实现细节，不对外承诺兼容。

判断规则很简单：

- 其他仓库会 import 的，优先放到顶层稳定包。
- 只有 core 自己装配会依赖的，放进 `internal/`。
- 需要复用但还不适合承诺长期兼容的辅助实现，先留在现有实现包里，等边界再明确后继续收口。
