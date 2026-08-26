package execution

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	agentapi "github.com/dekwanlabs/nasuta/agent"
	"github.com/dekwanlabs/nasuta/internal/llm"
)

type recordingCallGate struct {
	checkCalls int
	reserved   []agentapi.Usage
	released   int
	settled    int
}

func (gate *recordingCallGate) Check() error {
	gate.checkCalls++
	return nil
}

func (gate *recordingCallGate) ReserveCall(usage agentapi.Usage) (agentapi.RunBudgetCallReservation, error) {
	gate.reserved = append(gate.reserved, usage)
	return recordingCallReservation{gate: gate}, nil
}

type recordingCallReservation struct {
	gate *recordingCallGate
}

func (reservation recordingCallReservation) Settle(agentapi.Usage) error {
	reservation.gate.settled++
	return nil
}

func (reservation recordingCallReservation) Release() error {
	reservation.gate.released++
	return nil
}

type availableRecordingCallGate struct {
	recordingCallGate
	available agentapi.Usage
}

func (gate *availableRecordingCallGate) Available() agentapi.Usage {
	return gate.available
}

func TestCallModelShrinksOutputToSharedAvailability(t *testing.T) {
	server := fakeStreamServer(t, []streamEvent{{content: "答案", finish: "stop"}})
	defer server.Close()

	agent := newTestAgent(t, server.URL)
	gate := &availableRecordingCallGate{available: agentapi.Usage{
		InputTokens: 10_000, OutputTokens: 25,
	}}
	ctx := agentapi.WithRunBudgetGate(context.Background(), gate)
	result, err := agent.callModel(
		ctx,
		[]llm.Message{{Role: "user", Content: "请说明服务职责"}},
		nil,
		nil,
		100,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || result.Content != "答案" {
		t.Fatalf("result = %#v", result)
	}
	if len(gate.reserved) != 1 || gate.reserved[0].OutputTokens != 25 {
		t.Fatalf("call reservations = %#v, want output 25", gate.reserved)
	}
}

func TestForceConclusionUsesRemainingSharedOutputBudget(t *testing.T) {
	server := fakeStreamServer(t, []streamEvent{{content: "最终答案", finish: "stop"}})
	defer server.Close()

	agent := newTestAgent(t, server.URL)
	gate := &availableRecordingCallGate{available: agentapi.Usage{
		InputTokens: 10_000, OutputTokens: 25,
	}}
	ctx := agentapi.WithRunBudgetGate(context.Background(), gate)
	step := 0
	result, err := agent.forceConclusion(ctx, "run_budgeted_conclusion", nil, nil, &step, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || result.Content != "最终答案" {
		t.Fatalf("result = %#v", result)
	}
	if len(gate.reserved) != 1 || gate.reserved[0].OutputTokens != 25 {
		t.Fatalf("conclusion reservations = %#v, want output 25", gate.reserved)
	}
}

func TestCallModelRejectsWhenSharedInputBudgetIsExhausted(t *testing.T) {
	server := fakeStreamServer(t, []streamEvent{{content: "不应调用", finish: "stop"}})
	defer server.Close()

	agent := newTestAgent(t, server.URL)
	gate := &availableRecordingCallGate{available: agentapi.Usage{
		InputTokens: 1, OutputTokens: 100,
	}}
	ctx := agentapi.WithRunBudgetGate(context.Background(), gate)
	_, err := agent.callModel(
		ctx,
		[]llm.Message{{Role: "user", Content: "请说明服务职责"}},
		nil,
		nil,
		100,
	)
	if err == nil || !strings.Contains(err.Error(), "input_tokens") {
		t.Fatalf("error = %v, want input budget error", err)
	}
	if len(gate.reserved) != 0 {
		t.Fatalf("call reservations = %#v, want none", gate.reserved)
	}
}

func TestCallModelReservesInputBeforeProviderAndReleasesUnknownUsage(t *testing.T) {
	server := fakeStreamServer(t, []streamEvent{{content: "答案", finish: "stop"}})
	defer server.Close()

	agent := newTestAgent(t, server.URL)
	gate := &recordingCallGate{}
	ctx := agentapi.WithRunBudgetGate(context.Background(), gate)
	result, err := agent.callModel(
		ctx,
		[]llm.Message{{Role: "user", Content: "请说明服务职责"}},
		nil,
		nil,
		100,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || result.Content != "答案" {
		t.Fatalf("result = %#v", result)
	}
	if gate.checkCalls != 1 {
		t.Fatalf("budget checks = %d, want 1", gate.checkCalls)
	}
	if len(gate.reserved) != 1 || gate.reserved[0].InputTokens <= 0 || gate.reserved[0].OutputTokens != 100 {
		t.Fatalf("call reservations = %#v", gate.reserved)
	}
	if gate.released != 1 || gate.settled != 0 {
		t.Fatalf("call reservation lifecycle released=%d settled=%d", gate.released, gate.settled)
	}
}

func TestModelCallEstimateIncludesRequestedOutputAndCost(t *testing.T) {
	inputPrice := int64(2_000_000)
	outputPrice := int64(4_000_000)
	agent := &Agent{cfg: Config{
		InputPriceMicrosPerMillionTokens:  inputPrice,
		OutputPriceMicrosPerMillionTokens: outputPrice,
	}}
	got, err := agent.modelCallEstimate(1000, 500)
	if err != nil {
		t.Fatal(err)
	}
	if got.InputTokens != 1000 || got.OutputTokens != 500 || got.TotalTokens != 1500 {
		t.Fatalf("estimated usage = %+v", got)
	}
	// 1000 * 2 + 500 * 4 micros at the configured per-million rates.
	if got.CostMicros != 4000 {
		t.Fatalf("estimated cost = %d, want 4000", got.CostMicros)
	}
}

func TestCallModelRejectsWhenSharedOutputBudgetIsExhausted(t *testing.T) {
	server := fakeStreamServer(t, []streamEvent{{content: "不应调用", finish: "stop"}})
	defer server.Close()

	agent := newTestAgent(t, server.URL)
	gate := &availableRecordingCallGate{available: agentapi.Usage{
		InputTokens:  10_000,
		OutputTokens: 0,
	}}
	ctx := agentapi.WithRunBudgetGate(context.Background(), gate)
	_, err := agent.callModel(
		ctx,
		[]llm.Message{{Role: "user", Content: "请说明服务职责"}},
		nil,
		nil,
		100,
	)
	if !errors.Is(err, ErrModelCallBudgetExhausted) {
		t.Fatalf("error = %v, want ErrModelCallBudgetExhausted", err)
	}
	if len(gate.reserved) != 0 {
		t.Fatalf("call reservations = %#v, want none", gate.reserved)
	}
}

type exhaustAfterFirstCallGate struct {
	availableRecordingCallGate
}

func (gate *exhaustAfterFirstCallGate) ReserveCall(usage agentapi.Usage) (agentapi.RunBudgetCallReservation, error) {
	reservation, err := gate.availableRecordingCallGate.ReserveCall(usage)
	gate.available.OutputTokens = 0
	return reservation, err
}

func TestForceConclusionSkipsRetryWhenSharedOutputBudgetIsExhausted(t *testing.T) {
	server := fakeStreamServer(t, []streamEvent{{reasoning: "继续分析", finish: "length"}})
	defer server.Close()

	agent := newTestAgent(t, server.URL)
	gate := &exhaustAfterFirstCallGate{availableRecordingCallGate: availableRecordingCallGate{
		available: agentapi.Usage{InputTokens: 10_000, OutputTokens: 100},
	}}
	ctx := agentapi.WithRunBudgetGate(context.Background(), gate)
	step := 0
	_, err := agent.forceConclusion(ctx, "run_no_retry_budget", nil, nil, &step, time.Now())
	if !errors.Is(err, ErrReasoningTruncated) {
		t.Fatalf("forceConclusion() error = %v, want ErrReasoningTruncated", err)
	}
	if len(gate.reserved) != 1 {
		t.Fatalf("call reservations = %#v, want one call", gate.reserved)
	}
}
