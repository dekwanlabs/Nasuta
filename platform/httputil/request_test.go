package httputil

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func queryReq(raw string) *http.Request {
	return httptest.NewRequest(http.MethodGet, "/x?"+raw, nil)
}

func TestQueryInt(t *testing.T) {
	q := Query(queryReq("a=5&b=&c=abc"))
	if got := q.Int("a", 1); got != 5 {
		t.Fatalf("a: want 5, got %d", got)
	}
	if got := q.Int("b", 7); got != 7 {
		t.Fatalf("b (empty): want default 7, got %d", got)
	}
	if q.Err() != nil {
		t.Fatalf("empty value must not error: %v", q.Err())
	}
	if got := q.Int("c", 9); got != 9 {
		t.Fatalf("c (invalid): want default 9, got %d", got)
	}
	if q.Err() == nil {
		t.Fatal("invalid integer must record an error")
	}
}

func TestQueryBool(t *testing.T) {
	q := Query(queryReq("a=1&b=true&c=YES&d=false&e=&f=0"))
	for _, k := range []string{"a", "b", "c"} {
		if !q.Bool(k) {
			t.Fatalf("%s should be truthy", k)
		}
	}
	for _, k := range []string{"d", "e", "f", "missing"} {
		if q.Bool(k) {
			t.Fatalf("%s should be falsy", k)
		}
	}
}

func TestQueryPage(t *testing.T) {
	tests := []struct {
		name     string
		raw      string
		wantPage int
		wantSize int
		wantErr  bool
	}{
		{name: "defaults", wantPage: 1, wantSize: 20},
		{name: "snake case", raw: "page=3&page_size=40", wantPage: 3, wantSize: 40},
		{name: "legacy alias", raw: "page=2&pageSize=30", wantPage: 2, wantSize: 30},
		{name: "canonical wins", raw: "page_size=25&pageSize=30", wantPage: 1, wantSize: 25},
		{name: "bounds normalize", raw: "page=0&page_size=500", wantPage: 1, wantSize: 200},
		{name: "invalid integer", raw: "page=nope", wantPage: 1, wantSize: 20, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q := Query(queryReq(tt.raw))
			page, size := q.Page(20, 200)
			if page != tt.wantPage || size != tt.wantSize {
				t.Fatalf("page=%d size=%d, want page=%d size=%d", page, size, tt.wantPage, tt.wantSize)
			}
			if (q.Err() != nil) != tt.wantErr {
				t.Fatalf("err=%v, wantErr=%t", q.Err(), tt.wantErr)
			}
		})
	}
}

func TestQueryStrTrimAndRequired(t *testing.T) {
	q := Query(queryReq("name=%20%20foo%20%20"))
	if got := q.Str("name"); got != "foo" {
		t.Fatalf("want trimmed 'foo', got %q", got)
	}
	if got := q.StrDefault("missing", "def"); got != "def" {
		t.Fatalf("want default, got %q", got)
	}
	if q.Required("name") != "foo" || q.Err() != nil {
		t.Fatalf("present required must not error")
	}
	q2 := Query(queryReq("x=1"))
	_ = q2.Required("missing")
	if q2.Err() == nil {
		t.Fatal("missing required must record an error")
	}
}

func TestDecodeJSON(t *testing.T) {
	var dst struct {
		Name string `json:"name"`
	}
	ok := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(`{"name":"bar"}`))
	if err := DecodeJSON(ok, &dst); err != nil || dst.Name != "bar" {
		t.Fatalf("valid body: err=%v name=%q", err, dst.Name)
	}
	bad := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(`{bad`))
	if err := DecodeJSON(bad, &dst); err == nil {
		t.Fatal("invalid body must error")
	}
}

func TestDecodeStrictJSONRejectsUnknownFields(t *testing.T) {
	var dst struct {
		Name string `json:"name"`
	}
	request := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(`{"name":"bar","legacy":true}`))
	if err := DecodeStrictJSON(request, &dst); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown field error = %v", err)
	}
}
