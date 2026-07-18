package memory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/dekwanlabs/astris/internal/platform/dbschema"
	"github.com/dekwanlabs/astris/internal/platform/embed"
	qstore "github.com/dekwanlabs/astris/internal/platform/store"
	"github.com/dekwanlabs/astris/log"
	"github.com/dekwanlabs/astris/platform"
	mysqlDriver "github.com/go-sql-driver/mysql"
)

type LongTermRecord = MemoryRecord

const memorySelectColumns = `id,user_id,fact_key,kind,content,source_type,authority,status,
	superseded_by,source_session,confidence,expires_at,created_at,updated_at,last_used,use_count`

// MemoryStore keeps durable state in MySQL and semantic candidates in Qdrant.
type MemoryStore struct {
	db             *sql.DB
	semantic       qstore.SemanticStore
	embedder       embed.Embedder
	workContextTTL time.Duration
	now            func() time.Time
}

// OpenMemoryStore opens the long-term memory store.
func OpenMemoryStore(dsn string, semantic qstore.SemanticStore, embedder embed.Embedder, workContextTTL time.Duration) (*MemoryStore, error) {
	db, err := qstore.MySQL(dsn)
	if err != nil {
		return nil, fmt.Errorf("memory: open: %w", err)
	}
	if err := dbschema.MigrateMySQL(db, dbschema.GroupQAMemory); err != nil {
		db.Close()
		return nil, fmt.Errorf("memory: migrate: %w", err)
	}
	return newMemoryStore(db, semantic, embedder, workContextTTL), nil
}

func newMemoryStore(db *sql.DB, semantic qstore.SemanticStore, embedder embed.Embedder, workContextTTL time.Duration) *MemoryStore {
	return &MemoryStore{
		db:             db,
		semantic:       semantic,
		embedder:       embedder,
		workContextTTL: workContextTTL,
		now:            time.Now,
	}
}

// Enabled reports whether semantic recall is available.
func (memory *MemoryStore) Enabled() bool {
	return memory != nil &&
		memory.semantic != nil && memory.semantic.Enabled() &&
		memory.embedder != nil && memory.embedder.Enabled()
}

// Write applies authority-based replacement and then refreshes vector candidates.
func (memory *MemoryStore) Write(ctx context.Context, incoming MemoryRecord) (WriteResult, error) {
	now := memory.now().UTC()
	rec, err := canonicalizeRecord(incoming, memory.workContextTTL, now)
	if err != nil {
		return WriteResult{}, err
	}
	if rec.ID == "" {
		seed := fmt.Sprintf("mem:%d:%s:%s:%d", rec.UserID, rec.FactKey, rec.Content, now.UnixNano())
		rec.ID = platform.UUIDFromString(seed)
	}
	rec.CreatedAt = now
	rec.UpdatedAt = now

	var result WriteResult
	var stored MemoryRecord
	var replaced *MemoryRecord
	for attempt := 0; attempt < 2; attempt++ {
		result, stored, replaced, err = memory.writeTransaction(ctx, rec)
		if err == nil || !isDuplicateKey(err) {
			break
		}
	}
	if err != nil {
		return WriteResult{}, err
	}

	vectorSynced := true
	if replaced != nil {
		vectorSynced = memory.syncVector(ctx, *replaced) && vectorSynced
	}
	vectorSynced = memory.syncVector(ctx, stored) && vectorSynced
	result.VectorSynced = vectorSynced
	return result, nil
}

