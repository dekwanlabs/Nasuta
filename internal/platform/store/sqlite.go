package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/ontology"
	"github.com/dekwanlabs/nasuta/log"
	"github.com/dekwanlabs/nasuta/platform"
	_ "modernc.org/sqlite"
)

const structureSchemaVersion = 3

const schema = `
CREATE TABLE repositories (
  repo       TEXT PRIMARY KEY,
  head_sha   TEXT NOT NULL CHECK (head_sha <> ''),
  indexed_at INTEGER NOT NULL
) WITHOUT ROWID;

CREATE INDEX idx_repositories_indexed_at ON repositories(indexed_at DESC);

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

CREATE INDEX idx_services_name ON services(service_name);
CREATE INDEX idx_services_repo_name ON services(repo, service_name);

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

CREATE INDEX idx_endpoints_service_path ON endpoints(service_key, path, method);

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
CREATE INDEX idx_dependencies_caller ON dependencies(caller_service_key, protocol);
CREATE INDEX idx_dependencies_target ON dependencies(target_service_key, protocol)
  WHERE target_service_key IS NOT NULL;

CREATE TABLE dependency_evidence (
  evidence_id   INTEGER PRIMARY KEY,
  dependency_id INTEGER NOT NULL REFERENCES dependencies(dependency_id) ON DELETE CASCADE,
  file_path     TEXT NOT NULL,
  line          INTEGER NOT NULL CHECK (line >= 0),
  symbol        TEXT NOT NULL DEFAULT '',
  source_kind   TEXT NOT NULL CHECK (source_kind IN ('doc', 'code-scan')),
  UNIQUE (dependency_id, file_path, line, symbol, source_kind)
);

CREATE INDEX idx_dependency_evidence_dependency
  ON dependency_evidence(dependency_id, file_path, line);
CREATE INDEX idx_dependency_evidence_file ON dependency_evidence(file_path, line);

CREATE TABLE workspace_snapshot (
  singleton_id            INTEGER PRIMARY KEY CHECK (singleton_id = 1),
  generation              TEXT NOT NULL,
  ontology_schema_version INTEGER NOT NULL
);

CREATE TABLE ontology_entities (
  entity_id       TEXT PRIMARY KEY,
  class           TEXT NOT NULL,
  canonical_key   TEXT NOT NULL,
  name            TEXT NOT NULL,
  properties_json TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(properties_json)),
  confidence      REAL NOT NULL CHECK (confidence >= 0 AND confidence <= 1),
  UNIQUE (class, canonical_key)
);

CREATE INDEX idx_ontology_entities_class_name ON ontology_entities(class, name, entity_id);

CREATE TABLE ontology_aliases (
  entity_id        TEXT NOT NULL REFERENCES ontology_entities(entity_id) ON DELETE CASCADE,
  normalized_alias TEXT NOT NULL,
  source           TEXT NOT NULL,
  PRIMARY KEY (entity_id, normalized_alias)
);

CREATE INDEX idx_ontology_alias_lookup ON ontology_aliases(normalized_alias, entity_id);

CREATE TABLE ontology_facts (
  fact_id          TEXT PRIMARY KEY,
  subject_id       TEXT NOT NULL REFERENCES ontology_entities(entity_id) ON DELETE CASCADE,
  predicate        TEXT NOT NULL,
  object_id        TEXT NOT NULL REFERENCES ontology_entities(entity_id) ON DELETE CASCADE,
  qualifiers_json TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(qualifiers_json)),
  confidence      REAL NOT NULL CHECK (confidence >= 0 AND confidence <= 1),
  UNIQUE (subject_id, predicate, object_id, qualifiers_json)
);

CREATE INDEX idx_ontology_facts_out ON ontology_facts(subject_id, predicate, object_id);
CREATE INDEX idx_ontology_facts_in ON ontology_facts(object_id, predicate, subject_id);

CREATE TABLE ontology_fact_evidence (
  fact_id     TEXT NOT NULL REFERENCES ontology_facts(fact_id) ON DELETE CASCADE,
  file_path   TEXT NOT NULL,
  line        INTEGER NOT NULL CHECK (line >= 0),
  symbol      TEXT NOT NULL DEFAULT '',
  source_kind TEXT NOT NULL CHECK (source_kind IN ('doc', 'code-scan')),
  PRIMARY KEY (fact_id, file_path, line, symbol, source_kind)
);

CREATE INDEX idx_ontology_evidence_path ON ontology_fact_evidence(file_path, line, fact_id);

PRAGMA user_version = 3;
`

