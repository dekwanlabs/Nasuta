# Indexing, Ontology, and Source-file Storage

[English](05-index-ontology-and-storage.md) | [中文](05-index-ontology-and-storage.zh-CN.md)

> Status: core storage and ontology direction established; migration and provider adapters are delivered in slices
> Sources: Agent Index Storage Design, Nasuta Ontology Refactor, Docs Source-file Storage

## 1. Unified Data Plane

Nasuta uses three coordinated but distinct planes:

| Plane | Responsibility |
|---|---|
| Structure Store | Auditable records for services, repositories, files, symbols, APIs, docs, and tasks |
| Semantic Store | Dense/sparse chunk representations and retrieval payloads |
| Ontology Snapshot | Stable entities and traversable relations such as `depends_on` |

They share stable identities. The structure store is authoritative; semantic indexes and ontology snapshots are rebuildable projections. There is no second business dependency graph.

## 2. Stable Identity

Identity derives from canonical facts such as repository + relative path, logical service name, language + qualified symbol, method + normalized route, or document source + version + chunk locator.

Canonicalization occurs once at ingress. Downstream code does not repeat trimming, lowercasing, path cleanup, or legacy defaults. Deduplication and joins use stable IDs instead of display names.

## 3. Indexing Pipeline

```text
workspace/source
  -> discover
  -> parse/extract
  -> canonical entities and chunks
  -> structure transaction
  -> semantic batch upsert
  -> ontology projection
  -> atomic snapshot publish
```

Requirements:

- bounded scanning and reads;
- batch writes rather than N+1 calls;
- temporary generations and atomic publication;
- interruption cannot expose a half-written vocabulary or ontology snapshot;
- configured backend failure stays visible;
- generation/version cleanup removes stale projections after delete or rename.

## 4. Structure Store

The structure store records stable ID, type, source/version, structural attributes, source locator, content digest, indexing generation, timestamps, provenance, and parse status.

Online queries select only required columns and use `LIMIT` or cursors. Rebuild workflows that genuinely need all data use explicitly named full-read APIs.

## 5. Semantic Store

Payloads include stable entity/chunk ID, source type and locator, service/repository ownership, section/symbol metadata, index generation, and permission scope.

Dense and sparse representations may be produced independently, but every hit resolves to the same structure record. Missing embeddings disable only semantic capability; a configured semantic backend failure is not silently hidden by BM25.

## 6. Ontology

The first ontology stays intentionally small:

- entities: Service, Repository, File, Symbol, API, Document, Config, and similar stable types;
- relations: contains, defines, exposes, calls, depends_on, documents, configured_by;
- relations carry provenance, confidence, and source locator;
- only real facts are persisted; derivable workflow labels are not additional state.

Dependency QA and `trace_deps` share one Ontology Repository.

## 7. Docs Source-file Storage

```text
source object
  -> immutable storage key
  -> metadata record
  -> parser/extractor
  -> normalized text/chunks
  -> semantic and ontology projections
```

Untrusted filenames never directly form storage keys. Limits cover file and upload size, archive expansion and depth, page/image/pixel counts, parse duration, MIME/signature consistency, Zip Slip, and decompression bombs.

Downloads are authenticated and content-type controlled. Index deletion and source-object retention are separate policies.

## 8. Providers and Migration

Storage and semantic backends use explicit dispatch:

```text
provider config
  -> one switch
  -> provider-specific constructor
  -> clear unavailable/error result
```

Migration sequence:

1. introduce the new schema;
2. batch backfill stable IDs and generations;
3. use dual-write only for a defined migration window;
4. verify counts, references, and retrieval consistency;
5. switch reads;
6. remove the old path.

Permanent read-time compatibility cleanup is prohibited.

## 9. Acceptance Criteria

1. Every retrieved chunk resolves to a structure record and source object.
2. Failed rebuilds never publish incomplete snapshots.
3. One ontology repository owns dependency facts.
4. Stable IDs make repeated builds idempotent.
5. Online reads are bounded at storage.
6. Uploads resist path traversal and resource bombs.
7. Configured-backend failure never triggers silent provider substitution.