func (memory *MemoryStore) writeTransaction(ctx context.Context, rec MemoryRecord) (WriteResult, MemoryRecord, *MemoryRecord, error) {
	tx, err := memory.db.BeginTx(ctx, nil)
	if err != nil {
		return WriteResult{}, MemoryRecord{}, nil, fmt.Errorf("memory: begin write: %w", err)
	}
	defer tx.Rollback()

	active, err := getActiveFact(ctx, tx, rec.UserID, rec.FactKey)
	if err != nil {
		return WriteResult{}, MemoryRecord{}, nil, err
	}

	var result WriteResult
	stored := rec
	var replaced *MemoryRecord
	switch {
	case active == nil:
		if err := insertMemory(ctx, tx, rec); err != nil {
			return WriteResult{}, MemoryRecord{}, nil, err
		}
		result = WriteResult{ID: rec.ID, Outcome: WriteInserted}

	case active.Content == rec.Content:
		sourceType := active.SourceType
		authority := active.Authority
		kind := active.Kind
		sourceSession := active.SourceSession
		confidence := active.Confidence
		expiresAt := active.ExpiresAt
		if rec.Authority >= active.Authority {
			sourceType = rec.SourceType
			authority = rec.Authority
			kind = rec.Kind
			sourceSession = rec.SourceSession
			confidence = max(active.Confidence, rec.Confidence)
			expiresAt = rec.ExpiresAt
		}
		_, err := tx.ExecContext(ctx,
			`UPDATE qa_memories
			 SET kind=?,source_type=?,authority=?,source_session=?,confidence=?,
			     expires_at=?,updated_at=?,use_count=use_count+1
			 WHERE id=? AND user_id=? AND status='active'`,
			kind, sourceType, authority, sourceSession, confidence, expiresAt, rec.UpdatedAt,
			active.ID, rec.UserID,
		)
		if err != nil {
			return WriteResult{}, MemoryRecord{}, nil, fmt.Errorf("memory: refresh fact %q: %w", rec.FactKey, err)
		}
		active.Kind = kind
		active.SourceType = sourceType
		active.Authority = authority
		active.SourceSession = sourceSession
		active.Confidence = confidence
		active.ExpiresAt = expiresAt
		active.UpdatedAt = rec.UpdatedAt
		active.UseCount++
		stored = *active
		result = WriteResult{ID: active.ID, Outcome: WriteRefreshed}

	case rec.Authority < active.Authority:
		rec.Status = StatusSuperseded
		rec.SupersededBy = active.ID
		if err := insertMemory(ctx, tx, rec); err != nil {
			return WriteResult{}, MemoryRecord{}, nil, err
		}
		stored = rec
		result = WriteResult{ID: rec.ID, Outcome: WriteRejected, SupersededRecord: active.ID}

	default:
		res, err := tx.ExecContext(ctx,
			`UPDATE qa_memories
			 SET status='superseded',superseded_by=?,updated_at=?
			 WHERE id=? AND user_id=? AND status='active'`,
			rec.ID, rec.UpdatedAt, active.ID, rec.UserID,
		)
		if err != nil {
			return WriteResult{}, MemoryRecord{}, nil, fmt.Errorf("memory: supersede fact %q: %w", rec.FactKey, err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return WriteResult{}, MemoryRecord{}, nil, fmt.Errorf("memory: inspect supersede fact %q: %w", rec.FactKey, err)
		}
		if affected != 1 {
			return WriteResult{}, MemoryRecord{}, nil, fmt.Errorf("memory: active fact %q changed during write", rec.FactKey)
		}
		if err := insertMemory(ctx, tx, rec); err != nil {
			return WriteResult{}, MemoryRecord{}, nil, err
		}
		active.Status = StatusSuperseded
		active.SupersededBy = rec.ID
		active.UpdatedAt = rec.UpdatedAt
		replaced = active
		result = WriteResult{ID: rec.ID, Outcome: WriteSuperseded, SupersededRecord: active.ID}
	}

	if err := tx.Commit(); err != nil {
		return WriteResult{}, MemoryRecord{}, nil, fmt.Errorf("memory: commit fact %q: %w", rec.FactKey, err)
	}
	return result, stored, replaced, nil
}

func getActiveFact(ctx context.Context, tx *sql.Tx, userID int64, factKey string) (*MemoryRecord, error) {
	row := tx.QueryRowContext(ctx,
		`SELECT `+memorySelectColumns+`
		 FROM qa_memories
		 WHERE user_id=? AND fact_key=? AND status='active'
		 FOR UPDATE`,
		userID, factKey,
	)
	rec, err := scanMemory(row.Scan)
	if err != nil {
		return nil, fmt.Errorf("memory: load active fact %q: %w", factKey, err)
	}
	return rec, nil
}

func insertMemory(ctx context.Context, tx *sql.Tx, rec MemoryRecord) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO qa_memories(
			id,user_id,fact_key,kind,content,source_type,authority,status,superseded_by,
			source_session,confidence,expires_at,created_at,updated_at,last_used,use_count
		 ) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		rec.ID, rec.UserID, rec.FactKey, rec.Kind, rec.Content, rec.SourceType, rec.Authority,
		rec.Status, nullableString(rec.SupersededBy), rec.SourceSession, rec.Confidence, rec.ExpiresAt,
		rec.CreatedAt, rec.UpdatedAt, rec.LastUsed, rec.UseCount,
	)
	if err != nil {
		return fmt.Errorf("memory: insert fact %q: %w", rec.FactKey, err)
	}
	return nil
}