// SQLite stores the canonical structured workspace snapshot.
type SQLite struct {
	mu   sync.RWMutex
	db   *sql.DB
	path string
}

// Open opens the structure store and discards an obsolete derived schema.
func Open(path string) (*SQLite, error) {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create sqlite directory %q: %w", dir, err)
		}
	}
	db, obsolete, err := openStructureDB(path)
	if err != nil {
		return nil, err
	}
	if obsolete {
		if err := db.Close(); err != nil {
			return nil, fmt.Errorf("close obsolete sqlite %q: %w", path, err)
		}
		for _, suffix := range []string{"", "-wal", "-shm"} {
			if err := os.Remove(path + suffix); err != nil && !os.IsNotExist(err) {
				return nil, fmt.Errorf("remove obsolete sqlite %q: %w", path+suffix, err)
			}
		}
		log.Warnf("[store] discarded obsolete derived SQLite schema; run structural rebuild: %s", path)
		db, _, err = openStructureDB(path)
		if err != nil {
			return nil, err
		}
	}
	return &SQLite{db: db, path: path}, nil
}

func openStructureDB(path string) (*sql.DB, bool, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, false, fmt.Errorf("open sqlite %q: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA foreign_keys = ON; PRAGMA busy_timeout = 5000;`); err != nil {
		_ = db.Close()
		return nil, false, fmt.Errorf("configure sqlite %q: %w", path, err)
	}
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		_ = db.Close()
		return nil, false, fmt.Errorf("read sqlite schema version %q: %w", path, err)
	}
	if version != 0 && version != structureSchemaVersion {
		return db, true, nil
	}
	if version == 0 {
		var businessTables int
		if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'`).Scan(&businessTables); err != nil {
			_ = db.Close()
			return nil, false, fmt.Errorf("inspect sqlite schema %q: %w", path, err)
		}
		if businessTables > 0 {
			return db, true, nil
		}
		if _, err := db.Exec(schema); err != nil {
			_ = db.Close()
			return nil, false, fmt.Errorf("create sqlite schema %q: %w", path, err)
		}
	}
	return db, false, nil
}

// Close closes the active snapshot after current readers finish.
func (store *SQLite) Close() error {
	if store == nil {
		return nil
	}
	store.mu.Lock()
	db := store.db
	store.db = nil
	store.mu.Unlock()
	if db == nil {
		return nil
	}
	return db.Close()
}

// ReplaceStructure publishes a structure-only snapshot for non-SQLite ontology providers.
func (store *SQLite) ReplaceStructure(ctx context.Context, generation string, bundle domain.IndexBundle) error {
	return store.replaceSnapshot(ctx, generation, bundle, nil)
}

// ReplaceWorkspace keeps structure and ontology on the same atomic file generation.
func (store *SQLite) ReplaceWorkspace(ctx context.Context, bundle domain.IndexBundle, snapshot ontology.Snapshot) (string, error) {
	if err := ontology.ValidateSnapshot(snapshot); err != nil {
		return "", fmt.Errorf("validate ontology snapshot: %w", err)
	}
	generation, err := (ontology.WorkspaceSnapshot{Structure: bundle, Ontology: snapshot}).Generation()
	if err != nil {
		return "", fmt.Errorf("derive workspace generation: %w", err)
	}
	if err := store.replaceSnapshot(ctx, generation, bundle, &snapshot); err != nil {
		return "", err
	}
	return generation, nil
}

func (store *SQLite) replaceSnapshot(ctx context.Context, generation string, bundle domain.IndexBundle, snapshot *ontology.Snapshot) error {
	if generation == "" {
		return fmt.Errorf("workspace generation is required")
	}
	if err := validateBundle(bundle); err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.tmp-%d", store.path, time.Now().UnixNano())
	defer os.Remove(tmp)

	db, obsolete, err := openStructureDB(tmp)
	if err != nil {
		return err
	}
	if obsolete {
		_ = db.Close()
		return fmt.Errorf("temporary sqlite %q unexpectedly has an obsolete schema", tmp)
	}
	if err := writeSnapshot(ctx, db, generation, bundle, snapshot); err != nil {
		_ = db.Close()
		return err
	}
	if err := validateDatabase(ctx, db); err != nil {
		_ = db.Close()
		return err
	}
	if err := db.Close(); err != nil {
		return fmt.Errorf("close temporary sqlite %q: %w", tmp, err)
	}
	if err := os.Rename(tmp, store.path); err != nil {
		return fmt.Errorf("publish sqlite snapshot %q: %w", store.path, err)
	}
	next, obsolete, err := openStructureDB(store.path)
	if err != nil {
		return err
	}
	if obsolete {
		_ = next.Close()
		return fmt.Errorf("published sqlite snapshot %q has obsolete schema", store.path)
	}

	store.mu.Lock()
	previous := store.db
	store.db = next
	store.mu.Unlock()
	if previous != nil {
		if err := previous.Close(); err != nil {
			log.Warnf("[store] close replaced SQLite snapshot: %v", err)
		}
	}
	return nil
}

