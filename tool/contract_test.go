package tool

import (
	"context"
	"reflect"
	"testing"
	"time"
)

func TestArguments(t *testing.T) {
	args := Arguments{
		"string":       " value ",
		"empty":        "  ",
		"int":          3,
		"int64":        int64(4),
		"float64":      float64(5),
		"bool":         true,
		"strings":      []any{" first ", "", 2, "second"},
		"typedStrings": []string{" third ", "  ", "fourth"},
	}
	if got := args.String("string"); got != "value" {
		t.Fatalf("String = %q", got)
	}
	if got := args.StringDefault("empty", "fallback"); got != "fallback" {
		t.Fatalf("StringDefault = %q", got)
	}
	for key, want := range map[string]int{"int": 3, "int64": 4, "float64": 5, "missing": 6} {
		if got := args.Int(key, 6); got != want {
			t.Fatalf("Int(%q) = %d, want %d", key, got, want)
		}
	}
	if got := args.BoundedInt("int", 2, 4, 8); got != 4 {
		t.Fatalf("BoundedInt = %d", got)
	}
	if !args.Bool("bool") || args.Bool("missing") {
		t.Fatalf("Bool values are invalid")
	}
	if got, want := args.Strings("strings"), []string{"first", "second"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Strings = %#v, want %#v", got, want)
	}
	if got, want := args.Strings("typedStrings"), []string{"third", "fourth"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("typed Strings = %#v, want %#v", got, want)
	}
}

func TestTimeRangeContextRoundTrip(t *testing.T) {
	want := TimeRange{
		From:        time.Date(2026, 7, 17, 10, 30, 0, 0, time.UTC),
		To:          time.Date(2026, 7, 22, 10, 30, 0, 0, time.UTC),
		ToExclusive: true,
		Raw:         "最近几天",
	}
	got, ok := TimeRangeFromContext(WithTimeRange(context.Background(), want))
	if !ok || !got.From.Equal(want.From) || !got.To.Equal(want.To) || !got.ToExclusive || got.Raw != want.Raw {
		t.Fatalf("time range = %#v, ok=%v", got, ok)
	}
}
