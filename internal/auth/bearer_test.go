package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestIssueSessionReturnsBearerToken(t *testing.T) {
	sqlDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM sessions WHERE user_id=?")).
		WithArgs(int64(7)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO sessions (token,user_id,expires_at) VALUES (?,?,?)")).
		WithArgs(sqlmock.AnyArg(), int64(7), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))

	response := httptest.NewRecorder()
	NewService(nil, NewDB(sqlDB), "", "").issueSession(response, &User{
		ID: 7, Name: "Evaluator", Email: "eva@example.com", IsAdmin: true,
	})

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
	var payload sessionResponse
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload.AccessToken == "" || payload.TokenType != "Bearer" {
		t.Fatalf("token response = %#v", payload)
	}
	if payload.ExpiresIn != int64(sessionTTL.Seconds()) {
		t.Fatalf("expires_in = %d, want %d", payload.ExpiresIn, int64(sessionTTL.Seconds()))
	}
	if payload.User.ID != 7 || payload.User.Email != "eva@example.com" {
		t.Fatalf("user response = %#v", payload.User)
	}
	if cookies := response.Result().Cookies(); len(cookies) != 0 {
		t.Fatalf("login issued cookies: %#v", cookies)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRequestSessionTokenPrefersBearerHeader(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/auth/me", nil)
	request.AddCookie(&http.Cookie{Name: sessionCookie, Value: "cookie-token"})
	request.Header.Set("Authorization", "Bearer header-token")

	if token := requestSessionToken(request); token != "header-token" {
		t.Fatalf("token = %q, want header-token", token)
	}
}

func TestAccessTokenRedirectUsesFragment(t *testing.T) {
	redirect, err := accessTokenRedirect("https://codeloom.example/web", "secret-token")
	if err != nil {
		t.Fatal(err)
	}
	target, err := url.Parse(redirect)
	if err != nil {
		t.Fatal(err)
	}
	if target.RawQuery != "" {
		t.Fatalf("query contains token data: %q", target.RawQuery)
	}
	values, err := url.ParseQuery(target.Fragment)
	if err != nil {
		t.Fatal(err)
	}
	if values.Get("access_token") != "secret-token" || values.Get("token_type") != "Bearer" {
		t.Fatalf("fragment = %q", target.Fragment)
	}
}

func TestLogoutDeletesBearerSession(t *testing.T) {
	sqlDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM sessions WHERE token=?")).
		WithArgs("header-token").
		WillReturnResult(sqlmock.NewResult(0, 1))

	request := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	request.Header.Set("Authorization", "Bearer header-token")
	response := httptest.NewRecorder()
	NewService(nil, NewDB(sqlDB), "", "").Logout(response, request)

	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusNoContent)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
