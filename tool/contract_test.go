package tool

import (
	"context"
	"testing"
	"time"
)

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
