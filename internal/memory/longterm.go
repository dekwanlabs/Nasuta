package memory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/dekwanlabs/nasuta/internal/platform/embed"
	"github.com/dekwanlabs/nasuta/internal/retrieval"
	"github.com/dekwanlabs/nasuta/internal/semantic"
	"github.com/dekwanlabs/nasuta/log"
	"github.com/dekwanlabs/nasuta/platform"
	mysqlDriver "github.com/go-sql-driver/mysql"
)

type LongTermRecord = MemoryRecord

const memorySelectColumns = `id,user_id,fact_key,kind,content,source_type,authority,status,
	superseded_by,source_session,confidence,expires_at,created_at,updated_at,last_used,use_count`

const (
	memoryBM25RebuildBatch = 64
	supersededHistoryLimit = 5
	supersededPruneBatch   = 64
)

// MemoryStore keeps durable state in MySQL and candidates in the selected semantic store.
type MemoryStore struct {
	db             *sql.DB
	semantic       semantic.Store
	embedder       embed.Embedder
	bm25           *retrieval.BM25Builder
	bm25VocabPath  string
	workContextTTL time.Duration
	now            func() time.Time
}

// NewMemoryStore binds long-term memory to the platform-owned MySQL pool.
func NewMemoryStore(db *sql.DB, semantic semantic.Store, embedder embed.Embedder, workContextTTL time.Duration) *MemoryStore {
	if db == nil {
		return nil
	}
	return newMemoryStore(db, semantic, embedder, workContextTTL)
}

func newMemoryStore(db *sql.DB, semantic semantic.Store, embedder embed.Embedder, workContextTTL time.Duration) *MemoryStore {
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
	return memory.semantic != nil && memory.semantic.Capabilities().Dense &&
		memory.embedder != nil && memory.embedder.Enabled()
}

// Close releases the dedicated semantic backend owned by long-term memory.
func (memory *MemoryStore) Close() error {
	if memory == nil || memory.semantic == nil {
		return nil
	}
	return memory.semantic.Close()
}

// EnableBM25 binds sparse coordinates to the dedicated memory collection.
func (memory *MemoryStore) EnableBM25(ctx context.Context, vocabPath string) error {
	if vocabPath == "" {
		return fmt.Errorf("memory BM25: vocabulary path is required")
	}
	builder, err := retrieval.LoadVocab(vocabPath)
	if err == nil {
		memory.bm25 = builder
		memory.bm25VocabPath = vocabPath
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("memory BM25: load vocabulary %q: %w", vocabPath, err)
	}

	builder = retrieval.NewBM25Builder()
	if err := memory.rebuildVectors(ctx, builder, memoryBM25RebuildBatch); err != nil {
		return fmt.Errorf("memory BM25: rebuild existing memories: %w", err)
	}
	if err := builder.SaveVocab(vocabPath); err != nil {
		return fmt.Errorf("memory BM25: save vocabulary %q: %w", vocabPath, err)
	}
	memory.bm25 = builder
	memory.bm25VocabPath = vocabPath
	return nil
}

