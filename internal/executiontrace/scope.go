package executiontrace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/dekwanlabs/nasuta/internal/domain"
	"github.com/dekwanlabs/nasuta/internal/llm"
	"github.com/dekwanlabs/nasuta/log"
	"github.com/dekwanlabs/nasuta/platform"
	"github.com/dekwanlabs/nasuta/tool"
)

// Mode selects whether a run produces evaluation trace events.
type Mode uint8

const (
	Disabled Mode = iota
	Evaluation
)

const (
	maxEvents                  = 512
	maxEventBytes              = 64 << 10
	maxTotalBytes              = 2 << 20
	truncationEventByteReserve = 512
	maxOmittedNodeBytes        = 128
)

// Scope owns trace ordering and elapsed timing for one execution run.
type Scope struct {
	mu         sync.Mutex
	exportMu   sync.Mutex
	started    time.Time
	sequence   int
	enabled    bool
	closed     bool
	truncated  bool
	totalBytes int
	events     []domain.EvaluationTrace
	emit       func(domain.EvaluationTrace)
	traceID    string
}

// Correlation identifies a run within one shared execution trace.
type Correlation struct {
	RunID          string
	ParentRunID    string
	WorkflowRunID  string
	AgentRunID     string
	WorkflowNodeID string
}

type correlatedRecorder struct {
	scope       *Scope
	correlation Correlation
}

var traceSequence atomic.Uint64

type requestConfig struct {
	mode Mode
	emit func(domain.EvaluationTrace)
}

// NewScope creates one request-local trace scope.
func NewScope(mode Mode, emit func(domain.EvaluationTrace)) *Scope {
	started := time.Now()
	scope := &Scope{started: started, enabled: mode == Evaluation, emit: emit}
	if mode != Evaluation {
		return scope
	}
	seed := fmt.Sprintf("%d:%d", started.UnixNano(), traceSequence.Add(1))
	scope.traceID = "trace_" + platform.UUIDFromString(seed)
	return scope
}

type requestConfigKey struct{}

// WithEvaluation requests evaluation tracing for the managed execution started from ctx.
func WithEvaluation(ctx context.Context, emit func(domain.EvaluationTrace)) context.Context {
	return context.WithValue(ctx, requestConfigKey{}, requestConfig{mode: Evaluation, emit: emit})
}

// Begin creates the scope requested by the transport at a managed run boundary.
func Begin(ctx context.Context) *Scope {
	if ctx == nil {
		return nil
	}
	if scope := FromContext(ctx); scope != nil {
		return scope
	}
	config, _ := ctx.Value(requestConfigKey{}).(requestConfig)
	if config.mode != Evaluation {
		return nil
	}
	return NewScope(config.mode, config.emit)
}

// Enabled reports whether the scope records evaluation events.
func (scope *Scope) Enabled() bool {
	if scope == nil {
		return false
	}
	scope.mu.Lock()
	defer scope.mu.Unlock()
	return scope.enabled && !scope.closed && !scope.truncated
}

// Record assigns stable sequence and elapsed values before exporting the event.
func (scope *Scope) Record(event domain.EvaluationTrace) {
	scope.record(Correlation{}, event)
}

