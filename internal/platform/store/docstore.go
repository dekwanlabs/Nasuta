package store

import (
	"database/sql"
	"fmt"
	"regexp"
	"strings"

	"github.com/dekwanlabs/astris/internal/platform/dbschema"
	_ "github.com/go-sql-driver/mysql"
	"gopkg.in/yaml.v3"

	"github.com/dekwanlabs/astris/internal/domain"
	"github.com/dekwanlabs/astris/log"
)

// DocStore persists uploaded and generated markdown documents in MySQL.
type DocStore struct {
	db *sql.DB
}

// OpenDocStore opens the document store when MySQL is configured.
func OpenDocStore(dsn string) (*DocStore, error) {
	if dsn == "" {
		return nil, nil
	}
	db, err := MySQL(dsn)
	if err != nil {
		return nil, fmt.Errorf("docstore open: %w", err)
	}
	if err := dbschema.MigrateMySQL(db, dbschema.GroupDocuments); err != nil {
		return nil, fmt.Errorf("docstore migrate: %w", err)
	}
	if err := safeAddColumn(db, "documents", "kind", "VARCHAR(32) NOT NULL DEFAULT 'document'"); err != nil {
		return nil, fmt.Errorf("docstore migrate kind column: %w", err)
	}
	return &DocStore{db: db}, nil
}

func (s *DocStore) Close() error { return nil }

// safeAddColumn makes additive schema changes idempotent.
func safeAddColumn(db *sql.DB, table, column, def string) error {
	_, err := db.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, def))
	if err != nil {
		if strings.Contains(err.Error(), "Duplicate column") || strings.Contains(err.Error(), "1060") {
			return nil
		}
		return err
	}
	return nil
}

func (s *DocStore) InsertDoc(doc types.DocRecord) error {
	if doc.Kind == "" {
		doc.Kind = types.DocKindDocument
	}
	_, err := s.db.Exec(
		`INSERT INTO documents (id, title, filename, kind, content, chunk_count, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON DUPLICATE KEY UPDATE title=VALUES(title), filename=VALUES(filename),
		 kind=VALUES(kind), content=VALUES(content), chunk_count=VALUES(chunk_count), updated_at=VALUES(updated_at)`,
		doc.ID, doc.Title, doc.Filename, doc.Kind, doc.Content, doc.ChunkCount, DatabaseTime(doc.CreatedAt), DatabaseTime(doc.UpdatedAt),
	)
	if err != nil {
		return fmt.Errorf("insert doc: %w", err)
	}
	return nil
}

func (s *DocStore) GetDoc(id string) (types.DocRecord, error) {
	var d types.DocRecord
	var createdAt, updatedAt sql.NullTime
	err := s.db.QueryRow(
		`SELECT id, title, filename, COALESCE(kind,''), COALESCE(content,''), chunk_count, created_at, updated_at
		 FROM documents WHERE id = ?`, id,
	).Scan(&d.ID, &d.Title, &d.Filename, &d.Kind, &d.Content, &d.ChunkCount, &createdAt, &updatedAt)
	if err != nil {
		return d, err
	}
	d.CreatedAt, d.UpdatedAt = FormatDatabaseTime(createdAt), FormatDatabaseTime(updatedAt)
	return d, nil
}