## Detailed Consolidated Material

### Agent Index Storage Design

> Migrated from CodeLoom `docs/design/agent/agent-index-storage-design.md`; incorporated into this module on 2026-07-31.

Status: Proposal, not implemented

This document redesigns the CodeGraph and CodeLoom structured SQLite stores used by the Internal evidence path. The project is not in production, so the target implementation does not migrate old tables or preserve compatibility fields. Existing derived databases are rebuilt from source.

#### 1. Goals And Scope

The proposal addresses four problems:

1. CodeGraph symbol search performs `%LIKE%` table scans and incorrectly queries service names as symbol names.
2. The structure store duplicates scalar columns in full-object JSON, duplicates repository revision state, and collapses multi-service repositories to one service.
3. Dependency rows combine a logical edge with only its first evidence item and lack caller and target indexes.
4. Incremental cleanup relies on unreliable repository values and path guesses, leaving stale data and read-time compatibility logic.

This document does not redesign MySQL, Qdrant, or the complete external CodeGraph provider model. It defines their ownership boundary, the target CodeLoom structure model, and the read contract CodeGraph must satisfy.

#### 2. Storage Ownership

```text
Workspace source
  ├─ CodeGraph provider DB
  │    files / nodes / edges / nodes_fts
  │    symbols, source locations, and symbol relationships
  ├─ CodeLoom structure DB
  │    repositories / services / endpoints
  │    dependencies / dependency_evidence
  │    services, APIs, and service-level dependencies
  ├─ Qdrant
  │    semantic vectors for code, documents, and long-term memory
  └─ MySQL
       users, settings, documents, sessions, traces, memory, and incidents
```

The external provider produces the CodeGraph database. CodeLoom reads it without copying its physical model. The CodeLoom structure database is its own derived read model: `internal/indexing` produces it and `internal/platform/store` adapts it.

Each fact has one owner:

- Repository revision lives only in `repositories.head_sha`.
- Service ownership is represented by `services.repo + services.module_path`.
- Upstream and downstream relationships are derived from `dependencies`, not copied into service JSON.
- Runbook content and metadata belong to the MySQL document store; semantic recall belongs to Qdrant.
- Method-level calls belong to CodeGraph; service-level dependencies belong to the structure store.

#### 3. Current Database Audit

##### 3.1 CodeGraph Provider DB

| Table | Purpose and data | Current indexes | Assessment |
| --- | --- | --- | --- |
| `files` | Paths, content hashes, language, size, timestamps, node counts, parse errors | Primary key `path`, `language`, `modified_at` | Sound; supports file existence and index health |
| `nodes` | Symbol IDs, kinds, names, qualified names, locations, signatures, and language attributes | Primary key `id`; `kind`, `name`, `qualified_name`, `file_path`, `(file_path,start_line)`, and others | Sound model; `%LIKE%` cannot use the name indexes |
| `edges` | Symbol relationships with source, target, kind, location, provenance, and metadata | `(source,kind)`, `(target,kind)`, `kind`, `provenance` | Matches caller/callee access paths |
| `nodes_fts` | FTS5 index over name, qualified name, docstring, and signature | FTS5 internals and triggers synchronized with `nodes` | Already available but unused by CodeLoom; should be the symbol-search entry point |
| `unresolved_refs` | References not yet resolved to a target node and their candidates | `from_node_id`, `reference_name`, composite and file-path indexes | Provider diagnostics/post-processing data; not a CodeLoom dependency |
| `project_metadata` | Provider project-level key/value metadata | Primary key `key` | Provider-owned metadata |
| `schema_versions` | Provider schema revision history | Primary key `version` | Provider-owned migration metadata |
| `nodes_fts_*` | SQLite-managed FTS5 shadow tables | SQLite internals | Not business tables; CodeLoom must not query them directly |
| `sqlite_sequence`, `sqlite_stat1` | SQLite autoincrement and planner state | SQLite-managed | Not business tables |

