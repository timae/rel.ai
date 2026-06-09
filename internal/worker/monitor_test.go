package worker

import (
	"context"
	"strings"
	"testing"

	"github.com/timae/ses/internal/fleet"
)

const fakeStream = `{"type":"system","subtype":"init","session_id":"sess-123"}
{"type":"assistant","message":{"id":"m1","usage":{"input_tokens":100,"output_tokens":50,"cache_creation_input_tokens":0,"cache_read_input_tokens":1000}}}
{"type":"assistant","message":{"id":"m1","usage":{"input_tokens":100,"output_tokens":50,"cache_creation_input_tokens":0,"cache_read_input_tokens":1000}}}
{"type":"assistant","message":{"id":"m2","usage":{"input_tokens":200,"output_tokens":100,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}
{"type":"result","subtype":"success","num_turns":7,"is_error":false}
`

func TestMonitorDedupAndSessionID(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := monitorRun(ctx, strings.NewReader(fakeStream), fleet.Task{MaxTokens: 1_000_000}, Policy{}, nil, cancel)

	if m.sessionID != "sess-123" {
		t.Errorf("sessionID = %q, want sess-123", m.sessionID)
	}
	// m1 counted once: 100+50+1000/10 = 250; m2: 300. Total 550.
	if m.tokens != 550 {
		t.Errorf("tokens = %d, want 550", m.tokens)
	}
	// result event overrides counted turns
	if m.turns != 7 {
		t.Errorf("turns = %d, want 7", m.turns)
	}
	if m.budgetExceeded || m.canceled || m.resultErr != "" {
		t.Errorf("unexpected flags: %+v", m)
	}
}

func TestMonitorBudgetKill(t *testing.T) {
	killed := false
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	kill := func() { killed = true; cancel() }

	m := monitorRun(ctx, strings.NewReader(fakeStream), fleet.Task{MaxTokens: 100}, Policy{}, nil, kill)
	if !m.budgetExceeded {
		t.Error("expected budgetExceeded")
	}
	if !killed {
		t.Error("expected kill to be called")
	}
}

func TestMonitorTurnKill(t *testing.T) {
	killed := false
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	kill := func() { killed = true; cancel() }

	m := monitorRun(ctx, strings.NewReader(fakeStream), fleet.Task{MaxTokens: 1_000_000, MaxTurns: 1}, Policy{}, nil, kill)
	if !m.turnsExceeded {
		t.Error("expected turnsExceeded")
	}
	if !killed {
		t.Error("expected kill to be called")
	}
}
