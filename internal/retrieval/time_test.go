package retrieval

import (
	"testing"
	"time"
)

func TestResolveTimeUsesServerAnchor(t *testing.T) {
	location := mustShanghai(t)
	anchor := time.Date(2026, 7, 22, 10, 30, 0, 0, location)

	tests := []struct {
		name string
		expr TimeExpr
		from string
		to   string
	}{
		{name: "recent", expr: TimeExpr{Kind: "recent", Raw: "recently"}, from: "2026-07-21T10:30:00+08:00", to: "2026-07-22T10:30:00+08:00"},
		{name: "yesterday multilingual", expr: TimeExpr{Kind: "day", N: -1, Raw: "ayer"}, from: "2026-07-21T00:00:00+08:00", to: "2026-07-22T00:00:00+08:00"},
		{name: "recent days defaults to five", expr: TimeExpr{Kind: "last", Unit: "day", Raw: "最近几天"}, from: "2026-07-17T10:30:00+08:00", to: "2026-07-22T10:30:00+08:00"},
		{name: "last two hours", expr: TimeExpr{Kind: "last", N: 2, Unit: "hour", Raw: "last two hours"}, from: "2026-07-22T08:30:00+08:00", to: "2026-07-22T10:30:00+08:00"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok, err := ResolveTime(test.expr, anchor)
			if err != nil {
				t.Fatal(err)
			}
			if !ok || got.From.Format(time.RFC3339) != test.from || got.To.Format(time.RFC3339) != test.to {
				t.Fatalf("range = %s to %s, ok=%v", got.From.Format(time.RFC3339), got.To.Format(time.RFC3339), ok)
			}
		})
	}
}

func TestResolveTimeUsesAnchorLocation(t *testing.T) {
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	anchor := time.Date(2026, 3, 9, 10, 30, 0, 0, location)

	got, ok, err := ResolveTime(TimeExpr{Kind: "day", N: -1, Raw: "yesterday"}, anchor)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || got.From.Format(time.RFC3339) != "2026-03-08T00:00:00-05:00" || got.To.Format(time.RFC3339) != "2026-03-09T00:00:00-04:00" {
		t.Fatalf("range = %s to %s, ok=%v", got.From.Format(time.RFC3339), got.To.Format(time.RFC3339), ok)
	}
}

func TestBindTimeExprRequiresGroundedRawText(t *testing.T) {
	got, err := bindTimeExpr(map[string]any{
		"kind": "last", "n": float64(0), "unit": "day", "raw": "últimos días",
	}, "muestra los últimos días")
	if err != nil || got.Kind != "last" || got.N != 0 || got.Unit != "day" {
		t.Fatalf("expression=%#v error=%v", got, err)
	}
	_, err = bindTimeExpr(map[string]any{
		"kind": "day", "n": float64(-1), "unit": "", "raw": "invented",
	}, "查看昨天日志")
	if err == nil {
		t.Fatal("invented raw time was accepted")
	}
}

func mustShanghai(t *testing.T) *time.Location {
	t.Helper()
	location, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	return location
}