The local sample contains about 435,000 nodes and 1.57 million edges. A `name LIKE '%x%'` lookup took about 3.1 to 3.7 seconds, while the existing FTS lookup took about 0 to 10 ms. These measurements identify the current bottleneck; they are not a cross-machine SLA.

##### 3.2 CodeLoom Structure DB

| Current table | Original purpose | Problem | Target action |
| --- | --- | --- | --- |
| `repos` | Repository and indexing timestamp | `last_commit` duplicates `repo_index_state.head_sha` and is empty in the sample | Replace with `repositories` |
| `repo_index_state` | Repository SHA | Splits one lifecycle across two tables | Delete and merge into `repositories` |
| `services` | Service structure records | Scalar columns duplicate full `json`; `module_path` is only in JSON | Rebuild as normalized `services` |
| `endpoints` | API routes | Duplicates repo, service name, and full JSON; fuzzy service lookup misses indexes | Reference `service_key` |
| `dependency_edges` | Service dependencies and first evidence | Drops additional evidence; lacks caller/target indexes; contains empty repos; `interface_method` has no useful data | Split dependencies from evidence |
| `feign_edges` | Legacy Feign-specific relationships | Superseded by generic dependencies but still populated | Delete |
| `runbooks` | Legacy runbook structure data | Ownership moved to the MySQL document store and Qdrant | Delete |
| `sqlite_sequence` | SQLite autoincrement state | Not a business table | SQLite-managed |

The structure database is currently about 5 MB, so full snapshot rebuilds are simpler and safer than row migrations and permanent compatibility reads.

#### 4. Target Structure Store

##### 4.1 Relationship Model

```text
repositories 1 ─── n services 1 ─── n endpoints
                         │
                         ├── 1 ─── n dependencies (caller)
                         │                │
                         │                └── 1 ─── n dependency_evidence
                         │
                         └── 0 ─── n dependencies (service target)
```

The target has five business tables. The indexing ingress canonicalizes and validates every string, including case and paths. Store reads do not repair persisted data.

##### 4.2 `repositories`

One row represents a canonical repository and its current structural-index revision.

```sql
CREATE TABLE repositories (
  repo       TEXT PRIMARY KEY,
  head_sha   TEXT NOT NULL,
  indexed_at INTEGER NOT NULL
) WITHOUT ROWID;

CREATE INDEX idx_repositories_indexed_at
  ON repositories(indexed_at DESC);
```

- `repo` is a canonical repository identifier relative to the workspace, such as `group/name`, without a `repos/` prefix.
- `head_sha` identifies the indexed source snapshot. Inputs without a revision fail before writing; an empty string does not mean unknown.
- `indexed_at` is Unix milliseconds, avoiding textual timestamp variations.

##### 4.3 `services`

One row represents an independently identifiable service or runtime module. A repository can contain multiple services.

```sql
CREATE TABLE services (
  service_key          TEXT PRIMARY KEY,
  repo                 TEXT NOT NULL REFERENCES repositories(repo) ON DELETE CASCADE,
  module_path          TEXT NOT NULL,
  service_name         TEXT NOT NULL,
  layer                TEXT NOT NULL,
  language             TEXT NOT NULL,
  runtime              TEXT NOT NULL DEFAULT '',
  scope                TEXT NOT NULL DEFAULT '',
  owner                TEXT NOT NULL DEFAULT '',
  status               TEXT NOT NULL DEFAULT '',
  summary              TEXT NOT NULL DEFAULT '',
  confidence           REAL NOT NULL CHECK (confidence >= 0 AND confidence <= 1),
  tags_json            TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(tags_json)),
  docs_json            TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(docs_json)),
  source_of_truth_json TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(source_of_truth_json)),
  entrypoints_json     TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(entrypoints_json)),
  ports_json           TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(ports_json)),
  UNIQUE (repo, module_path)
);

CREATE INDEX idx_services_name
  ON services(service_name);
CREATE INDEX idx_services_repo_name
  ON services(repo, service_name);
```

