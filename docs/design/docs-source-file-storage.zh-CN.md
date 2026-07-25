# Docs 源文件存储设计

## 1. 状态与范围

- 状态：设计稿
- 范围：Docs 源文件的保存、可访问性校验、读取、重新解析和删除
- 支持：本地 workspace、S3/MinIO、阿里云 OSS
- 核心原则：系统只配置一个当前存储，一份文档只写入一个位置

`documents.content` 继续保存标准化后的 UTF-8 Markdown，并作为文档详情、切块、Embedding 和检索的输入。源文件独立保存，用于下载、重新解析和解析器升级。

## 2. 设计约束

1. 存储配置只存在于应用配置文件或对应环境变量中，不允许用户在数据库中维护多套存储配置。
2. 运行实例同一时刻只有一个当前存储提供方。
3. `document_sources` 不保存 `storage_config_id`，只保存文件类型、位置和上传时的可访问性校验结果。
4. 本地文件固定保存到 `<workspace>/document/asserts`，并按日期分目录。
5. 云端保存稳定对象 URL，不保存会过期的 presigned URL。
6. 切换配置只影响切换后的上传，不迁移、不复制历史文件。
7. 云端源文件不随 Docs 文档删除而物理删除；本地源文件可以删除。
8. 配置错误或存储不可达必须明确报错，禁止静默切换提供方。

## 3. 目标与非目标

### 3.1 目标

- 保存 Markdown、PDF、DOCX、粘贴文本、URL 导入和系统生成内容的源文件。
- 每次上传完成前验证最终路径确实可读取。
- 本地路径在容器重启后仍可通过 workspace 挂载解析。
- 支持 S3、MinIO 和 OSS 的单提供方部署。
- 上传和读取流式处理，并设置明确的资源上限。

### 3.2 非目标

- 不保存多套用户存储配置。
- 不通过数据库保留历史云 endpoint 或历史凭据。
- 不保证云存储配置切换后，旧云文件仍能被当前实例读取。
- 不支持同一文件同时写入多个提供方。
- 不设计存储迁移、复制或自动回填到新提供方。
- 首期不永久保存 PDF/DOCX 中提取出的每一张图片。

## 4. 当前存储配置

存储配置由应用配置入口一次性规范化，运行时业务代码直接信任配置不变量。配置结构只表达一个当前提供方，不使用配置数组。

本地示例：

```yaml
document_storage:
  type: local
```

S3 或 MinIO 示例：

```yaml
document_storage:
  type: s3
  endpoint: https://minio.example.com
  region: us-east-1
  bucket: docs
  prefix: documents
  path_style: true
  access_key: ${DOCUMENT_STORAGE_ACCESS_KEY}
  secret_key: ${DOCUMENT_STORAGE_SECRET_KEY}
```

OSS 示例：

```yaml
document_storage:
  type: oss
  endpoint: https://oss-cn-hangzhou.aliyuncs.com
  region: cn-hangzhou
  bucket: docs
  prefix: documents
  access_key: ${DOCUMENT_STORAGE_ACCESS_KEY}
  secret_key: ${DOCUMENT_STORAGE_SECRET_KEY}
```

实际键名应在实现时遵循 Nasuta 的配置命名规范。密钥只能通过环境变量或密钥挂载注入，不能写入数据库、日志或前端响应。

启动校验包括：

- `type` 只能是 `local`、`s3`、`oss`。
- 本地模式不接受云端专用字段。
- S3/OSS 必须提供 endpoint、bucket 和凭据。
- prefix 规范化后不能以 `/` 开头，也不能包含目录穿越片段。
- 显式配置的存储初始化失败时，相关上传能力不可用并记录明确错误。

## 5. 源文件定位模型

每份文档只保存以下定位信息：

```text
storage_type  local | s3 | oss
source_path   workspace 相对路径或稳定对象 URL
```

示例：

```text
local  document/asserts/2026/07/26/aaa-01j3....pdf
s3     https://minio.example.com/docs/documents/2026/07/26/aaa-01j3....pdf
s3     https://docs.s3.us-east-1.amazonaws.com/documents/2026/07/26/aaa-01j3....pdf
oss    https://docs.oss-cn-hangzhou.aliyuncs.com/documents/2026/07/26/aaa-01j3....pdf
```

`source_path` 在上传成功后不可修改。本地保存相对路径，避免绑定机器绝对路径；云端保存不带临时签名参数的稳定 URL。