func validateBundle(bundle domain.IndexBundle) error {
	repositories := make(map[string]struct{}, len(bundle.Repositories))
	for _, repository := range bundle.Repositories {
		if repository.Repo == "" || repository.HeadSHA == "" || repository.IndexedAt <= 0 {
			return fmt.Errorf("invalid repository snapshot: %+v", repository)
		}
		repositories[repository.Repo] = struct{}{}
	}
	services := make(map[string]struct{}, len(bundle.Services))
	for _, service := range bundle.Services {
		if service.ServiceKey == "" || service.Repo == "" || service.ModulePath == "" || service.ServiceName == "" {
			return fmt.Errorf("invalid canonical service %q", service.ServiceName)
		}
		if service.ServiceKey != platform.UUIDFromString(service.Repo+"\x00"+service.ModulePath) {
			return fmt.Errorf("invalid service key for %q", service.ServiceName)
		}
		if _, ok := repositories[service.Repo]; !ok {
			return fmt.Errorf("service %q references missing repository %q", service.ServiceName, service.Repo)
		}
		services[service.ServiceKey] = struct{}{}
	}
	for _, endpoint := range bundle.Endpoints {
		if _, ok := services[endpoint.ServiceKey]; !ok {
			return fmt.Errorf("endpoint %s %s references missing service %q", endpoint.Method, endpoint.Path, endpoint.ServiceKey)
		}
	}
	for _, dependency := range bundle.Dependencies {
		if _, ok := services[dependency.CallerServiceKey]; !ok {
			return fmt.Errorf("dependency %q -> %q references missing caller", dependency.From, dependency.To)
		}
		if dependency.TargetKind == domain.DependencyTargetService {
			if _, ok := services[dependency.TargetServiceKey]; !ok {
				return fmt.Errorf("dependency %q -> %q references missing target", dependency.From, dependency.To)
			}
		} else if dependency.TargetKind != domain.DependencyTargetExternal || dependency.ExternalTarget == "" {
			return fmt.Errorf("dependency %q -> %q has invalid target", dependency.From, dependency.To)
		}
	}
	return nil
}

