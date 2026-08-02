package rbac

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestRevokeMCPKeyUsesKeyIDPathValue(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectExec(`UPDATE rbac_mcp_keys SET is_active=0 WHERE id=\? AND user_id=\?`).
		WithArgs(int64(9), int64(42)).
		WillReturnResult(sqlmock.NewResult(0, 1))

	request := httptest.NewRequest(http.MethodDelete, "/api/rbac/users/42/keys/9", nil)
	request.SetPathValue("id", "42")
	request.SetPathValue("keyID", "9")
	response := httptest.NewRecorder()
	NewHandler(&Store{db: db}).RevokeMCPKey(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
