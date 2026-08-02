package store

import (
	"database/sql/driver"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/dekwanlabs/nasuta/internal/domain"
)

func TestDocToRunbookUsesDocumentStoreID(t *testing.T) {
	got := docToRunbook(domain.DocRecord{
		ID: "doc-b3da2891a54da9ca", Title: "flow-system-overview", Filename: "flow-system-overview.md", Kind: domain.DocKindFlow,
		Content: "---\nid: flow-system-overview\ntags: [flow, architecture, gateway]\n---\n# Flow: System Architecture Overview\n\nBody\n",
	})
	if got.ID != "doc-b3da2891a54da9ca" {
		t.Fatalf("runbook ID = %q, want document store ID", got.ID)
	}
	if strings.Contains(got.Text, "id: flow-system-overview") {
		t.Fatalf("runbook text contains frontmatter: %q", got.Text)
	}
}

func TestRunbookMetaByIDUsesNarrowQuery(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	kinds := make([]driver.Value, 0, len(domain.RunbookKinds)+1)
	kinds = append(kinds, "doc-a")
	for _, kind := range domain.RunbookKinds {
		kinds = append(kinds, kind)
	}
	mock.ExpectQuery(regexp.QuoteMeta(
		`SELECT id, title, filename, COALESCE(kind,'')
		 FROM documents WHERE id = ? AND kind IN (` + regexpPlaceholders(len(domain.RunbookKinds)) + `)`,
	)).WithArgs(kinds...).WillReturnRows(
		sqlmock.NewRows([]string{"id", "title", "filename", "kind"}).
			AddRow("doc-a", "Architecture", "docs/a.md", "flow"),
	)

	got, err := NewDocStore(db).RunbookMetaByID("doc-a")
	if err != nil {
		t.Fatalf("RunbookMetaByID: %v", err)
	}
	if got.ID != "doc-a" || got.Title != "Architecture" || got.Path != "docs/a.md" || got.Text != "" {
		t.Fatalf("runbook metadata = %#v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestSearchRunbooksKeywordUsesStorageLimit(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	args := make([]driver.Value, 0, len(domain.RunbookKinds)+4)
	for _, kind := range domain.RunbookKinds {
		args = append(args, kind)
	}
	args = append(args, "%gateway%", "%gateway%", "%gateway%", 3)
	mock.ExpectQuery(`(?s)SELECT id, title, filename, COALESCE\(kind,''\), COALESCE\(content,''\).*ORDER BY updated_at DESC LIMIT \?`).
		WithArgs(args...).
		WillReturnRows(sqlmock.NewRows([]string{"id", "title", "filename", "kind", "content"}))

	got, err := NewDocStore(db).SearchRunbooksKeyword("gateway", 3)
	if err != nil {
		t.Fatalf("SearchRunbooksKeyword: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("results = %#v, want empty", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func regexpPlaceholders(n int) string {
	if n == 0 {
		return ""
	}
	out := "?"
	for range n - 1 {
		out += ",?"
	}
	return out
}