func writeSnapshot(ctx context.Context, db *sql.DB, generation string, bundle domain.IndexBundle, snapshot *ontology.Snapshot) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin sqlite snapshot: %w", err)
	}
	defer tx.Rollback()
	ontologyVersion := 0
	if snapshot != nil {
		ontologyVersion = snapshot.SchemaVersion
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO workspace_snapshot(singleton_id,generation,ontology_schema_version) VALUES(1,?,?)`, generation, ontologyVersion); err != nil {
		return fmt.Errorf("insert workspace generation %q: %w", generation, err)
	}
	for _, repository := range bundle.Repositories {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO repositories(repo,head_sha,indexed_at) VALUES(?,?,?)`,
			repository.Repo, repository.HeadSHA, repository.IndexedAt); err != nil {
			return fmt.Errorf("insert repository %q: %w", repository.Repo, err)
		}
	}
	for _, service := range bundle.Services {
		arrays, err := marshalServiceArrays(service)
		if err != nil {
			return fmt.Errorf("marshal service %q: %w", service.ServiceName, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO services(
service_key,repo,module_path,service_name,layer,language,runtime,scope,owner,status,summary,confidence,
tags_json,docs_json,source_of_truth_json,entrypoints_json,ports_json) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			service.ServiceKey, service.Repo, service.ModulePath, service.ServiceName,
			service.Layer, service.Language, service.Runtime, service.Scope, service.Owner,
			service.Status, service.Summary, service.Confidence,
			arrays[0], arrays[1], arrays[2], arrays[3], arrays[4]); err != nil {
			return fmt.Errorf("insert service %q: %w", service.ServiceName, err)
		}
	}
	for _, endpoint := range bundle.Endpoints {
		if _, err := tx.ExecContext(ctx, `INSERT INTO endpoints(
service_key,method,path,handler,handler_method,file_path,line,source_kind,confidence) VALUES(?,?,?,?,?,?,?,?,?)`,
			endpoint.ServiceKey, endpoint.Method, endpoint.Path, endpoint.Handler,
			endpoint.HandlerMethod, endpoint.File, endpoint.Line, string(endpoint.Source), endpoint.Confidence); err != nil {
			return fmt.Errorf("insert endpoint %s %s: %w", endpoint.Method, endpoint.Path, err)
		}
	}
	for _, dependency := range bundle.Dependencies {
		var targetKey, external any
		if dependency.TargetKind == domain.DependencyTargetService {
			targetKey = dependency.TargetServiceKey
		} else {
			external = dependency.ExternalTarget
		}
		result, err := tx.ExecContext(ctx, `INSERT INTO dependencies(
caller_service_key,target_kind,target_service_key,external_target,protocol,confidence) VALUES(?,?,?,?,?,?)`,
			dependency.CallerServiceKey, string(dependency.TargetKind), targetKey, external,
			string(dependency.Type), dependency.Confidence)
		if err != nil {
			return fmt.Errorf("insert dependency %q -> %q: %w", dependency.From, dependency.To, err)
		}
		dependencyID, err := result.LastInsertId()
		if err != nil {
			return fmt.Errorf("read dependency id %q -> %q: %w", dependency.From, dependency.To, err)
		}
		for _, evidence := range dependency.Evidence {
			if _, err := tx.ExecContext(ctx, `INSERT INTO dependency_evidence(
dependency_id,file_path,line,symbol,source_kind) VALUES(?,?,?,?,?)`,
				dependencyID, evidence.Path, evidence.Line, evidence.Symbol, string(evidence.Kind)); err != nil {
				return fmt.Errorf("insert dependency evidence %q:%d: %w", evidence.Path, evidence.Line, err)
			}
		}
	}
	if snapshot != nil {
		if err := writeOntologySnapshot(ctx, tx, *snapshot); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit sqlite snapshot: %w", err)
	}
	return nil
}

func writeOntologySnapshot(ctx context.Context, tx *sql.Tx, snapshot ontology.Snapshot) error {
	for _, entity := range snapshot.Entities {
		properties, err := json.Marshal(entity.Properties)
		if err != nil {
			return fmt.Errorf("marshal ontology entity %q properties: %w", entity.ID, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO ontology_entities(
entity_id,class,canonical_key,name,properties_json,confidence) VALUES(?,?,?,?,?,?)`,
			entity.ID, string(entity.Class), entity.Key, entity.Name, properties, entity.Confidence); err != nil {
			return fmt.Errorf("insert ontology entity %q: %w", entity.ID, err)
		}
		for _, alias := range entity.Aliases {
			if _, err := tx.ExecContext(ctx, `INSERT INTO ontology_aliases(entity_id,normalized_alias,source) VALUES(?,?,?)`, entity.ID, alias, "projection"); err != nil {
				return fmt.Errorf("insert ontology alias %q for %q: %w", alias, entity.ID, err)
			}
		}
	}
	for _, fact := range snapshot.Facts {
		qualifiers, err := json.Marshal(fact.Qualifiers)
		if err != nil {
			return fmt.Errorf("marshal ontology fact %q qualifiers: %w", fact.ID, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO ontology_facts(
fact_id,subject_id,predicate,object_id,qualifiers_json,confidence) VALUES(?,?,?,?,?,?)`,
			fact.ID, fact.SubjectID, string(fact.Predicate), fact.ObjectID, qualifiers, fact.Confidence); err != nil {
			return fmt.Errorf("insert ontology fact %q: %w", fact.ID, err)
		}
		for _, evidence := range fact.Evidence {
			if _, err := tx.ExecContext(ctx, `INSERT INTO ontology_fact_evidence(
fact_id,file_path,line,symbol,source_kind) VALUES(?,?,?,?,?)`,
				fact.ID, evidence.Path, evidence.Line, evidence.Symbol, string(evidence.Source)); err != nil {
				return fmt.Errorf("insert ontology evidence %q:%d for %q: %w", evidence.Path, evidence.Line, fact.ID, err)
			}
		}
	}
	return nil
}

func validateDatabase(ctx context.Context, db *sql.DB) error {
	var integrity string
	if err := db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&integrity); err != nil {
		return fmt.Errorf("sqlite integrity check: %w", err)
	}
	if integrity != "ok" {
		return fmt.Errorf("sqlite integrity check: %s", integrity)
	}
	rows, err := db.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return fmt.Errorf("sqlite foreign key check: %w", err)
	}
	defer rows.Close()
	if rows.Next() {
		return fmt.Errorf("sqlite foreign key check failed")
	}
	return rows.Err()
}

