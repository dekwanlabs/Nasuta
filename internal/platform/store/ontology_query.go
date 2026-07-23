package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/dekwanlabs/nasuta/internal/ontology"
	"github.com/dekwanlabs/nasuta/platform"
)

func (store *SQLite) ResolveOntology(ctx context.Context, query ontology.ResolveQuery) ([]ontology.Entity, error) {
	if err := ontology.ValidateResolveQuery(query); err != nil {
		return nil, err
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	if err := requireOntologyGeneration(ctx, store.db); err != nil {
		return nil, err
	}

	text := strings.TrimSpace(query.Text)
	normalized := platform.Normalize(text)
	where := `(e.entity_id=? OR e.canonical_key=? OR a.normalized_alias=? OR lower(e.name) LIKE ?)`
	args := []any{text, text, normalized, strings.ToLower(text) + "%"}
	if len(query.Classes) > 0 {
		where += ` AND e.class IN (` + placeholders(len(query.Classes)) + `)`
		for _, class := range query.Classes {
			args = append(args, string(class))
		}
	}
	args = append(args, query.Limit)
	rows, err := store.db.QueryContext(ctx, `SELECT e.entity_id,e.class,e.canonical_key,e.name,e.properties_json,e.confidence,
MIN(CASE WHEN e.entity_id=? THEN 0 WHEN e.canonical_key=? THEN 1 WHEN a.normalized_alias=? THEN 2 ELSE 3 END) AS rank
FROM ontology_entities e LEFT JOIN ontology_aliases a ON a.entity_id=e.entity_id
WHERE `+where+` GROUP BY e.entity_id,e.class,e.canonical_key,e.name,e.properties_json,e.confidence
ORDER BY rank,lower(e.name),e.entity_id LIMIT ?`, append([]any{text, text, normalized}, args...)...)
	if err != nil {
		return nil, fmt.Errorf("resolve ontology entity: %w", err)
	}
	defer rows.Close()
	entities := make([]ontology.Entity, 0, query.Limit)
	for rows.Next() {
		var entity ontology.Entity
		var class, properties string
		var rank int
		if err := rows.Scan(&entity.ID, &class, &entity.Key, &entity.Name, &properties, &entity.Confidence, &rank); err != nil {
			return nil, fmt.Errorf("scan ontology entity: %w", err)
		}
		entity.Class = ontology.Class(class)
		if err := json.Unmarshal([]byte(properties), &entity.Properties); err != nil {
			return nil, fmt.Errorf("decode ontology entity %q properties: %w", entity.ID, err)
		}
		entities = append(entities, entity)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("resolve ontology entities: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close ontology entity rows: %w", err)
	}
	if err := loadOntologyAliases(ctx, store.db, entities); err != nil {
		return nil, err
	}
	return entities, nil
}

func (store *SQLite) OntologyNeighbors(ctx context.Context, query ontology.NeighborQuery) ([]ontology.Fact, bool, error) {
	if err := ontology.ValidateNeighborQuery(query); err != nil {
		return nil, false, err
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	if err := requireOntologyGeneration(ctx, store.db); err != nil {
		return nil, false, err
	}

	entityArgs := make([]any, len(query.EntityIDs))
	for i, id := range query.EntityIDs {
		entityArgs[i] = id
	}
	var where string
	args := make([]any, 0, len(entityArgs)*2+len(query.Predicates)+1)
	switch query.Direction {
	case ontology.DirectionOutgoing:
		where = `subject_id IN (` + placeholders(len(entityArgs)) + `)`
		args = append(args, entityArgs...)
	case ontology.DirectionIncoming:
		where = `object_id IN (` + placeholders(len(entityArgs)) + `)`
		args = append(args, entityArgs...)
	case ontology.DirectionBoth:
		where = `(subject_id IN (` + placeholders(len(entityArgs)) + `) OR object_id IN (` + placeholders(len(entityArgs)) + `))`
		args = append(args, entityArgs...)
		args = append(args, entityArgs...)
	}
	if len(query.Predicates) > 0 {
		where += ` AND predicate IN (` + placeholders(len(query.Predicates)) + `)`
		for _, predicate := range query.Predicates {
			args = append(args, string(predicate))
		}
	}
	args = append(args, query.Limit+1)
	rows, err := store.db.QueryContext(ctx, `SELECT fact_id,subject_id,predicate,object_id,qualifiers_json,confidence
FROM ontology_facts WHERE `+where+` ORDER BY predicate,subject_id,object_id,fact_id LIMIT ?`, args...)
	if err != nil {
		return nil, false, fmt.Errorf("query ontology neighbors: %w", err)
	}
	defer rows.Close()
	facts := make([]ontology.Fact, 0, query.Limit+1)
	for rows.Next() {
		var fact ontology.Fact
		var predicate, qualifiers string
		if err := rows.Scan(&fact.ID, &fact.SubjectID, &predicate, &fact.ObjectID, &qualifiers, &fact.Confidence); err != nil {
			return nil, false, fmt.Errorf("scan ontology fact: %w", err)
		}
		fact.Predicate = ontology.Predicate(predicate)
		if err := json.Unmarshal([]byte(qualifiers), &fact.Qualifiers); err != nil {
			return nil, false, fmt.Errorf("decode ontology fact %q qualifiers: %w", fact.ID, err)
		}
		facts = append(facts, fact)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, false, fmt.Errorf("query ontology neighbors: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, false, fmt.Errorf("close ontology fact rows: %w", err)
	}
	truncated := len(facts) > query.Limit
	if truncated {
		facts = facts[:query.Limit]
	}
	if err := loadOntologyEvidence(ctx, store.db, facts); err != nil {
		return nil, false, err
	}
	return facts, truncated, nil
}

func (store *SQLite) OntologyStats(ctx context.Context) (ontology.Stats, error) {
	store.mu.RLock()
	defer store.mu.RUnlock()
	var stats ontology.Stats
	var schemaVersion int
	if err := store.db.QueryRowContext(ctx, `SELECT generation,ontology_schema_version FROM workspace_snapshot WHERE singleton_id=1`).Scan(&stats.Generation, &schemaVersion); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return stats, ontology.ErrUnavailable
		}
		return stats, fmt.Errorf("read ontology generation: %w", err)
	}
	if schemaVersion != ontology.CurrentSchemaVersion {
		return stats, fmt.Errorf("%w: schema version %d", ontology.ErrUnavailable, schemaVersion)
	}
	for query, destination := range map[string]*int{
		`SELECT COUNT(*) FROM ontology_entities`:      &stats.Entities,
		`SELECT COUNT(*) FROM ontology_facts`:         &stats.Facts,
		`SELECT COUNT(*) FROM ontology_fact_evidence`: &stats.Evidence,
	} {
		if err := store.db.QueryRowContext(ctx, query).Scan(destination); err != nil {
			return stats, fmt.Errorf("read ontology stats: %w", err)
		}
	}
	stats.ByClass = make(map[ontology.Class]int)
	if err := scanOntologyCounts(ctx, store.db, `SELECT class,COUNT(*) FROM ontology_entities GROUP BY class`, func(key string, count int) {
		stats.ByClass[ontology.Class(key)] = count
	}); err != nil {
		return stats, err
	}
	stats.ByPredicate = make(map[ontology.Predicate]int)
	if err := scanOntologyCounts(ctx, store.db, `SELECT predicate,COUNT(*) FROM ontology_facts GROUP BY predicate`, func(key string, count int) {
		stats.ByPredicate[ontology.Predicate(key)] = count
	}); err != nil {
		return stats, err
	}
	return stats, nil
}

func requireOntologyGeneration(ctx context.Context, db *sql.DB) error {
	var version int
	err := db.QueryRowContext(ctx, `SELECT ontology_schema_version FROM workspace_snapshot WHERE singleton_id=1`).Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		return ontology.ErrUnavailable
	}
	if err != nil {
		return fmt.Errorf("read ontology generation: %w", err)
	}
	if version != ontology.CurrentSchemaVersion {
		return fmt.Errorf("%w: schema version %d", ontology.ErrUnavailable, version)
	}
	return nil
}

