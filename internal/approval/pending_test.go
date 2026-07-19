package approval

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/dekwanlabs/nasuta/internal/auth"
	"github.com/dekwanlabs/nasuta/tool"
	"github.com/dekwanlabs/nasuta/writeaction"
)

func TestGetUsesPendingActionScanner(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now()
	mock.ExpectQuery(`SELECT id,tool,incident_id,args_json.*FROM pending_actions WHERE id=\?`).
		WithArgs("act-1").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "tool", "incident_id", "args_json", "rationale", "impact", "status", "requested_by",
			"approver", "result_json", "created_at", "decided_at", "expires_at",
		}).AddRow("act-1", "propose", "inc-1", `{"branch":"fix"}`, "why", "low", ActionPending, 7, nil, nil, now, nil, now.Add(time.Hour)))

	service := &Service{db: db}
	action, err := service.Get("act-1")
	if err != nil {
		t.Fatal(err)
	}
	if action.ID != "act-1" || action.Args["branch"] != "fix" {
		t.Fatalf("action = %#v", action)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestProposeRequiresAdministratorAndRecordsRequester(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	service := &Service{db: db, ttl: time.Hour}
	proposal := writeaction.Proposal{
		ToolID: "propose_branch", IncidentID: "inc-1",
		Arguments: tool.Arguments{"incident_id": "inc-1"},
		Rationale: "repair incident", Impact: "assignee=alice",
	}
	if _, err := service.Propose(context.Background(), proposal); err == nil {
		t.Fatal("unauthenticated proposal was accepted")
	}

	mock.ExpectExec(`INSERT INTO pending_actions`).
		WithArgs(sqlmock.AnyArg(), "propose_branch", "inc-1", `{"incident_id":"inc-1"}`, "repair incident", "assignee=alice", ActionPending, int64(7), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	ctx := auth.WithUser(context.Background(), &auth.User{ID: 7, IsAdmin: true})
	if _, err := service.Propose(ctx, proposal); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