func (scope *Scope) record(correlation Correlation, event domain.EvaluationTrace) {
	if !scope.Enabled() {
		return
	}
	scope.exportMu.Lock()
	defer scope.exportMu.Unlock()
	scope.mu.Lock()
	if scope.closed || scope.truncated {
		scope.mu.Unlock()
		return
	}
	scope.sequence++
	event.Sequence = scope.sequence
	event.TraceID = scope.traceID
	event.RunID = correlation.RunID
	event.ParentRunID = correlation.ParentRunID
	event.WorkflowRunID = correlation.WorkflowRunID
	event.AgentRunID = correlation.AgentRunID
	event.WorkflowNodeID = correlation.WorkflowNodeID
	if event.ElapsedMS == 0 {
		event.ElapsedMS = time.Since(scope.started).Milliseconds()
	}
	eventBytes, marshalErr := json.Marshal(event)
	reason := ""
	switch {
	case marshalErr != nil:
		reason = "encoding_failed"
	case len(eventBytes) > maxEventBytes:
		reason = "event_bytes"
	case len(scope.events) >= maxEvents-1:
		reason = "event_count"
	case scope.totalBytes+len(eventBytes) > maxTotalBytes-truncationEventByteReserve:
		reason = "total_bytes"
	}
	if reason != "" {
		event = truncatedEvent(event.Sequence, event.ElapsedMS, reason, event.Node)
		event.TraceID = scope.traceID
		event.RunID = correlation.RunID
		event.ParentRunID = correlation.ParentRunID
		event.WorkflowRunID = correlation.WorkflowRunID
		event.AgentRunID = correlation.AgentRunID
		event.WorkflowNodeID = correlation.WorkflowNodeID
		eventBytes, _ = json.Marshal(event)
		scope.truncated = true
	}
	event = cloneTraceEvent(event)
	scope.events = append(scope.events, event)
	scope.totalBytes += len(eventBytes)
	scope.mu.Unlock()
	if scope.emit != nil {
		exported := cloneTraceEvent(event)
		func() {
			defer func() {
				if recovered := recover(); recovered != nil {
					log.Errorf("[execution_trace] export node=%q sequence=%d: %v", event.Node, event.Sequence, recovered)
				}
			}()
			scope.emit(exported)
		}()
	}
}

// Close seals the scope after its owning run finishes.
func (scope *Scope) Close() {
	if scope == nil {
		return
	}
	scope.exportMu.Lock()
	defer scope.exportMu.Unlock()
	scope.mu.Lock()
	scope.closed = true
	scope.mu.Unlock()
}

func truncatedEvent(sequence int, elapsedMS int64, reason, omittedNode string) domain.EvaluationTrace {
	return domain.EvaluationTrace{
		Sequence: sequence, Node: "execution_trace", Status: "truncated", ElapsedMS: elapsedMS,
		Output: map[string]any{
			"reason": reason, "omitted_node": truncateBytes(omittedNode, maxOmittedNodeBytes),
			"max_events": maxEvents, "max_event_bytes": maxEventBytes, "max_total_bytes": maxTotalBytes,
		},
	}
}