- `service_key` is deterministically derived from canonical `repo + module_path`; callers do not assemble its representation.
- `module_path` is relative to the repository root; `.` denotes the root module instead of an empty string.
- JSON is limited to bounded arrays that are not SQL predicates or relationships. There is no full `ServiceRecord` JSON.
- Upstreams and downstreams are derived from dependency queries.
- A repository module owns at most one service. Documentation and code-scan candidates are merged at indexing ingress before persistence.

##### 4.4 `endpoints`

One row represents one canonical API exposed by a service.

```sql
CREATE TABLE endpoints (
  endpoint_id     INTEGER PRIMARY KEY,
  service_key    TEXT NOT NULL REFERENCES services(service_key) ON DELETE CASCADE,
  method         TEXT NOT NULL,
  path           TEXT NOT NULL,
  handler        TEXT NOT NULL DEFAULT '',
  handler_method TEXT NOT NULL DEFAULT '',
  file_path      TEXT NOT NULL,
  line           INTEGER NOT NULL CHECK (line >= 0),
  source_kind    TEXT NOT NULL CHECK (source_kind IN ('doc', 'code-scan')),
  confidence     REAL NOT NULL CHECK (confidence >= 0 AND confidence <= 1),
  UNIQUE (service_key, method, path)
);

CREATE INDEX idx_endpoints_service_path
  ON endpoints(service_key, path, method);
```

- `method` is uppercase and `path` is a canonical route beginning with `/`.
- `file_path` is canonical and workspace-relative. Documentation without a source location uses an empty value rather than a fabricated path.
- Structured API lookup resolves an exact service and uses exact or prefix path conditions. It does not use `%LIKE%` on service names.
- Semantic discovery belongs to retrieval; the structure store does not add a duplicate full-text index for a few thousand routes.

##### 4.5 `dependencies`

One row represents one deduplicated logical dependency. Evidence is stored separately.

```sql
CREATE TABLE dependencies (
  dependency_id      INTEGER PRIMARY KEY,
  caller_service_key TEXT NOT NULL REFERENCES services(service_key) ON DELETE CASCADE,
  target_kind        TEXT NOT NULL CHECK (target_kind IN ('service', 'external')),
  target_service_key TEXT REFERENCES services(service_key) ON DELETE CASCADE,
  external_target    TEXT,
  protocol           TEXT NOT NULL,
  confidence         REAL NOT NULL CHECK (confidence >= 0 AND confidence <= 1),
  CHECK (
    (target_kind = 'service' AND target_service_key IS NOT NULL AND external_target IS NULL) OR
    (target_kind = 'external' AND target_service_key IS NULL AND external_target IS NOT NULL)
  )
);

CREATE UNIQUE INDEX uq_dependencies_service
  ON dependencies(caller_service_key, target_service_key, protocol)
  WHERE target_kind = 'service';
CREATE UNIQUE INDEX uq_dependencies_external
  ON dependencies(caller_service_key, external_target, protocol)
  WHERE target_kind = 'external';
CREATE INDEX idx_dependencies_caller
  ON dependencies(caller_service_key, protocol);
CREATE INDEX idx_dependencies_target
  ON dependencies(target_service_key, protocol)
  WHERE target_service_key IS NOT NULL;
```

- `protocol` uses domain values such as `feign`, `http`, `grpc`, and `rpc`, but table and API names no longer call every dependency Feign.
- Resolved targets reference `service_key`. Targets outside the workspace are explicit `external_target` values.
- Repository ownership is derived from the caller service and is not duplicated.
- Separate partial unique indexes constrain service and external targets so SQLite NULL uniqueness cannot admit duplicate edges.

##### 4.6 `dependency_evidence`