数据库不保存 `is_accessible`。可访问性是实时属性，持久化布尔值会很快失真。系统只记录最近一次成功校验时间 `access_verified_at`，并在每次实际读取失败时返回当前错误。

## 6. 日期目录与文件名

本地根目录固定为：

```text
<workspace>/document/asserts
```

本地物理路径格式为：

```text
<workspace>/document/asserts/YYYY/MM/DD/<filename>
```

数据库保存的相对路径为：

```text
document/asserts/YYYY/MM/DD/<filename>
```

例如：

```text
<workspace>/document/asserts/2026/07/26/aaa-01j3....pdf
```

规则如下：

1. 日期取服务端统一配置时区中的上传时间，同一次上传只计算一次。
2. 文件名保留清洗后的原始主文件名，并追加文档 ID 或同等强度的唯一后缀。
3. 扩展名规范为小写，原始文件名单独保存到 `original_filename`。
4. 去除路径分隔符、控制字符和 `.`、`..` 等危险片段，客户端不能决定目录。
5. 云端 object key 使用相同的 `documents/YYYY/MM/DD/<filename>` 日期规则。
6. 写入使用不覆盖语义；若唯一后缀仍发生冲突，则重新生成后重试。

## 7. 存储接口与分发

存储能力保持最小接口：

```go
type SourceStore interface {
	Put(ctx context.Context, objectKey string, body io.Reader, size int64, contentType string) (string, error)
	Open(ctx context.Context, sourcePath string) (io.ReadCloser, error)
	Stat(ctx context.Context, sourcePath string) (SourceInfo, error)
}
```

`Put` 返回最终 `source_path`，`Stat` 用于确认对象存在、大小正确且当前配置可访问。云端物理删除不属于通用接口；本地文件删除由本地实现单独提供。

提供方通过一个显式 dispatcher 分发：

```text
local -> put/open/statLocal
s3    -> put/open/statS3
oss   -> put/open/statOSS
其他值 -> 明确报错
```

S3 和 MinIO 共享 S3 协议实现，OSS 使用独立实现。不得在某个提供方失败后尝试其他提供方。

## 8. 上传与可访问性校验

上传流程：

1. 在 HTTP 入口规范化文件名、MIME、扩展名和大小。
2. 读取唯一的当前存储配置，生成日期 object key。
3. 流式写入当前存储，同时计算 SHA-256 和实际字节数。
4. 使用同一个 provider 对返回的 `source_path` 执行 `Stat`。
5. 校验对象大小与实际上传大小一致；需要时抽样打开文件验证读取权限。
6. 解析源文件并生成规范化 Markdown。
7. 在数据库事务中写入 `documents`、`document_sources` 和索引任务。

只有第 4、5 步成功，上传才算成功，`access_verified_at` 记录此次校验时间。校验失败时返回明确的 provider、路径和原因，不创建可见文档记录。

上传入口必须使用 `io.LimitReader` 或等价机制设置硬上限，禁止无上限 `io.ReadAll`。同时校验客户端声明大小、实际写入大小和存储端返回大小。

数据库事务失败时，可以在当前请求仍持有当前存储客户端期间清理本次新写入对象。该补偿只处理尚未形成有效文档的上传，不改变“云端文档源文件不随 Docs 文档删除”的语义。

## 9. 本地存储

本地写入流程：

1. 生成 `document/asserts/YYYY/MM/DD/<filename>` 相对路径。
2. 将相对路径与规范化后的 `WorkspaceRoot` 安全拼接。
3. 确认最终路径仍位于 `<workspace>/document/asserts` 内。
4. 在目标目录创建临时文件，流式写入并计算 SHA-256。
5. 同目录原子重命名为最终文件名。
6. 重新打开最终文件完成可访问性校验。

读取时使用 `<workspace>/<source_path>`。本地根目录固定，因此当前云配置如何变化都不影响本地历史文件的定位。

## 10. S3 与 MinIO

`s3` 提供方同时覆盖 AWS S3 和兼容 S3 API 的 MinIO。`Put` 完成后生成不带签名参数的稳定 HTTPS 对象 URL，`Stat` 使用当前 S3 配置和签名请求确认对象可访问。

私有 bucket 不得为了得到可直接访问 URL 而改成公开读。下载时使用当前凭据读取并由 Docs API 流式返回；如需临时直链，在请求时动态签发，不能覆盖数据库中的稳定 `source_path`。

## 11. 阿里云 OSS

