package agentworkflow

import (
	"context"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestListEventsBoundsReadAtStorageBoundary(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	workflowStore, err := NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT
		workflow_run_id,seq,kind,node_id,summary,detail_json,created_at
		FROM workflow_events
		WHERE workflow_run_id=? AND seq>?
		ORDER BY seq LIMIT ?`)).
		WithArgs("run_1", int64(4), 100).
		WillReturnRows(sqlmock.NewRows([]string{
			"workflow_run_id", "seq", "kind", "node_id", "summary", "detail_json", "created_at",
		}))
	if _, err := workflowStore.ListEvents(context.Background(), "run_1", 4, 1000); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