// Write applies authority-based replacement and then refreshes vector candidates.
func (memory *MemoryStore) Write(ctx context.Context, incoming MemoryRecord) (WriteResult, error) {
	now := memory.now().UTC()
	rec, err := canonicalizeRecord(incoming)
	if err != nil {
		return WriteResult{}, err
	}
	if rec.Kind == KindWorkContext && rec.ExpiresAt == nil {
		expiresAt := now.Add(memory.workContextTTL)
		rec.ExpiresAt = &expiresAt
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
	var prunedIDs []string
	for attempt := 0; attempt < 2; attempt++ {
		result, stored, replaced, prunedIDs, err = memory.writeTransaction(ctx, rec)
		if err == nil || !isDuplicateKey(err) {
			break
		}
	}
	if err != nil {
		return WriteResult{}, err
	}

	vectorSynced := true
	if err := memory.deleteVectors(ctx, prunedIDs); err != nil {
		log.WarnfCtx(ctx, "[memory] pruned vector cleanup failed fact_key=%s count=%d: %v", rec.FactKey, len(prunedIDs), err)
		vectorSynced = false
	}
	if replaced != nil {
		vectorSynced = memory.syncVector(ctx, *replaced) && vectorSynced
	}
	vectorSynced = memory.syncVector(ctx, stored) && vectorSynced
	result.VectorSynced = vectorSynced
	return result, nil
}

func (memory *MemoryStore) writeTransaction(ctx context.Context, rec MemoryRecord) (WriteResult, MemoryRecord, *MemoryRecord, []string, error) {
	tx, err := memory.db.BeginTx(ctx, nil)
	if err != nil {
		return WriteResult{}, MemoryRecord{}, nil, nil, fmt.Errorf("memory: begin write: %w", err)
	}
	defer tx.Rollback()

	active, err := getActiveFact(ctx, tx, rec.UserID, rec.FactKey)
	if err != nil {
		return WriteResult{}, MemoryRecord{}, nil, nil, err
	}

	var result WriteResult
	stored := rec
	var replaced *MemoryRecord
	addedSuperseded := false
	switch {
	case active == nil:
		if err := insertMemory(ctx, tx, rec); err != nil {
			return WriteResult{}, MemoryRecord{}, nil, nil, err
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
			return WriteResult{}, MemoryRecord{}, nil, nil, fmt.Errorf("memory: refresh fact %q: %w", rec.FactKey, err)
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
		duplicate, err := getSupersededFact(ctx, tx, rec.UserID, rec.FactKey, rec.Content)
		if err != nil {
			return WriteResult{}, MemoryRecord{}, nil, nil, err
		}
		if duplicate == nil {
			if err := insertMemory(ctx, tx, rec); err != nil {
				return WriteResult{}, MemoryRecord{}, nil, nil, err
			}
			stored = rec
			addedSuperseded = true
		} else {
			if rec.Authority >= duplicate.Authority {
				duplicate.Kind = rec.Kind
				duplicate.SourceType = rec.SourceType
				duplicate.Authority = rec.Authority
				duplicate.SourceSession = rec.SourceSession
				duplicate.Confidence = max(duplicate.Confidence, rec.Confidence)
				duplicate.ExpiresAt = rec.ExpiresAt
			}
			duplicate.SupersededBy = active.ID
			duplicate.UpdatedAt = rec.UpdatedAt
			duplicate.UseCount++
			if err := refreshSupersededFact(ctx, tx, *duplicate); err != nil {
				return WriteResult{}, MemoryRecord{}, nil, nil, err
			}
			stored = *duplicate
		}
		result = WriteResult{ID: stored.ID, Outcome: WriteRejected, SupersededRecord: active.ID}

	default:
		res, err := tx.ExecContext(ctx,
			`UPDATE qa_memories
			 SET status='superseded',superseded_by=?,updated_at=?
			 WHERE id=? AND user_id=? AND status='active'`,
			rec.ID, rec.UpdatedAt, active.ID, rec.UserID,
		)
		if err != nil {
			return WriteResult{}, MemoryRecord{}, nil, nil, fmt.Errorf("memory: supersede fact %q: %w", rec.FactKey, err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return WriteResult{}, MemoryRecord{}, nil, nil, fmt.Errorf("memory: inspect supersede fact %q: %w", rec.FactKey, err)
		}
		if affected != 1 {
			return WriteResult{}, MemoryRecord{}, nil, nil, fmt.Errorf("memory: active fact %q changed during write", rec.FactKey)
		}
		if err := insertMemory(ctx, tx, rec); err != nil {
			return WriteResult{}, MemoryRecord{}, nil, nil, err
		}
		active.Status = StatusSuperseded
		active.SupersededBy = rec.ID
		active.UpdatedAt = rec.UpdatedAt
		replaced = active
		addedSuperseded = true
		result = WriteResult{ID: rec.ID, Outcome: WriteSuperseded, SupersededRecord: active.ID}
	}

	var prunedIDs []string
	if addedSuperseded {
		prunedIDs, err = pruneSupersededHistory(ctx, tx, rec.UserID, rec.FactKey)
		if err != nil {
			return WriteResult{}, MemoryRecord{}, nil, nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return WriteResult{}, MemoryRecord{}, nil, nil, fmt.Errorf("memory: commit fact %q: %w", rec.FactKey, err)
	}
	return result, stored, replaced, prunedIDs, nil
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

func getSupersededFact(ctx context.Context, tx *sql.Tx, userID int64, factKey, content string) (*MemoryRecord, error) {
	row := tx.QueryRowContext(ctx,
		`SELECT `+memorySelectColumns+`
		 FROM qa_memories
		 WHERE user_id=? AND fact_key=? AND content=? AND status='superseded'
		 ORDER BY updated_at DESC,id DESC
		 LIMIT 1
		 FOR UPDATE`,
		userID, factKey, content,
	)
	rec, err := scanMemory(row.Scan)
	if err != nil {
		return nil, fmt.Errorf("memory: load superseded fact %q: %w", factKey, err)
	}
	return rec, nil
}

func refreshSupersededFact(ctx context.Context, tx *sql.Tx, rec MemoryRecord) error {
	_, err := tx.ExecContext(ctx,
		`UPDATE qa_memories
		 SET kind=?,source_type=?,authority=?,source_session=?,confidence=?,
		     expires_at=?,superseded_by=?,updated_at=?,use_count=?
		 WHERE id=? AND user_id=? AND status='superseded'`,
		rec.Kind, rec.SourceType, rec.Authority, rec.SourceSession, rec.Confidence,
		rec.ExpiresAt, rec.SupersededBy, rec.UpdatedAt, rec.UseCount, rec.ID, rec.UserID,
	)
	if err != nil {
		return fmt.Errorf("memory: refresh superseded fact %q: %w", rec.FactKey, err)
	}
	return nil
}

func pruneSupersededHistory(ctx context.Context, tx *sql.Tx, userID int64, factKey string) ([]string, error) {
	pruned := make([]string, 0)
	for {
		rows, err := tx.QueryContext(ctx,
			`SELECT id
			 FROM qa_memories
			 WHERE user_id=? AND fact_key=? AND status='superseded'
			 ORDER BY updated_at DESC,id DESC
			 LIMIT ? OFFSET ?
			 FOR UPDATE`,
			userID, factKey, supersededPruneBatch, supersededHistoryLimit,
		)
		if err != nil {
			return nil, fmt.Errorf("memory: list superseded history %q: %w", factKey, err)
		}
		ids := make([]string, 0, supersededPruneBatch)
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, fmt.Errorf("memory: scan superseded history %q: %w", factKey, err)
			}
			ids = append(ids, id)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, fmt.Errorf("memory: iterate superseded history %q: %w", factKey, err)
		}
		if err := rows.Close(); err != nil {
			return nil, fmt.Errorf("memory: close superseded history %q: %w", factKey, err)
		}
		if len(ids) == 0 {
			return pruned, nil
		}

		args := make([]any, 0, len(ids)+2)
		args = append(args, userID, factKey)
		for _, id := range ids {
			args = append(args, id)
		}
		result, err := tx.ExecContext(ctx,
			`DELETE FROM qa_memories
			 WHERE user_id=? AND fact_key=? AND status='superseded'
			   AND id IN (`+placeholders(len(ids))+`)`,
			args...,
		)
		if err != nil {
			return nil, fmt.Errorf("memory: prune superseded history %q: %w", factKey, err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return nil, fmt.Errorf("memory: inspect pruned history %q: %w", factKey, err)
		}
		if affected != int64(len(ids)) {
			return nil, fmt.Errorf("memory: expected to prune %d histories for %q, pruned %d", len(ids), factKey, affected)
		}
		pruned = append(pruned, ids...)
		if len(ids) < supersededPruneBatch {
			return pruned, nil
		}
	}
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
	var sparse *semantic.SparseVector
	if memory.bm25 != nil {
		sparse = buildMemorySparse(memory.bm25, rec)
		if err := memory.bm25.SaveVocab(memory.bm25VocabPath); err != nil {
			return fmt.Errorf("save memory BM25 vocabulary %q: %w", memory.bm25VocabPath, err)
		}
	}
	return memory.semantic.Upsert(ctx, []semantic.Record{
		{ID: rec.ID, DenseVector: vecs[0], SparseVector: sparse, Metadata: memoryVectorPayload(rec)},
	})
}

func (memory *MemoryStore) rebuildVectors(ctx context.Context, builder *retrieval.BM25Builder, batchSize int) error {
	if !memory.Enabled() {
		return fmt.Errorf("semantic recall unavailable")
	}
	if batchSize <= 0 {
		return fmt.Errorf("batch size must be positive")
	}
	cursor := ""
	for {
		records, err := memory.loadVectorRecords(ctx, cursor, batchSize)
		if err != nil {
			return err
		}
		if len(records) == 0 {
			return nil
		}
		texts := make([]string, len(records))
		for i := range records {
			texts[i] = records[i].Content
		}
		vectors, err := memory.embedder.Embed(ctx, texts)
		if err != nil {
			return fmt.Errorf("embed batch after %q: %w", cursor, err)
		}
		if len(vectors) != len(records) {
			return fmt.Errorf("embed batch after %q: got %d vectors for %d memories", cursor, len(vectors), len(records))
		}
		points := make([]semantic.Record, len(records))
		for i := range records {
			points[i] = semantic.Record{
				ID:           records[i].ID,
				DenseVector:  vectors[i],
				SparseVector: buildMemorySparse(builder, records[i]),
				Metadata:     memoryVectorPayload(records[i]),
			}
		}
		if err := memory.semantic.Upsert(ctx, points); err != nil {
			return fmt.Errorf("upsert batch after %q: %w", cursor, err)
		}
		cursor = records[len(records)-1].ID
		if len(records) < batchSize {
			return nil
		}
	}
}

func (memory *MemoryStore) loadVectorRecords(ctx context.Context, afterID string, limit int) ([]MemoryRecord, error) {
	rows, err := memory.db.QueryContext(ctx,
		`SELECT id,user_id,fact_key,content,source_type,status
		 FROM qa_memories
		 WHERE id>?
		 ORDER BY id
		 LIMIT ?`,
		afterID, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("load vector records after %q: %w", afterID, err)
	}
	defer rows.Close()

	records := make([]MemoryRecord, 0, limit)
	for rows.Next() {
		var rec MemoryRecord
		if err := rows.Scan(&rec.ID, &rec.UserID, &rec.FactKey, &rec.Content, &rec.SourceType, &rec.Status); err != nil {
			return nil, fmt.Errorf("scan vector record: %w", err)
		}
		records = append(records, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate vector records: %w", err)
	}
	return records, nil
}

func buildMemorySparse(builder *retrieval.BM25Builder, rec MemoryRecord) *semantic.SparseVector {
	tokens := builder.AddDoc(rec.FactKey + "\n" + rec.Content)
	indices, values := retrieval.SparseToSorted(builder.BuildSparse(tokens))
	if len(indices) == 0 {
		return nil
	}
	return &semantic.SparseVector{Indices: indices, Values: values}
}

func memoryVectorPayload(rec MemoryRecord) map[string]any {
	return map[string]any{
		"kind":        "memory",
		"memory_id":   rec.ID,
		"user_id":     rec.UserID,
		"fact_key":    rec.FactKey,
		"source_type": string(rec.SourceType),
		"status":      string(rec.Status),
	}
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
