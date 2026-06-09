package scanner

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/timae/ses/internal/model"
)

// A transcript where one assistant turn spans three lines (text, tool_use,
// text) sharing message id msg_1 with identical usage, followed by a second
// turn (msg_2) on a later day, and one line with no message id but usage.
// Correct totals must count msg_1's usage exactly once.
const usageFixture = `{"type":"user","uuid":"u1","sessionId":"s1","timestamp":"2026-06-01T10:00:00Z","cwd":"/tmp/proj","message":{"role":"user","content":"do the thing"}}
{"type":"assistant","uuid":"a1","sessionId":"s1","timestamp":"2026-06-01T10:00:05Z","message":{"id":"msg_1","role":"assistant","model":"claude-opus-4-6","usage":{"input_tokens":100,"cache_creation_input_tokens":50,"cache_read_input_tokens":1000,"output_tokens":20},"content":[{"type":"text","text":"working on it"}]}}
{"type":"assistant","uuid":"a2","sessionId":"s1","timestamp":"2026-06-01T10:00:06Z","message":{"id":"msg_1","role":"assistant","model":"claude-opus-4-6","usage":{"input_tokens":100,"cache_creation_input_tokens":50,"cache_read_input_tokens":1000,"output_tokens":20},"content":[{"type":"tool_use","id":"tu_1","name":"Read","input":{"file_path":"/tmp/proj/main.go"}}]}}
{"type":"assistant","uuid":"a3","sessionId":"s1","timestamp":"2026-06-01T10:00:07Z","message":{"id":"msg_1","role":"assistant","model":"claude-opus-4-6","usage":{"input_tokens":100,"cache_creation_input_tokens":50,"cache_read_input_tokens":1000,"output_tokens":20},"content":[{"type":"text","text":"done"}]}}
{"type":"assistant","uuid":"a4","sessionId":"s1","timestamp":"2026-06-02T09:00:00Z","message":{"id":"msg_2","role":"assistant","model":"claude-opus-4-6","usage":{"input_tokens":40,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":10},"content":[{"type":"text","text":"next day"}]}}
{"type":"assistant","uuid":"a5","sessionId":"s1","timestamp":"2026-06-02T09:01:00Z","message":{"role":"assistant","model":"claude-opus-4-6","usage":{"input_tokens":5,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":5},"content":[{"type":"text","text":"no id line"}]}}
`

func parseFixture(t *testing.T) *model.Session {
	t.Helper()
	dir := t.TempDir()
	projDir := filepath.Join(dir, "projects", "-tmp-proj")
	if err := os.MkdirAll(projDir, 0755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(projDir, "s1.jsonl")
	if err := os.WriteFile(path, []byte(usageFixture), 0644); err != nil {
		t.Fatal(err)
	}

	s := NewClaudeScanner(dir)
	files, err := s.Discover()
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("expected 1 session file, got %d", len(files))
	}
	session, _, err := s.Parse(files[0])
	if err != nil {
		t.Fatal(err)
	}
	return session
}

func TestClaudeUsageDedupByMessageID(t *testing.T) {
	session := parseFixture(t)

	// msg_1 counted once (100/20/50/1000), msg_2 once (40/10), id-less line once (5/5)
	if got, want := session.InputTokens, int64(100+40+5); got != want {
		t.Errorf("InputTokens = %d, want %d", got, want)
	}
	if got, want := session.OutputTokens, int64(20+10+5); got != want {
		t.Errorf("OutputTokens = %d, want %d", got, want)
	}
	if got, want := session.CacheCreationTokens, int64(50); got != want {
		t.Errorf("CacheCreationTokens = %d, want %d", got, want)
	}
	if got, want := session.CacheReadTokens, int64(1000); got != want {
		t.Errorf("CacheReadTokens = %d, want %d", got, want)
	}
	if got, want := session.APICallCount, 3; got != want {
		t.Errorf("APICallCount = %d, want %d", got, want)
	}
}

func TestClaudeUsageDayBuckets(t *testing.T) {
	session := parseFixture(t)

	if len(session.UsageDays) != 2 {
		t.Fatalf("expected 2 usage days, got %d: %+v", len(session.UsageDays), session.UsageDays)
	}
	// Days are bucketed in local time, so just verify the split of input tokens
	// across the two buckets and that both carry the model.
	var inputs []int64
	for _, d := range session.UsageDays {
		if d.Model != "claude-opus-4-6" {
			t.Errorf("UsageDay model = %q, want claude-opus-4-6", d.Model)
		}
		inputs = append(inputs, d.InputTokens)
	}
	if inputs[0] != 100 || inputs[1] != 45 {
		t.Errorf("per-day input tokens = %v, want [100 45]", inputs)
	}
}
