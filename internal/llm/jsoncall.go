package llm

import (
	"context"
	"fmt"
	"reflect"
	"unicode/utf8"

	"github.com/dekwanlabs/nasuta/internal/prompts"
)

const defaultRepairAttempts = 1

func (o CallOptions) repairAttempts() int {
	if o.RepairAttempts <= 0 {
		return defaultRepairAttempts
	}
	return o.RepairAttempts
}

// ChatJSON calls the model and decodes its answer into out (a pointer), retrying
// transport failures with exponential backoff. A malformed answer is first run
// through RepairJSON; only if that also fails (or Validate rejects it) does it
// re-prompt the model with the failure reason. Transport retries and reprompts
// share one budget (MaxAttempts).
func (lc *LLMClient) ChatJSON(ctx context.Context, system, user string, out any, opts CallOptions) error {
	return chatJSONWith(ctx, lc.chatMessages, system, user, out, opts)
}

func chatJSONWith(ctx context.Context, call chatCaller, system, user string, out any, opts CallOptions) error {
	maxAttempts := opts.maxAttempts()
	backoff := opts.backoff()
	maxRepair := opts.repairAttempts()
	msgs := []Message{{Role: "system", Content: system}, {Role: "user", Content: user}}
	repairs := 0
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		raw, err := call(ctx, msgs, opts.MaxTokens)
		if err != nil {
			if ce, retryable := retryableCallError(err); retryable {
				if attempt == maxAttempts {
					return fmt.Errorf("%w: %w", ErrMaxAttempts, err)
				}
				if !sleepFor(ctx, ce, backoff) {
					return ctx.Err()
				}
				backoff = doubleBackoff(backoff)
				continue
			}
			return err
		}
		ok, problem := parseRepairValidate(raw, out, opts.Validate, opts.DisallowUnknownFields)
		if ok {
			return nil
		}
		if repairs < maxRepair && attempt < maxAttempts {
			msgs = append(msgs,
				Message{Role: "assistant", Content: raw},
				Message{Role: "user", Content: repairPrompt(problem)},
			)
			repairs++
			continue
		}
		return fmt.Errorf("%w: %s; response=%s", ErrInvalidJSON, problem, truncateForErr(raw))
	}
	return ErrMaxAttempts
}

// parseRepairValidate decodes raw into out (a pointer), running RepairJSON first
// if the raw text is malformed, then the optional schema check. Parsing always
// targets a fresh zero value so a failed first parse cannot leave stale fields
// (maps keep existing entries across re-unmarshal). Returns (ok, problem) where
// problem is a human-readable reason fed back to the model on reprompt.
func parseRepairValidate(raw string, out any, validate func(any) error, disallowUnknownFields bool) (bool, string) {
	rv := reflect.ValueOf(out)
	if rv.Kind() != reflect.Ptr || rv.IsNil() {
		return false, "out must be a non-nil pointer"
	}
	fresh := reflect.New(rv.Elem().Type())
	if err := parseJSONLoose(raw, fresh.Interface(), disallowUnknownFields); err != nil {
		fresh = reflect.New(rv.Elem().Type())
		if err := parseJSONLoose(RepairJSON(raw), fresh.Interface(), disallowUnknownFields); err != nil {
			return false, "malformed JSON (" + err.Error() + ")"
		}
	}
	if validate != nil {
		if err := validate(fresh.Interface()); err != nil {
			return false, "schema validation (" + err.Error() + ")"
		}
	}
	rv.Elem().Set(fresh.Elem())
	return true, ""
}

func repairPrompt(problem string) string {
	return prompts.MustRender(prompts.LLMJSONRepair, struct {
		Problem string
	}{Problem: problem})
}

func truncateForErr(s string) string {
	const max = 400
	if len(s) <= max {
		return s
	}
	s = s[:max]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s + "…"
}
