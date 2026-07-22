package mysql

import (
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestOpenRequiresDSN(t *testing.T) {
	_, err := Open(" ")
	if err == nil || !strings.Contains(err.Error(), "DSN is required") {
		t.Fatalf("error = %v", err)
	}
}

func TestConfigurePoolSetsSharedConnectionLimit(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	configurePool(db)
	if got := db.Stats().MaxOpenConnections; got != 20 {
		t.Fatalf("max open connections = %d, want 20", got)
	}
}