OSS 使用独立的 `oss` provider 和 SDK，不通过 S3 实现伪装。`Put` 返回稳定 OSS 对象 URL，`Stat` 使用当前 OSS 配置验证对象存在、大小正确且可读取。

OSS 故障必须直接暴露，不允许静默改用 S3 或本地存储。

## 12. 数据模型

只新增源文件表，不新增存储配置表：

```sql
CREATE TABLE document_sources (
  document_id         VARCHAR(64)   NOT NULL,
  storage_type       VARCHAR(16)   NOT NULL,
  source_path        VARCHAR(2048) NOT NULL,
  original_filename  VARCHAR(512)  NOT NULL,
  source_origin      VARCHAR(32)   NOT NULL,
  mime_type          VARCHAR(255)  NOT NULL,
  extension          VARCHAR(32)   NOT NULL,
  sha256             CHAR(64)      NOT NULL,
  size_bytes         BIGINT        NOT NULL,
  parser_version     VARCHAR(64)   NULL,
  source_url         VARCHAR(2048) NULL,
  access_verified_at DATETIME(6)   NOT NULL,
  created_at         DATETIME(6)   NOT NULL,
  updated_at         DATETIME(6)   NOT NULL,
  PRIMARY KEY (document_id),
  CONSTRAINT fk_document_sources_document
    FOREIGN KEY (document_id) REFERENCES documents(id),
  CONSTRAINT chk_document_sources_storage
    CHECK (storage_type IN ('local', 's3', 'oss')),
  CONSTRAINT chk_document_sources_size CHECK (size_bytes >= 0)
);
```

`source_origin` 使用 `upload`、`paste`、`url_import`、`generated`、`legacy_content`。它只描述源文件如何产生，不参与存储路由。

表中不包含：

- `storage_config_id`
- endpoint、bucket、region 或凭据
- 容易失真的 `is_accessible` 布尔状态

## 13. 配置切换后的行为

配置文件变更并重启后，新上传只使用新的当前配置。历史 `document_sources` 不修改，也不会被复制到新存储。

读取规则：

```text
local
  -> 始终按 WorkspaceRoot + source_path 读取

s3 / oss
  -> source_path 的 provider 与当前配置一致时，使用当前凭据读取
  -> provider 不一致或当前凭据无权访问时，返回源文件不可用
```

系统不保存历史 endpoint 和凭据，因此不能承诺切换后继续读取旧云对象。稳定对象 URL 仍保留在数据库中，但其实际可访问性取决于当前网络、对象是否存在以及当前配置是否拥有权限。

如果业务未来重新要求“切换后旧云文件必须可读”，就必须在“保留历史配置”或“迁移旧文件”中选择一种；仅保存 URL 无法为私有对象恢复旧凭据。该能力不属于本设计。

## 14. 各类来源处理

| 来源 | 保存的源文件 | `source_origin` |
|---|---|---|
| Markdown 上传 | 原始 `.md` | `upload` |
| PDF/DOCX 上传 | 原始二进制文件 | `upload` |
| 粘贴文本 | 生成 UTF-8 `.md` | `paste` |
| URL 导入 | 原始响应体，保留识别出的类型 | `url_import` |
| 系统生成 Markdown | 生成的 `.md` | `generated` |
| 历史文档回填 | 从 `documents.content` 生成 `.md` | `legacy_content` |

URL 导入额外保存原始 URL 到 `source_url`。它描述内容来源，不替代源文件的 `source_path`。

## 15. PDF、DOCX 与图片

PDF/DOCX 原文件永久保存在上传时的目标位置。解析时将源文件流式写入受控临时目录，完成文本和图片提取后删除临时文件。

首期图片策略：

1. 提取出的图片仅用于本次解析、OCR 或多模态理解。
2. 不为每张图片创建永久对象，也不将图片二进制写入数据库。
3. Markdown 中不得写入即将失效的临时文件路径。
4. 后续若需要文档内图片预览，应另行设计图片资产表和稳定引用。

源 PDF/DOCX 保留后，未来仍可使用新解析器重新提取图片。

## 16. 下载、重新解析与删除

### 16.1 下载

下载接口读取 `document_sources`：

- 本地文件按固定 workspace 根目录打开。
- 云文件仅在当前 provider 和凭据可以访问该 URL 时打开。
- 下载响应使用 `original_filename`，不从物理路径反推展示名称。

每次读取失败都返回实时错误，不使用 `access_verified_at` 代替实际访问。

### 16.2 重新解析