func (s *DocStore) ListDocs() ([]types.DocRecord, error) {
	rows, err := s.db.Query(
		`SELECT id, title, filename, COALESCE(kind,''), COALESCE(content,''), chunk_count, created_at, updated_at
		 FROM documents ORDER BY updated_at DESC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanDocRows(rows)
}

func (s *DocStore) ListDocsMetaPage(page, pageSize int) (*types.Page[types.DocRecord], error) {
	return s.ListDocsMetaPageFiltered(page, pageSize, "", "", "", "")
}

func (s *DocStore) ListDocsMetaPageFiltered(page, pageSize int, kind, query, sortBy, sortOrder string) (*types.Page[types.DocRecord], error) {
	if page < 1 {
		page = 1
	}
	if pageSize <= 0 || pageSize > 200 {
		pageSize = 20
	}

	var (
		where []string
		args  []any
	)
	if kind = strings.TrimSpace(kind); kind != "" && kind != "all" {
		where = append(where, "kind = ?")
		args = append(args, kind)
	}
	if query = strings.TrimSpace(query); query != "" {
		like := "%" + query + "%"
		where = append(where, "(title LIKE ? OR filename LIKE ? OR content LIKE ?)")
		args = append(args, like, like, like)
	}
	cond := ""
	if len(where) > 0 {
		cond = " WHERE " + strings.Join(where, " AND ")
	}

	var total int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM documents`+cond, args...).Scan(&total); err != nil {
		return nil, err
	}

	offset := (page - 1) * pageSize
	listArgs := append(append([]any{}, args...), pageSize, offset)
	orderBy := docListOrderBy(sortBy, sortOrder)
	rows, err := s.db.Query(
		`SELECT id, title, filename, COALESCE(kind,''), chunk_count, created_at, updated_at
		 FROM documents`+cond+` `+orderBy+` LIMIT ? OFFSET ?`,
		listArgs...,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	list, err := scanDocMetaRows(rows)
	if err != nil {
		return nil, err
	}
	return &types.Page[types.DocRecord]{
		Total:    total,
		Page:     page,
		PageSize: pageSize,
		List:     list,
	}, nil
}

func docListOrderBy(sortBy, sortOrder string) string {
	dir := "DESC"
	switch strings.ToLower(strings.TrimSpace(sortOrder)) {
	case "asc":
		dir = "ASC"
	case "desc":
		dir = "DESC"
	}

	switch strings.ToLower(strings.TrimSpace(sortBy)) {
	case "title":
		return fmt.Sprintf("ORDER BY LOWER(title) %s, updated_at DESC, id DESC", dir)
	case "filename":
		return fmt.Sprintf("ORDER BY LOWER(filename) %s, updated_at DESC, id DESC", dir)
	case "chunk_count", "chunkcount", "chunks":
		return fmt.Sprintf("ORDER BY chunk_count %s, updated_at DESC, id DESC", dir)
	case "module":
		return fmt.Sprintf(
			"ORDER BY CASE WHEN kind = 'module' THEN 0 ELSE 1 END ASC, LOWER(CASE WHEN kind = 'module' THEN filename ELSE title END) %s, updated_at DESC, id DESC",
			dir,
		)
	case "time", "updated_at", "updatedat", "":
		fallthrough
	default:
		if strings.TrimSpace(sortBy) == "" && strings.TrimSpace(sortOrder) == "" {
			return "ORDER BY updated_at DESC, id DESC"
		}
		return fmt.Sprintf("ORDER BY updated_at %s, id DESC", dir)
	}
}

func (s *DocStore) ListDocsMeta() ([]types.DocRecord, error) {
	rows, err := s.db.Query(
		`SELECT id, title, filename, COALESCE(kind,''), chunk_count, created_at, updated_at
		 FROM documents ORDER BY updated_at DESC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanDocMetaRows(rows)
}

func (s *DocStore) DeleteDoc(id string) (string, error) {
	_, err := s.db.Exec(`DELETE FROM documents WHERE id = ?`, id)
	if err != nil {
		return id, fmt.Errorf("delete doc: %w", err)
	}
	log.Infof("[docstore] deleted document %s", id)
	return id, nil
}

func (s *DocStore) ListDocsByKind(kind string) ([]types.DocRecord, error) {
	rows, err := s.db.Query(
		`SELECT id, title, filename, COALESCE(kind,''), COALESCE(content,''), chunk_count, created_at, updated_at
		 FROM documents WHERE kind = ? ORDER BY updated_at DESC`, kind,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanDocRows(rows)
}

func (s *DocStore) ListDocsMetaByKind(kind string) ([]types.DocRecord, error) {
	rows, err := s.db.Query(
		`SELECT id, title, filename, COALESCE(kind,''), chunk_count, created_at, updated_at
		 FROM documents WHERE kind = ? ORDER BY updated_at DESC`, kind,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanDocMetaRows(rows)
}

func (s *DocStore) ListDocsByKinds(kinds []string) ([]types.DocRecord, error) {
	if len(kinds) == 0 {
		return nil, nil
	}
	args := make([]any, len(kinds))
	for i, k := range kinds {
		args[i] = k
	}
	ph := strings.TrimRight(strings.Repeat("?,", len(kinds)), ",")
	rows, err := s.db.Query(
		`SELECT id, title, filename, COALESCE(kind,''), COALESCE(content,''), chunk_count, created_at, updated_at
		 FROM documents WHERE kind IN (`+ph+`) ORDER BY updated_at DESC`,
		args...,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanDocRows(rows)
}

func (s *DocStore) ListDocsMetaByKinds(kinds []string) ([]types.DocRecord, error) {
	if len(kinds) == 0 {
		return nil, nil
	}
	args := make([]any, len(kinds))
	for i, k := range kinds {
		args[i] = k
	}
	ph := strings.TrimRight(strings.Repeat("?,", len(kinds)), ",")
	rows, err := s.db.Query(
		`SELECT id, title, filename, COALESCE(kind,''), chunk_count, created_at, updated_at
		 FROM documents WHERE kind IN (`+ph+`) ORDER BY updated_at DESC`,
		args...,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanDocMetaRows(rows)
}

func scanDocRows(rows *sql.Rows) ([]types.DocRecord, error) {
	var docs []types.DocRecord
	for rows.Next() {
		var d types.DocRecord
		var createdAt, updatedAt sql.NullTime
		if err := rows.Scan(&d.ID, &d.Title, &d.Filename, &d.Kind, &d.Content, &d.ChunkCount, &createdAt, &updatedAt); err != nil {
			return nil, err
		}
		d.CreatedAt, d.UpdatedAt = FormatDatabaseTime(createdAt), FormatDatabaseTime(updatedAt)
		docs = append(docs, d)
	}
	return docs, rows.Err()
}

func scanDocMetaRows(rows *sql.Rows) ([]types.DocRecord, error) {
	var docs []types.DocRecord
	for rows.Next() {
		var d types.DocRecord
		var createdAt, updatedAt sql.NullTime
		if err := rows.Scan(&d.ID, &d.Title, &d.Filename, &d.Kind, &d.ChunkCount, &createdAt, &updatedAt); err != nil {
			return nil, err
		}
		d.CreatedAt, d.UpdatedAt = FormatDatabaseTime(createdAt), FormatDatabaseTime(updatedAt)
		docs = append(docs, d)
	}
	return docs, rows.Err()
}

func (s *DocStore) CountRunbooks() (int, error) {
	ph := strings.TrimRight(strings.Repeat("?,", len(types.RunbookKinds)), ",")
	args := make([]any, len(types.RunbookKinds))
	for i, k := range types.RunbookKinds {
		args[i] = k
	}
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM documents WHERE kind IN (`+ph+`)`, args...).Scan(&n)
	return n, err
}

func (s *DocStore) CountByKind(kind string) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM documents WHERE kind = ?`, kind).Scan(&n)
	return n, err
}

// RunbookMetas returns metadata-only runbook records for retrieval joins.
func (s *DocStore) RunbookMetas() ([]types.RunbookRecord, error) {
	metas, err := s.ListDocsMetaByKinds(types.RunbookKinds)
	if err != nil {
		return nil, err
	}
	out := make([]types.RunbookRecord, 0, len(metas))
	for _, d := range metas {
		out = append(out, types.RunbookRecord{
			ID:         d.ID,
			Repo:       "docs",
			Title:      d.Title,
			Path:       d.Filename,
			Scope:      d.Kind,
			Confidence: 1,
		})
	}
	return out, nil
}

// RunbookByID loads one runbook body and frontmatter.
func (s *DocStore) RunbookByID(id string) (types.RunbookRecord, error) {
	d, err := s.GetDoc(id)
	if err != nil {
		return types.RunbookRecord{}, err
	}
	return docToRunbook(d), nil
}

// docToRunbook converts a stored markdown doc to a runbook record.
func docToRunbook(d types.DocRecord) types.RunbookRecord {
	fm := parseDocFrontmatter(d.Content)
	id := fmScalar(fm.data, "id")
	if id == "" {
		id = d.ID
	}
	title := extractMarkdownTitle(fm.content)
	if title == "" {
		title = d.Title
	}
	return types.RunbookRecord{
		ID:         id,
		Repo:       "docs",
		Title:      title,
		Path:       d.Filename,
		Scope:      d.Kind, // scope mirrors kind verbatim
		Tags:       fmScalarArray(fm.data, "tags"),
		Text:       fm.content,
		Confidence: 1,
	}
}

type docFrontmatter struct {
	data    map[string]any
	content string
}

var docFmRe = regexp.MustCompile(`(?s)^---\r?\n(.*?)\r?\n---\r?\n?(.*)$`)

func parseDocFrontmatter(raw string) docFrontmatter {
	m := docFmRe.FindStringSubmatch(raw)
	if m == nil {
		return docFrontmatter{data: map[string]any{}, content: raw}
	}
	data := map[string]any{}
	_ = yaml.Unmarshal([]byte(m[1]), &data)
	if data == nil {
		data = map[string]any{}
	}
	return docFrontmatter{data: data, content: m[2]}
}

func fmScalar(data map[string]any, key string) string {
	if v, ok := data[key]; ok {
		if s, ok := v.(string); ok {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

func fmScalarArray(data map[string]any, key string) []string {
	v, ok := data[key]
	if !ok {
		return nil
	}
	switch t := v.(type) {
	case []any:
		out := make([]string, 0, len(t))
		for _, item := range t {
			if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, strings.TrimSpace(s))
			}
		}
		return out
	case string:
		if strings.TrimSpace(t) != "" {
			return []string{strings.TrimSpace(t)}
		}
	}
	return nil
}

var docTitleRe = regexp.MustCompile(`(?m)^#\s+(.+)$`)

func extractMarkdownTitle(content string) string {
	if m := docTitleRe.FindStringSubmatch(content); m != nil {
		return strings.TrimSpace(m[1])
	}
	return ""
}
