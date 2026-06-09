package worker

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/timae/ses/internal/fleet"
)

// streamLine is the subset of Claude Code's --output-format stream-json we
// watch: the init event (session id), assistant messages (usage), and the
// final result event.
type streamLine struct {
	Type      string `json:"type"`
	Subtype   string `json:"subtype"`
	SessionID string `json:"session_id"`
	IsError   bool   `json:"is_error"`
	NumTurns  int    `json:"num_turns"`
	Result    string `json:"result"`
	Message   struct {
		ID    string `json:"id"`
		Usage *struct {
			InputTokens              int64 `json:"input_tokens"`
			CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
			CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
			OutputTokens             int64 `json:"output_tokens"`
		} `json:"usage"`
	} `json:"message"`
}

type runMonitor struct {
	mu             sync.Mutex
	sessionID      string
	tokens         int64 // weighted
	turns          int
	budgetExceeded bool
	turnsExceeded  bool
	canceled       bool
	resultErr      string
	tailLines      []string
}

func (m *runMonitor) logTail() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return strings.Join(m.tailLines, "\n")
}

// monitorRun consumes the stream-json output, enforcing the task's token and
// turn budgets, and renews the coordinator lease every minute (which is also
// how a remote cancel arrives). kill aborts the claude process.
func monitorRun(ctx context.Context, stdout io.Reader, task fleet.Task, p Policy, lease LeaseFunc, kill context.CancelFunc) *runMonitor {
	m := &runMonitor{}
	seen := make(map[string]bool)
	done := make(chan struct{})

	// Lease renewal loop.
	go func() {
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-done:
				return
			case <-t.C:
				m.mu.Lock()
				tokens := m.tokens
				m.mu.Unlock()
				if lease == nil {
					continue
				}
				cancel, err := lease(tokens)
				if err != nil {
					continue // transient coordinator errors must not kill the run
				}
				if cancel {
					m.mu.Lock()
					m.canceled = true
					m.mu.Unlock()
					kill()
					return
				}
			}
		}
	}()

	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 4*1024*1024), 4*1024*1024)
	for sc.Scan() {
		raw := sc.Text()
		var line streamLine
		if json.Unmarshal([]byte(raw), &line) != nil {
			continue
		}

		m.mu.Lock()
		if len(raw) > 400 {
			raw = raw[:400]
		}
		m.tailLines = append(m.tailLines, raw)
		if len(m.tailLines) > 20 {
			m.tailLines = m.tailLines[1:]
		}

		switch line.Type {
		case "system":
			if line.Subtype == "init" && line.SessionID != "" {
				m.sessionID = line.SessionID
			}
		case "assistant":
			// Same dedup rule as the transcript scanner: one usage object per
			// message id, multi-block turns repeat it.
			u := line.Message.Usage
			if u != nil && (line.Message.ID == "" || !seen[line.Message.ID]) {
				if line.Message.ID != "" {
					seen[line.Message.ID] = true
				}
				m.tokens += u.InputTokens + u.OutputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens/10
				m.turns++
			}
			if task.MaxTokens > 0 && m.tokens > task.MaxTokens && !m.budgetExceeded {
				m.budgetExceeded = true
				m.mu.Unlock()
				kill()
				continue
			}
			if task.MaxTurns > 0 && m.turns > task.MaxTurns && !m.turnsExceeded {
				m.turnsExceeded = true
				m.mu.Unlock()
				kill()
				continue
			}
		case "result":
			if line.NumTurns > 0 {
				m.turns = line.NumTurns
			}
			if line.IsError {
				m.resultErr = tailStr(line.Result, 300)
			}
		}
		m.mu.Unlock()
	}
	close(done)
	return m
}
