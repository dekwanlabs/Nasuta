package app

import (
	"errors"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestInitRunStoreFailsWhenRunSchemaIsIncompatible(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectBegin()
	mock.ExpectQuery("FROM agent_delegation_tasks").
		WillReturnError(errors.New("unknown column r.cost_micros"))
	mock.ExpectRollback()

	platform := &Platform{db: db}
	err = platform.initRunStore()
	if err == nil ||
		!strings.Contains(err.Error(), "apply pending docs/sql migrations") ||
		!strings.Contains(err.Error(), "unknown column r.cost_micros") {
		t.Fatalf("initRunStore error = %v", err)
	}
	if platform.qa.runs != nil {
		t.Fatal("initRunStore published a run store after recovery failed")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
