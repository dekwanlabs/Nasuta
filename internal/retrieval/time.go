package retrieval

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/dekwanlabs/nasuta/tool"
)

const (
	defaultRecent     = 24 * time.Hour
	defaultRecentDays = 5
)

// TimeExpr is a language-neutral relative-time value extracted by the router.
type TimeExpr struct {
	Kind string
	N    int
	Unit string
	Raw  string
}

// ResolveTime calculates an interval exclusively from the server anchor.
func ResolveTime(expr TimeExpr, anchor time.Time) (tool.TimeRange, bool, error) {
	if expr.Kind == "" || expr.Kind == "none" {
		return tool.TimeRange{}, false, nil
	}
	if anchor.IsZero() {
		return tool.TimeRange{}, false, fmt.Errorf("time resolver requires an anchor")
	}
	location := anchor.Location()
	local := anchor
	switch expr.Kind {
	case "recent":
		return tool.TimeRange{From: local.Add(-defaultRecent), To: local, ToExclusive: true, Raw: expr.Raw}, true, nil
	case "day":
		start := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, location).AddDate(0, 0, expr.N)
		return tool.TimeRange{From: start, To: start.AddDate(0, 0, 1), ToExclusive: true, Raw: expr.Raw}, true, nil
	case "last":
		n := expr.N
		if n == 0 && expr.Unit == "day" {
			n = defaultRecentDays
		}
		duration, err := relativeDuration(n, expr.Unit)
		if err != nil {
			return tool.TimeRange{}, false, err
		}
		return tool.TimeRange{From: local.Add(-duration), To: local, ToExclusive: true, Raw: expr.Raw}, true, nil
	default:
		return tool.TimeRange{}, false, fmt.Errorf("unsupported time kind %q", expr.Kind)
	}
}

func bindTimeExpr(raw map[string]any, question string) (TimeExpr, error) {
	expr := TimeExpr{
		Kind: strings.TrimSpace(stringValue(raw["kind"])),
		Unit: strings.TrimSpace(stringValue(raw["unit"])),
		Raw:  strings.TrimSpace(stringValue(raw["raw"])),
	}
	n, err := integerValue(raw["n"])
	if err != nil {
		return TimeExpr{}, fmt.Errorf("time.n: %w", err)
	}
	expr.N = n
	if expr.Kind == "none" {
		if expr.Raw != "" || expr.N != 0 || expr.Unit != "" {
			return TimeExpr{}, fmt.Errorf("time.none must not contain a value")
		}
		return expr, nil
	}
	if expr.Raw == "" || !strings.Contains(question, expr.Raw) {
		return TimeExpr{}, fmt.Errorf("time.raw must be copied exactly from the current question")
	}
	switch expr.Kind {
	case "recent":
		if expr.N != 0 || expr.Unit != "" {
			return TimeExpr{}, fmt.Errorf("time.recent must not contain n or unit")
		}
	case "day":
		if expr.N < -30 || expr.N > 0 || expr.Unit != "" {
			return TimeExpr{}, fmt.Errorf("time.day n must be between -30 and 0 without a unit")
		}
	case "last":
		if expr.N < 0 || expr.N > 365 {
			return TimeExpr{}, fmt.Errorf("time.last n must be between 0 and 365")
		}
		if expr.N == 0 && expr.Unit != "day" {
			return TimeExpr{}, fmt.Errorf("only a vague day expression may omit n")
		}
		if _, err := relativeDuration(max(expr.N, 1), expr.Unit); err != nil {
			return TimeExpr{}, err
		}
	default:
		return TimeExpr{}, fmt.Errorf("unsupported time kind %q", expr.Kind)
	}
	return expr, nil
}

func relativeDuration(n int, unit string) (time.Duration, error) {
	if n <= 0 {
		return 0, fmt.Errorf("relative duration must be positive")
	}
	var base time.Duration
	switch unit {
	case "minute":
		base = time.Minute
	case "hour":
		base = time.Hour
	case "day":
		base = 24 * time.Hour
	case "week":
		base = 7 * 24 * time.Hour
	default:
		return 0, fmt.Errorf("unsupported time unit %q", unit)
	}
	if float64(n) > float64(math.MaxInt64)/float64(base) {
		return 0, fmt.Errorf("relative duration is too large")
	}
	return time.Duration(n) * base, nil
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}

func integerValue(value any) (int, error) {
	switch number := value.(type) {
	case int:
		return number, nil
	case float64:
		if math.Trunc(number) != number {
			return 0, fmt.Errorf("must be an integer")
		}
		return int(number), nil
	default:
		return 0, fmt.Errorf("must be an integer")
	}
}