func (memory *MemoryStore) syncVector(ctx context.Context, rec MemoryRecord) bool {
	if !memory.Enabled() {
		return false
	}
	if err := memory.vectorize(ctx, rec); err != nil {
		log.WarnfCtx(ctx, "[memory] vector sync failed id=%s status=%s: %v", rec.ID, rec.Status, err)
		return false
	}
	return true
}

func (memory *MemoryStore) vectorize(ctx context.Context, rec MemoryRecord) error {
	vecs, err := memory.embedder.Embed(ctx, []string{rec.Content})
	if err != nil {
		return fmt.Errorf("embed memory %q: %w", rec.ID, err)
	}
	if len(vecs) != 1 {
		return fmt.Errorf("embed memory %q: expected one vector, got %d", rec.ID, len(vecs))
	}
	payload := map[string]any{
		"kind":        "memory",
		"memory_id":   rec.ID,
		"user_id":     rec.UserID,
		"fact_key":    rec.FactKey,
		"source_type": string(rec.SourceType),
		"status":      string(rec.Status),
	}
	return memory.semantic.Upsert(ctx, []qstore.SemanticPoint{
		{ID: rec.ID, Vector: vecs[0], Payload: payload},
	})
}

func isDuplicateKey(err error) bool {
	var mysqlErr *mysqlDriver.MySQLError
	return errors.As(err, &mysqlErr) && mysqlErr.Number == 1062
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

type scanner func(dest ...any) error

func scanMemory(scan scanner) (*MemoryRecord, error) {
	var rec MemoryRecord
	var supersededBy, sourceSession sql.NullString
	var expiresAt, createdAt, updatedAt, lastUsed sql.NullTime
	err := scan(
		&rec.ID, &rec.UserID, &rec.FactKey, &rec.Kind, &rec.Content, &rec.SourceType,
		&rec.Authority, &rec.Status, &supersededBy, &sourceSession, &rec.Confidence,
		&expiresAt, &createdAt, &updatedAt, &lastUsed, &rec.UseCount,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	rec.SupersededBy = supersededBy.String
	rec.SourceSession = sourceSession.String
	rec.ExpiresAt = nullTimePtr(expiresAt)
	rec.CreatedAt = createdAt.Time.UTC()
	rec.UpdatedAt = updatedAt.Time.UTC()
	rec.LastUsed = nullTimePtr(lastUsed)
	return &rec, nil
}

func nullTimePtr(value sql.NullTime) *time.Time {
	if !value.Valid {
		return nil
	}
	utc := value.Time.UTC()
	return &utc
}

func placeholders(count int) string {
	if count <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", count), ",")
}
