package store

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestInterruptActiveImplementationsOnlyClaimsExpiredLeases(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Date(2026, 7, 28, 10, 0, 0, 0, time.UTC)
	retainUntil := now.Add(72 * time.Hour)
	mock.ExpectQuery(`(?s)SELECT id FROM feature_implementation_runs.*status IN \('preparing','running','validating'\).*lease_expires_at IS NULL OR lease_expires_at<=\?.*LIMIT 100`).
		WithArgs(now).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("run-expired").AddRow("run-raced"))
	mock.ExpectExec(`(?s)UPDATE feature_implementation_runs.*error_summary='worker lease expired'.*lease_expires_at IS NULL OR lease_expires_at<=\?`).
		WithArgs(now, retainUntil, "run-expired", now).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`(?s)UPDATE feature_implementation_runs.*error_summary='worker lease expired'.*lease_expires_at IS NULL OR lease_expires_at<=\?`).
		WithArgs(now, retainUntil, "run-raced", now).
		WillReturnResult(sqlmock.NewResult(0, 0))

	store := NewFeatureDeliveryStore(db)
	ids, err := store.InterruptActiveImplementations(context.Background(), now, retainUntil)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != "run-expired" {
		t.Fatalf("interrupted IDs = %v", ids)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