func marshalServiceArrays(service domain.ServiceRecord) ([5]string, error) {
	values := []any{service.Tags, service.Docs, service.SourceOfTruth, service.Entrypoints, service.Ports}
	var out [5]string
	for i, value := range values {
		blob, err := json.Marshal(value)
		if err != nil {
			return out, err
		}
		out[i] = string(blob)
	}
	return out, nil
}

// GetIndexSHA returns the repository revision in the active snapshot.
func (store *SQLite) GetIndexSHA(ctx context.Context, repo string) (string, error) {
	store.mu.RLock()
	defer store.mu.RUnlock()
	var sha string
	err := store.db.QueryRowContext(ctx, `SELECT head_sha FROM repositories WHERE repo = ?`, repo).Scan(&sha)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return sha, err
}

// WorkspaceGeneration identifies the structure and ontology version currently visible.
func (store *SQLite) WorkspaceGeneration(ctx context.Context) (string, int, error) {
	store.mu.RLock()
	defer store.mu.RUnlock()
	var generation string
	var schemaVersion int
	err := store.db.QueryRowContext(ctx, `SELECT generation,ontology_schema_version FROM workspace_snapshot WHERE singleton_id=1`).Scan(&generation, &schemaVersion)
	return generation, schemaVersion, err
}

// AllServices returns the canonical service rows without read-time merging.
func (store *SQLite) AllServices(ctx context.Context) ([]domain.ServiceRecord, error) {
	return store.queryServices(ctx, "", nil)
}

// ServicesByRepos returns every service module for the selected repositories.
func (store *SQLite) ServicesByRepos(ctx context.Context, repos []string) ([]domain.ServiceRecord, error) {
	if len(repos) == 0 {
		return []domain.ServiceRecord{}, nil
	}
	placeholders := strings.TrimRight(strings.Repeat("?,", len(repos)), ",")
	args := make([]any, len(repos))
	for i, repo := range repos {
		args[i] = repo
	}
	return store.queryServices(ctx, " WHERE repo IN ("+placeholders+")", args)
}

// ServiceForPath resolves canonical repos/<group>/<project>/... ownership by longest module prefix.
func (store *SQLite) ServiceForPath(ctx context.Context, filePath string) (domain.ServiceRecord, error) {
	path := strings.Trim(strings.ReplaceAll(filePath, "\\", "/"), "/")
	parts := strings.Split(path, "/")
	if len(parts) < 4 || parts[0] != "repos" {
		return domain.ServiceRecord{}, fmt.Errorf("resolve service path %q: expected repos/<group>/<project>/...", filePath)
	}
	repo := parts[1] + "/" + parts[2]
	relative := strings.Join(parts[3:], "/")

	store.mu.RLock()
	defer store.mu.RUnlock()
	row := store.db.QueryRowContext(ctx, `SELECT service_key,repo,module_path,service_name,layer,language,
runtime,scope,owner,status,summary,confidence,tags_json,docs_json,source_of_truth_json,entrypoints_json,ports_json
FROM services
WHERE repo=? AND (module_path='.' OR module_path=? OR ? LIKE module_path || '/%')
ORDER BY CASE WHEN module_path='.' THEN 0 ELSE length(module_path) END DESC
LIMIT 1`, repo, relative, relative)
	service, err := scanService(row)
	if err != nil {
		return domain.ServiceRecord{}, fmt.Errorf("resolve service path %q: %w", filePath, err)
	}
	return service, nil
}