One row is one evidence location for a logical dependency. A dependency can have multiple evidence rows.

```sql
CREATE TABLE dependency_evidence (
  evidence_id   INTEGER PRIMARY KEY,
  dependency_id INTEGER NOT NULL REFERENCES dependencies(dependency_id) ON DELETE CASCADE,
  file_path     TEXT NOT NULL,
  line          INTEGER NOT NULL CHECK (line >= 0),
  symbol        TEXT NOT NULL DEFAULT '',
  source_kind   TEXT NOT NULL CHECK (source_kind IN ('doc', 'code-scan', 'config')),
  UNIQUE (dependency_id, file_path, line, symbol, source_kind)
);

CREATE INDEX idx_dependency_evidence_dependency
  ON dependency_evidence(dependency_id, file_path, line);
CREATE INDEX idx_dependency_evidence_file
  ON dependency_evidence(file_path, line);
```

This table preserves every valid evidence item instead of only `Evidence[0]`. `config` identifies external configuration-center values used to resolve dependency targets. `symbol` replaces the unused Feign-specific `interface_method` and can anchor any protocol.

#### 5. CodeGraph Read Contract

Upper layers do not assemble CodeGraph SQL. The adapter exposes only four capabilities:

```go
SearchSymbols(ctx, SymbolQuery)
FindNodeAt(ctx, canonicalFilePath, line)
FindRelated(ctx, nodeID, direction, edgeKinds, limit)
FindFile(ctx, canonicalFilePath)
```

`SymbolQuery` contains canonical terms, optional kinds, an optional `module_path` scope, and a limit. The rules are:

1. Symbol search uses `nodes_fts MATCH` and joins `nodes.rowid` for full fields.
2. Kind and path scopes are applied before `LIMIT`, not filtered in Go afterward.
3. Terms form one escaped FTS query; there are no per-keyword goroutines or repeated scans.
4. A service name resolves through the structure store to `repo + module_path`, which becomes a CodeGraph `file_path` prefix. It is not queried against `nodes.name`.
5. File lookup accepts only an ingress-canonicalized exact path. Suffix `%LIKE%` and `serviceFromPath` guessing are not normal execution paths.
6. SQL uses `QueryContext`/`QueryRowContext`; request cancellation stops database work.
7. Returned nodes must resolve to current workspace files. A missing file is an index-integrity error, not an empty `source` result.

The CodeGraph provider must retain these indexes:

| Query | Required index |
| --- | --- |
| Full-text symbol lookup | `nodes_fts` and synchronization triggers |
| Symbol at file location | `nodes(file_path, start_line)` |
| Outgoing relationships | `edges(source, kind)` |
| Incoming relationships | `edges(target, kind)` |
| Exact file lookup | `files(path)` primary key |

`nodes(name)` may support exact or prefix lookups, but cannot optimize a leading wildcard. CodeLoom no longer treats it as a fuzzy-search index.

Connection management permits bounded read concurrency and ensures Refresh does not close a connection still used by a query. A validated generation is atomically published; the old generation closes after its in-flight readers release it.

#### 6. Write And Rebuild Lifecycle

Both SQLite databases are disposable derived data and use snapshot publication:

```text
scan source
  -> canonicalize and validate domain records at ingress
  -> write a temporary database
  -> validate schema, foreign keys, duplicates, paths, and revision
  -> close and fsync the temporary database
  -> publish with atomic rename
  -> refresh read connections
```

The structure build enables `PRAGMA foreign_keys = ON` and uses one transaction with prepared batch inserts. The temporary store uses a single-file journal mode and is closed before rename so no WAL sidecar is omitted.

Pre-publication checks include:

- `PRAGMA integrity_check` returns `ok`;
- `PRAGMA foreign_key_check` returns no rows;
- every service has a repository and every endpoint/dependency has a service;
- every repo, module path, and file path is canonical;
- there are no duplicate services, APIs, logical dependencies, or evidence rows;
- each `head_sha` matches the scanned input;
- CodeGraph `nodes_fts` is synchronized with `nodes`, and node paths belong to the current `repos/` workspace.

