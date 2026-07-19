package incident

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestListUsesOneSummaryQuery(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now()
	mock.ExpectQuery(regexp.QuoteMeta(`
SELECT id,dedup_key,created_at,updated_at,status,source,alert_title,affected_svcs_json,
       root_cause,solution,assigned_to,fix_branches_json,fix_started_at,fixed_at
FROM incident_records ORDER BY created_unix DESC LIMIT 200`)).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "dedup_key", "created_at", "updated_at", "status", "source", "alert_title",
			"affected_svcs_json", "root_cause", "solution", "assigned_to", "fix_branches_json", "fix_started_at", "fixed_at",
		}).AddRow("inc-1", "dedup", now, now, StatusOpen, "alert", "title", `["svc-a"]`, "cause", "solution", "owner", `[]`, nil, nil))

	manager := &Manager{db: db}
	incidents, err := manager.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(incidents) != 1 || incidents[0].ID != "inc-1" || len(incidents[0].AffectedSvcs) != 1 {
		t.Fatalf("incidents = %#v", incidents)
	}
	if incidents[0].AlertPayload != nil || incidents[0].ErrorLogs != nil || incidents[0].Traces != nil || incidents[0].AnalysisDoc != "" {
		t.Fatalf("list loaded detail-only fields: %#v", incidents[0])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