// ServiceByKey returns one canonical service record.
func (store *SQLite) ServiceByKey(ctx context.Context, serviceKey string) (domain.ServiceRecord, error) {
	store.mu.RLock()
	defer store.mu.RUnlock()
	row := store.db.QueryRowContext(ctx, `SELECT service_key,repo,module_path,service_name,layer,language,
runtime,scope,owner,status,summary,confidence,tags_json,docs_json,source_of_truth_json,entrypoints_json,ports_json
FROM services WHERE service_key=? LIMIT 1`, serviceKey)
	service, err := scanService(row)
	if err != nil {
		return domain.ServiceRecord{}, fmt.Errorf("get service %q: %w", serviceKey, err)
	}
	return service, nil
}

// DependenciesByEvidencePath returns bounded dependencies declared by one source file.
func (store *SQLite) DependenciesByEvidencePath(ctx context.Context, filePath string, limit int) ([]domain.DependencyEdge, bool, error) {
	if limit <= 0 {
		limit = 10
	}
	return store.queryDependencyEvidence(ctx, `WHERE e.file_path=?`, []any{filePath}, limit)
}

// IncomingDependencies returns bounded evidence for dependencies targeting one service.
func (store *SQLite) IncomingDependencies(ctx context.Context, targetServiceKey string, limit int) ([]domain.DependencyEdge, bool, error) {
	if limit <= 0 {
		limit = 40
	}
	return store.queryDependencyEvidence(ctx, `WHERE d.target_service_key=?`, []any{targetServiceKey}, limit)
}