Validation failure keeps the current generation serving and reports an explicit error. Partial data is never published, and another provider is never substituted silently.

#### 7. Domain And Query API Changes

Implementation removes the following complexity instead of wrapping it in compatibility code:

- Delete `repos`, `repo_index_state`, `feign_edges`, `runbooks`, and the old `dependency_edges`.
- Delete full-object JSON columns from structural records.
- Delete `IndexBundle.Feigns` and Feign-specific persistence; scanners emit `Dependencies`.
- Delete persisted `ServiceRecord.Upstreams/Downstreams`; generate them from dependency queries.
- Limit `IndexBundle` to structural snapshots; runbooks continue through the document store and Qdrant.
- Merge documentation and scan service candidates at indexing ingress; Store accepts unique canonical services and read-time `MergeServices` is removed.
- Replace single-value `RepoSvcMap` with `ServicesByRepos(repos) -> map[repo][]ServiceRef` to preserve multi-service repositories.
- Resolve a CodeGraph file to a service by the longest `module_path` prefix; remove separate Dashboard and retrieval `serviceFromPath` guesses.
- Resolve an exact service before `ListApis` queries by `service_key`; do not apply `%LIKE%` to `service_name`.
- Rename Dashboard/API summary terminology from `feigns` to `dependencies`.
- Remove CodeGraph `%LIKE%` symbol lookup and per-keyword fan-out.

Store methods accept `context.Context`. Batch reads use one `IN` query and map aggregation rather than N+1 calls. Upper layers depend on domain queries, not table names or JSON layouts.

#### 8. Implementation Slices

After approval, implement the proposal in independently verifiable slices:

1. Define the structural domain model, target schema, and Store contract tests.
2. Canonicalize and deduplicate repository, module, service, API, dependency, and evidence records at indexing ingress.
3. Switch structure writes, reads, and in-memory graph construction, then delete the old model.
4. Implement the CodeGraph FTS/context adapter and module-path scoping.
5. Switch Agent, retrieval, Dashboard/API/UI service resolution and `dependencies` terminology.
6. Implement temporary-store validation, atomic publication, and connection-generation refresh.
7. Remove all old tables, compatibility methods, and unreachable code, then perform a full rebuild.

Each slice runs focused package tests and `go build ./...`; final verification runs `go test ./...` and `go vet ./...`. Because the project is not live, no old-database migration script is created. Startup/bootstrap detecting the old schema requires a rebuild.

#### 9. Acceptance Criteria

##### Correctness

- Multiple services in one repository remain independently queryable and map CodeGraph nodes through `module_path`.
- One logical dependency has one row and returns all evidence.
- A removed or renamed repository/module leaves no stale nodes or structure rows after snapshot publication.
- Runbooks, vectors, method calls, and service structure each have one owner.
- Normal reads contain no trimming, case repair, legacy fallback, or suffix-path guessing.

##### Query Plans

- Symbol lookup `EXPLAIN QUERY PLAN` uses `nodes_fts` and does not `SCAN nodes`.
- API listing narrows by the `service_key` index first.
- Outgoing and incoming dependency queries use caller and target indexes respectively.
- File symbol lookup uses `(file_path,start_line)` and graph traversal uses `(source,kind)` or `(target,kind)`.
- There are no per-keyword database calls, per-repository service calls, or other N+1 paths.

##### Performance Baseline

At the current scale of about 435,000 CodeGraph nodes:

- single symbol search targets p95 below 100 ms;
- exact service, API, and one-hop structure queries target p95 below 50 ms;
- the first-token path no longer contains multi-second CodeGraph `%LIKE%` scans;
- metrics separate database, source-read, and evidence-assembly time instead of reporting only total duration.

The fixed dataset and real QA traces must validate these targets. The local 0 to 10 ms FTS measurement is not itself a production SLA.