重新解析从 `source_path` 读取源文件。成功后更新 `documents.content`、`parser_version`、切块和向量索引，但不修改源文件位置。无法访问历史云文件时明确失败，不回退到其他存储。

### 16.3 删除

删除 Docs 文档时：

- 本地源文件可以随文档删除。
- 云端源文件不执行物理删除，只删除 Docs 数据、切块和向量索引。

本地删除采用数据库事务内写删除 outbox、事务外删除文件的方式，避免数据库与文件系统不一致。对象不存在视为幂等成功，其他错误按退避策略重试。

## 17. 本地删除 Outbox

删除 outbox 只承担本地文件清理，不保存云配置：

```sql
CREATE TABLE document_source_delete_outbox (
  id               BIGINT        NOT NULL AUTO_INCREMENT,
  document_id      VARCHAR(64)   NOT NULL,
  source_path      VARCHAR(2048) NOT NULL,
  status           VARCHAR(16)   NOT NULL,
  attempt_count    INT           NOT NULL DEFAULT 0,
  next_attempt_at  DATETIME(6)   NOT NULL,
  last_error       TEXT          NULL,
  created_at       DATETIME(6)   NOT NULL,
  updated_at       DATETIME(6)   NOT NULL,
  PRIMARY KEY (id),
  KEY idx_document_source_delete_due (status, next_attempt_at)
);
```

任务按有界批次领取。worker 只接受 `document/asserts/` 下经过边界校验的相对路径，拒绝其他目标。

## 18. 安全与资源限制

1. 所有外部文件名、MIME、扩展名和 URL 在入口只规范化一次。
2. 本地路径必须经过 `filepath.Clean` 和根目录边界检查，拒绝绝对路径、软链接逃逸和目录穿越。
3. 云端上传返回的 URL 必须匹配当前配置的 endpoint、bucket 和 object key，不能接受客户端提供的对象 URL。
4. 下载权限沿用 Docs 访问控制，不能因数据库保存了 URL 而绕过鉴权。
5. 云 bucket 默认私有，临时下载 URL 必须短时有效。
6. 日志不得输出凭据、签名参数或完整敏感 URL 查询串。
7. 限制单文件大小、解析文本大小、压缩包展开大小、页数、图片数量、图片像素和解析时长。
8. 校验文件签名与声明类型，DOCX 必须防止 Zip Slip 和压缩炸弹。
9. SHA-256 用于完整性校验和审计，不默认用于跨文档复用或删除对象。

## 19. 历史数据回填

历史 `documents` 没有源文件时，从 `documents.content` 生成 UTF-8 Markdown，并写入执行回填时的当前存储：

```text
source_origin = legacy_content
original_filename = <document-title>.md
```

回填任务按主键游标分页，单批有界，逐条执行上传和 `Stat` 校验。已有 `document_sources` 的文档跳过，失败记录原因并允许继续。

## 20. 测试与验收标准

### 20.1 单元测试

- 配置入口只接受一个当前 provider，并拒绝不完整配置。
- 日期目录在月末、年末和指定时区下正确生成。
- 同名上传生成不同路径，原始文件名仍可用于下载。
- 路径穿越、绝对路径和软链接逃逸被拒绝。
- dispatcher 对 `local`、`s3`、`oss` 路由正确，对未知类型明确报错。
- `Put` 后 `Stat` 失败时不创建可见文档。
- 云 URL 不接受 presigned 参数或与当前 endpoint/bucket 不一致的地址。
- 文件大小边界、哈希和流式复制行为正确。

### 20.2 集成测试

- 本地文件写入 `workspace/document/asserts/YYYY/MM/DD`，重启后仍可下载和重新解析。
- S3、MinIO、OSS 分别完成上传、`Stat`、下载和失败反馈。
- 配置切换后新文件只进入新存储，不复制或修改历史记录。
- 历史云文件与当前配置不匹配时返回明确不可用错误。
- 删除本地文档会清理本地文件，删除云文档不会删除云对象。
- PDF/DOCX 解析完成后临时文件被清理，源文件保持可用。

### 20.3 验收结论

满足以下条件即完成：

1. 系统不创建存储配置表，也不在数据库保存云凭据。
2. `document_sources` 不包含 `storage_config_id`。
3. 本地路径符合 `workspace/document/asserts/YYYY/MM/DD/<filename>`。
4. 每次上传只有通过最终路径可访问性校验后才成功。
5. 配置切换只影响新上传，不迁移、不复制历史文件。
6. 云文件不随 Docs 删除，本地文件可通过受控 outbox 删除。