func (store *SQLite) queryDependencyEvidence(ctx context.Context, where string, args []any, limit int) ([]domain.DependencyEdge, bool, error) {
	store.mu.RLock()
	defer store.mu.RUnlock()
	queryArgs := append(append([]any{}, args...), limit+1)
	rows, err := store.db.QueryContext(ctx, `SELECT d.dependency_id,d.caller_service_key,caller.service_name,
d.target_kind,COALESCE(d.target_service_key,''),COALESCE(target.service_name,''),COALESCE(d.external_target,''),
d.protocol,d.confidence,e.file_path,e.line,e.symbol,e.source_kind
FROM dependencies d
JOIN services caller ON caller.service_key=d.caller_service_key
LEFT JOIN services target ON target.service_key=d.target_service_key
JOIN dependency_evidence e ON e.dependency_id=d.dependency_id
`+where+` ORDER BY d.dependency_id,e.evidence_id LIMIT ?`, queryArgs...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	edges := make([]domain.DependencyEdge, 0)
	byID := make(map[int64]int)
	rowCount := 0
	more := false
	for rows.Next() {
		rowCount++
		if rowCount > limit {
			more = true
			break
		}
		var id int64
		var edge domain.DependencyEdge
		var targetKind, protocol, targetName string
		var evidence domain.Evidence
		if err := rows.Scan(&id, &edge.CallerServiceKey, &edge.From, &targetKind, &edge.TargetServiceKey,
			&targetName, &edge.ExternalTarget, &protocol, &edge.Confidence,
			&evidence.Path, &evidence.Line, &evidence.Symbol, &evidence.Kind); err != nil {
			return nil, false, err
		}
		index, found := byID[id]
		if !found {
			edge.TargetKind = domain.DependencyTargetKind(targetKind)
			edge.Type = domain.EdgeType(protocol)
			if edge.TargetKind == domain.DependencyTargetService {
				edge.To = targetName
			} else {
				edge.To = edge.ExternalTarget
			}
			edge.Evidence = []domain.Evidence{}
			index = len(edges)
			byID[id] = index
			edges = append(edges, edge)
		}
		edges[index].Evidence = append(edges[index].Evidence, evidence)
	}
	return edges, more, rows.Err()
}

// EndpointNearNode resolves a route annotation inside or immediately before a symbol.
func (store *SQLite) EndpointNearNode(ctx context.Context, serviceKey, filePath string, startLine, endLine int) (domain.EndpointRecord, error) {
	store.mu.RLock()
	defer store.mu.RUnlock()
	if startLine > 8 {
		startLine -= 8
	} else {
		startLine = 1
	}
	if endLine < startLine {
		endLine = startLine
	}
	row := store.db.QueryRowContext(ctx, `SELECT e.service_key,s.service_name,s.repo,e.method,e.path,
e.handler,e.handler_method,e.file_path,e.line,e.source_kind,e.confidence
FROM endpoints e JOIN services s ON s.service_key=e.service_key
WHERE e.service_key=? AND e.file_path=? AND e.line BETWEEN ? AND ?
ORDER BY abs(e.line-?) LIMIT 1`, serviceKey, filePath, startLine, endLine, startLine+8)
	var endpoint domain.EndpointRecord
	var source string
	if err := row.Scan(&endpoint.ServiceKey, &endpoint.ServiceName, &endpoint.Repo, &endpoint.Method,
		&endpoint.Path, &endpoint.Handler, &endpoint.HandlerMethod, &endpoint.File,
		&endpoint.Line, &source, &endpoint.Confidence); err != nil {
		return domain.EndpointRecord{}, err
	}
	endpoint.Source = domain.SourceKind(source)
	return endpoint, nil
}

func (store *SQLite) queryServices(ctx context.Context, where string, args []any) ([]domain.ServiceRecord, error) {
	store.mu.RLock()
	defer store.mu.RUnlock()
	rows, err := store.db.QueryContext(ctx, `SELECT service_key,repo,module_path,service_name,layer,language,
runtime,scope,owner,status,summary,confidence,tags_json,docs_json,source_of_truth_json,entrypoints_json,ports_json
FROM services`+where+` ORDER BY repo,module_path`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	services := make([]domain.ServiceRecord, 0)
	for rows.Next() {
		service, err := scanService(rows)
		if err != nil {
			return nil, err
		}
		services = append(services, service)
	}
	return services, rows.Err()
}

type rowScanner interface {
	Scan(...any) error
}

func scanService(row rowScanner) (domain.ServiceRecord, error) {
	var service domain.ServiceRecord
	var tags, docs, sourceOfTruth, entrypoints, ports string
	err := row.Scan(&service.ServiceKey, &service.Repo, &service.ModulePath, &service.ServiceName,
		&service.Layer, &service.Language, &service.Runtime, &service.Scope, &service.Owner,
		&service.Status, &service.Summary, &service.Confidence,
		&tags, &docs, &sourceOfTruth, &entrypoints, &ports)
	if err != nil {
		return service, err
	}
	for _, item := range []struct {
		blob string
		dest any
	}{{tags, &service.Tags}, {docs, &service.Docs}, {sourceOfTruth, &service.SourceOfTruth}, {entrypoints, &service.Entrypoints}, {ports, &service.Ports}} {
		if err := json.Unmarshal([]byte(item.blob), item.dest); err != nil {
			return service, fmt.Errorf("decode service %q arrays: %w", service.ServiceName, err)
		}
	}
	return service, nil
}

// EndpointPage is a paginated endpoint list.
type EndpointPage struct {
	Total int                     `json:"total"`
	List  []domain.EndpointRecord `json:"list"`
}

// ListApis lists APIs for an exact service name and optional path prefix.
func (store *SQLite) ListApis(ctx context.Context, service, pathPrefix string, page, pageSize int) (*EndpointPage, error) {
	store.mu.RLock()
	defer store.mu.RUnlock()
	where := " WHERE 1=1"
	args := make([]any, 0, 2)
	if service != "" {
		where += " AND s.service_name = ?"
		args = append(args, service)
	}
	if pathPrefix != "" {
		where += " AND e.path >= ? AND e.path < ?"
		args = append(args, pathPrefix, pathPrefix+"\U0010ffff")
	}
	var total int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM endpoints e JOIN services s ON s.service_key=e.service_key`+where, args...).Scan(&total); err != nil {
		return nil, err
	}
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = 20
	}
	offset := (page - 1) * pageSize
	rows, err := store.db.QueryContext(ctx, `SELECT e.service_key,s.service_name,s.repo,e.method,e.path,
e.handler,e.handler_method,e.file_path,e.line,e.source_kind,e.confidence
FROM endpoints e JOIN services s ON s.service_key=e.service_key`+where+` ORDER BY s.service_name,e.path,e.method LIMIT ? OFFSET ?`,
		append(args, pageSize, offset)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := make([]domain.EndpointRecord, 0, pageSize)
	for rows.Next() {
		var endpoint domain.EndpointRecord
		var source string
		if err := rows.Scan(&endpoint.ServiceKey, &endpoint.ServiceName, &endpoint.Repo, &endpoint.Method,
			&endpoint.Path, &endpoint.Handler, &endpoint.HandlerMethod, &endpoint.File,
			&endpoint.Line, &source, &endpoint.Confidence); err != nil {
			return nil, err
		}
		endpoint.Source = domain.SourceKind(source)
		list = append(list, endpoint)
	}
	return &EndpointPage{Total: total, List: list}, rows.Err()
}

// Edges returns logical dependencies with all evidence locations.
func (store *SQLite) Edges(ctx context.Context) ([]domain.DependencyEdge, error) {
	store.mu.RLock()
	defer store.mu.RUnlock()
	rows, err := store.db.QueryContext(ctx, `SELECT d.dependency_id,d.caller_service_key,caller.service_name,
d.target_kind,COALESCE(d.target_service_key,''),COALESCE(target.service_name,''),COALESCE(d.external_target,''),
d.protocol,d.confidence,e.file_path,e.line,e.symbol,e.source_kind
FROM dependencies d
JOIN services caller ON caller.service_key=d.caller_service_key
LEFT JOIN services target ON target.service_key=d.target_service_key
LEFT JOIN dependency_evidence e ON e.dependency_id=d.dependency_id
ORDER BY d.dependency_id,e.evidence_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	edges := make([]domain.DependencyEdge, 0)
	byID := make(map[int64]int)
	for rows.Next() {
		var id int64
		var edge domain.DependencyEdge
		var targetKind, protocol, targetName string
		var evidencePath, symbol, source sql.NullString
		var line sql.NullInt64
		if err := rows.Scan(&id, &edge.CallerServiceKey, &edge.From, &targetKind, &edge.TargetServiceKey,
			&targetName, &edge.ExternalTarget, &protocol, &edge.Confidence,
			&evidencePath, &line, &symbol, &source); err != nil {
			return nil, err
		}
		index, ok := byID[id]
		if !ok {
			edge.TargetKind = domain.DependencyTargetKind(targetKind)
			edge.Type = domain.EdgeType(protocol)
			if edge.TargetKind == domain.DependencyTargetService {
				edge.To = targetName
			} else {
				edge.To = edge.ExternalTarget
			}
			edge.Evidence = []domain.Evidence{}
			index = len(edges)
			byID[id] = index
			edges = append(edges, edge)
		}
		if evidencePath.Valid {
			edges[index].Evidence = append(edges[index].Evidence, domain.Evidence{
				Path: evidencePath.String, Line: int(line.Int64), Symbol: symbol.String, Kind: domain.SourceKind(source.String),
			})
		}
	}
	return edges, rows.Err()
}

func (store *SQLite) EndpointCountFor(ctx context.Context, service string) (int, error) {
	return store.count(ctx, `SELECT COUNT(*) FROM endpoints e JOIN services s ON s.service_key=e.service_key WHERE s.service_name=?`, service)
}

func (store *SQLite) OutgoingCountFor(ctx context.Context, service string) (int, error) {
	return store.count(ctx, `SELECT COUNT(*) FROM dependencies d JOIN services s ON s.service_key=d.caller_service_key WHERE s.service_name=?`, service)
}

func (store *SQLite) ReposWithServices(ctx context.Context) ([]string, error) {
	store.mu.RLock()
	defer store.mu.RUnlock()
	rows, err := store.db.QueryContext(ctx, `SELECT repo FROM repositories ORDER BY repo`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	repos := make([]string, 0)
	for rows.Next() {
		var repo string
		if err := rows.Scan(&repo); err != nil {
			return nil, err
		}
		repos = append(repos, repo)
	}
	return repos, rows.Err()
}

// Summary holds top-level structure-store counts.
type Summary struct {
	Services     int `json:"services"`
	Endpoints    int `json:"endpoints"`
	Dependencies int `json:"dependencies"`
	Repos        int `json:"repos"`
}

func (store *SQLite) Summary(ctx context.Context) (Summary, error) {
	var summary Summary
	var err error
	if summary.Services, err = store.count(ctx, `SELECT COUNT(*) FROM services`); err != nil {
		return summary, err
	}
	if summary.Endpoints, err = store.count(ctx, `SELECT COUNT(*) FROM endpoints`); err != nil {
		return summary, err
	}
	if summary.Dependencies, err = store.count(ctx, `SELECT COUNT(*) FROM dependencies`); err != nil {
		return summary, err
	}
	if summary.Repos, err = store.count(ctx, `SELECT COUNT(*) FROM repositories`); err != nil {
		return summary, err
	}
	return summary, nil
}

func (store *SQLite) count(ctx context.Context, query string, args ...any) (int, error) {
	store.mu.RLock()
	defer store.mu.RUnlock()
	var count int
	return count, store.db.QueryRowContext(ctx, query, args...).Scan(&count)
}