func truncateBytes(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	value = value[:limit]
	for len(value) > 0 && !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

func cloneTraceEvent(event domain.EvaluationTrace) domain.EvaluationTrace {
	event.Input = cloneTraceMap(event.Input)
	event.Output = cloneTraceMap(event.Output)
	return event
}

func cloneTraceMap(fields map[string]any) map[string]any {
	if fields == nil {
		return nil
	}
	cloned := make(map[string]any, len(fields))
	for name, value := range fields {
		clonedValue := cloneTraceValue(reflect.ValueOf(value))
		if !clonedValue.IsValid() {
			cloned[name] = nil
			continue
		}
		cloned[name] = clonedValue.Interface()
	}
	return cloned
}

// Trace projections use JSON-compatible values but retain their Go types inside the run.
func cloneTraceValue(value reflect.Value) reflect.Value {
	if !value.IsValid() {
		return value
	}
	switch value.Kind() {
	case reflect.Interface:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		cloned := reflect.New(value.Type()).Elem()
		cloned.Set(cloneTraceValue(value.Elem()))
		return cloned
	case reflect.Pointer:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		cloned := reflect.New(value.Type().Elem())
		cloned.Elem().Set(cloneTraceValue(value.Elem()))
		return cloned
	case reflect.Map:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		cloned := reflect.MakeMapWithSize(value.Type(), value.Len())
		iterator := value.MapRange()
		for iterator.Next() {
			cloned.SetMapIndex(cloneTraceValue(iterator.Key()), cloneTraceValue(iterator.Value()))
		}
		return cloned
	case reflect.Slice:
		if value.IsNil() {
			return reflect.Zero(value.Type())
		}
		cloned := reflect.MakeSlice(value.Type(), value.Len(), value.Len())
		for index := 0; index < value.Len(); index++ {
			cloned.Index(index).Set(cloneTraceValue(value.Index(index)))
		}
		return cloned
	case reflect.Array:
		cloned := reflect.New(value.Type()).Elem()
		for index := 0; index < value.Len(); index++ {
			cloned.Index(index).Set(cloneTraceValue(value.Index(index)))
		}
		return cloned
	case reflect.Struct:
		cloned := reflect.New(value.Type()).Elem()
		cloned.Set(value)
		valueType := value.Type()
		for index := 0; index < value.NumField(); index++ {
			if valueType.Field(index).PkgPath == "" {
				cloned.Field(index).Set(cloneTraceValue(value.Field(index)))
			}
		}
		return cloned
	default:
		return value
	}
}

// RecordTrace keeps existing trace-producing business code on the v1 contract.
func (scope *Scope) RecordTrace(event domain.EvaluationTrace) {
	scope.Record(event)
}

// Snapshot returns a stable copy for a transport exporter.
func (scope *Scope) Snapshot() []domain.EvaluationTrace {
	if scope == nil {
		return nil
	}
	scope.mu.Lock()
	events := append([]domain.EvaluationTrace(nil), scope.events...)
	scope.mu.Unlock()
	for index := range events {
		events[index] = cloneTraceEvent(events[index])
	}
	return events
}

type scopeKey struct{}

// WithScope attaches the scope and legacy trace bridge to the execution context.
func WithScope(ctx context.Context, scope *Scope) context.Context {
	if scope == nil || !scope.Enabled() {
		return ctx
	}
	ctx = context.WithValue(ctx, scopeKey{}, scope)
	return WithCorrelation(ctx, Correlation{})
}

// FromContext returns the request-local execution trace scope.
func FromContext(ctx context.Context) *Scope {
	if ctx == nil {
		return nil
	}
	scope, _ := ctx.Value(scopeKey{}).(*Scope)
	return scope
}

type correlationKey struct{}

// WithCorrelation associates subsequent events with one run or workflow node.
func WithCorrelation(ctx context.Context, correlation Correlation) context.Context {
	scope := FromContext(ctx)
	if scope == nil || !scope.Enabled() {
		return ctx
	}
	current, _ := ctx.Value(correlationKey{}).(Correlation)
	merged := mergeCorrelation(current, correlation)
	recorder := &correlatedRecorder{scope: scope, correlation: merged}
	ctx = context.WithValue(ctx, correlationKey{}, merged)
	ctx = domain.WithTraceRecorder(ctx, recorder)
	ctx = tool.WithExecutionObserver(ctx, recorder)
	return llm.WithExecutionObserver(ctx, recorder)
}

func mergeCorrelation(current, next Correlation) Correlation {
	if next.RunID != "" {
		current.RunID = next.RunID
	}
	if next.ParentRunID != "" {
		current.ParentRunID = next.ParentRunID
	}
	if next.WorkflowRunID != "" {
		current.WorkflowRunID = next.WorkflowRunID
	}
	if next.AgentRunID != "" {
		current.AgentRunID = next.AgentRunID
	}
	if next.WorkflowNodeID != "" {
		current.WorkflowNodeID = next.WorkflowNodeID
	}
	return current
}

func (recorder *correlatedRecorder) Enabled() bool {
	return recorder != nil && recorder.scope.Enabled()
}

func (recorder *correlatedRecorder) RecordTrace(event domain.EvaluationTrace) {
	if recorder != nil {
		recorder.scope.record(recorder.correlation, event)
	}
}

// OnToolExecution projects the shared executor callback onto Trace v1.
func (scope *Scope) OnToolExecution(_ context.Context, execution tool.Execution) {
	scope.recordTool(Correlation{}, execution)
}

func (recorder *correlatedRecorder) OnToolExecution(_ context.Context, execution tool.Execution) {
	if recorder != nil {
		recorder.scope.recordTool(recorder.correlation, execution)
	}
}

func (scope *Scope) recordTool(correlation Correlation, execution tool.Execution) {
	argumentNames := make([]string, 0, len(execution.Arguments))
	for name := range execution.Arguments {
		argumentNames = append(argumentNames, name)
	}
	sort.Strings(argumentNames)
	output := map[string]any{
		"content_bytes": len(execution.Result.Content), "reference_count": len(execution.Result.References),
		"partial": execution.Result.Coverage.Partial, "omitted_items": execution.Result.Coverage.OmittedItems,
	}
	status := "completed"
	switch {
	case execution.Panic != nil:
		status = "failed"
		output["error"] = fmt.Sprint(execution.Panic)
	case errors.Is(execution.Err, context.Canceled), errors.Is(execution.Err, context.DeadlineExceeded):
		status = "cancelled"
		output["error"] = execution.Err.Error()
	case execution.Err != nil:
		status = "failed"
		output["error"] = execution.Err.Error()
	}
	scope.record(correlation, domain.EvaluationTrace{
		Node: "tool_execution", Status: status, DurationMS: execution.Duration.Milliseconds(),
		Input: map[string]any{
			"tool": execution.ID, "argument_count": len(execution.Arguments), "argument_names": argumentNames,
		},
		Output: output,
	})
}

// OnLLMExecution projects the shared model callback onto Trace v1.
func (scope *Scope) OnLLMExecution(_ context.Context, execution llm.Execution) {
	scope.recordLLM(Correlation{}, execution)
}

func (recorder *correlatedRecorder) OnLLMExecution(_ context.Context, execution llm.Execution) {
	if recorder != nil {
		recorder.scope.recordLLM(recorder.correlation, execution)
	}
}

// OnLLMAttempt records retry and repair decisions without prompt content.
func (scope *Scope) OnLLMAttempt(_ context.Context, attempt llm.Attempt) {
	scope.recordLLMAttempt(Correlation{}, attempt)
}

func (recorder *correlatedRecorder) OnLLMAttempt(_ context.Context, attempt llm.Attempt) {
	if recorder != nil {
		recorder.scope.recordLLMAttempt(recorder.correlation, attempt)
	}
}

func (scope *Scope) recordLLMAttempt(correlation Correlation, attempt llm.Attempt) {
	output := map[string]any{
		"logical_call_seq": attempt.LogicalCallSeq, "kind": attempt.Kind,
		"attempt": attempt.Attempt, "max_attempts": attempt.MaxAttempts,
		"outcome": attempt.Outcome, "retryable": attempt.Retryable,
		"retry_scheduled": attempt.RetryScheduled,
	}
	if attempt.RepairRound > 0 {
		output["repair_round"] = attempt.RepairRound
	}
	if attempt.ErrorKind != "" {
		output["error_kind"] = attempt.ErrorKind
	}
	if attempt.StatusCode > 0 {
		output["status_code"] = attempt.StatusCode
	}
	if attempt.ValidationErrorKind != "" {
		output["validation_error_kind"] = attempt.ValidationErrorKind
	}
	if attempt.Backoff > 0 {
		output["backoff_ms"] = attempt.Backoff.Milliseconds()
	}
	status := "completed"
	if attempt.Outcome == "failed" || attempt.Outcome == "exhausted" {
		status = "failed"
	}
	scope.record(correlation, domain.EvaluationTrace{
		Node: "llm_attempt", Status: status, DurationMS: attempt.Duration.Milliseconds(),
		Output: output,
	})
}

func (scope *Scope) recordLLM(correlation Correlation, execution llm.Execution) {
	output := map[string]any{
		"call_seq": execution.CallSeq, "provider": execution.Provider,
		"model": execution.Model, "max_output_tokens": execution.MaxOutputTokens,
	}
	status := "completed"
	switch {
	case execution.Panic != nil:
		status = "failed"
		output["error"] = fmt.Sprint(execution.Panic)
	case errors.Is(execution.Err, context.Canceled), errors.Is(execution.Err, context.DeadlineExceeded):
		status = "cancelled"
		output["error"] = execution.Err.Error()
	case execution.Err != nil:
		status = "failed"
		output["error"] = execution.Err.Error()
	}
	scope.record(correlation, domain.EvaluationTrace{
		Node: "llm_call", Status: status, DurationMS: execution.Duration.Milliseconds(),
		Input: map[string]any{"phase": execution.Phase}, Output: output,
	})
}