func loadOntologyAliases(ctx context.Context, db *sql.DB, entities []ontology.Entity) error {
	if len(entities) == 0 {
		return nil
	}
	args := make([]any, len(entities))
	byID := make(map[string]int, len(entities))
	for i := range entities {
		args[i] = entities[i].ID
		byID[entities[i].ID] = i
		entities[i].Aliases = []string{}
	}
	rows, err := db.QueryContext(ctx, `SELECT entity_id,normalized_alias FROM ontology_aliases WHERE entity_id IN (`+placeholders(len(args))+`) ORDER BY entity_id,normalized_alias`, args...)
	if err != nil {
		return fmt.Errorf("load ontology aliases: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, alias string
		if err := rows.Scan(&id, &alias); err != nil {
			return fmt.Errorf("scan ontology alias: %w", err)
		}
		entities[byID[id]].Aliases = append(entities[byID[id]].Aliases, alias)
	}
	return rows.Err()
}

func loadOntologyEvidence(ctx context.Context, db *sql.DB, facts []ontology.Fact) error {
	if len(facts) == 0 {
		return nil
	}
	args := make([]any, len(facts))
	byID := make(map[string]int, len(facts))
	for i := range facts {
		args[i] = facts[i].ID
		byID[facts[i].ID] = i
		facts[i].Evidence = []ontology.Evidence{}
	}
	rows, err := db.QueryContext(ctx, `SELECT fact_id,file_path,line,symbol,source_kind FROM ontology_fact_evidence WHERE fact_id IN (`+placeholders(len(args))+`) ORDER BY fact_id,file_path,line,symbol,source_kind`, args...)
	if err != nil {
		return fmt.Errorf("load ontology evidence: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var factID, source string
		var evidence ontology.Evidence
		if err := rows.Scan(&factID, &evidence.Path, &evidence.Line, &evidence.Symbol, &source); err != nil {
			return fmt.Errorf("scan ontology evidence: %w", err)
		}
		evidence.Source = ontology.EvidenceSource(source)
		facts[byID[factID]].Evidence = append(facts[byID[factID]].Evidence, evidence)
	}
	return rows.Err()
}

func scanOntologyCounts(ctx context.Context, db *sql.DB, query string, add func(string, int)) error {
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return fmt.Errorf("query ontology counts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		var count int
		if err := rows.Scan(&key, &count); err != nil {
			return fmt.Errorf("scan ontology count: %w", err)
		}
		add(key, count)
	}
	return rows.Err()
}

func placeholders(count int) string {
	return strings.TrimSuffix(strings.Repeat("?,", count), ",")
}
