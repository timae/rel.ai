package worker

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// AuditEntry is one line in the local append-only audit log. It mirrors the
// coordinator's task_events but lives on the seat-holder's machine, so they
// can always see what ran under their account even without server access.
type AuditEntry struct {
	At         time.Time `json:"at"`
	TaskID     string    `json:"task_id"`
	Title      string    `json:"title,omitempty"`
	RepoURL    string    `json:"repo_url,omitempty"`
	Branch     string    `json:"branch,omitempty"`
	Event      string    `json:"event"` // claimed|started|done|failed|canceled|budget_exceeded|timeout
	TokensUsed int64     `json:"tokens_used,omitempty"`
	NumTurns   int       `json:"num_turns,omitempty"`
	Error      string    `json:"error,omitempty"`
}

func auditPath(workdir string) string {
	return filepath.Join(workdir, "audit.jsonl")
}

func appendAudit(workdir string, e AuditEntry) {
	if e.At.IsZero() {
		e.At = time.Now()
	}
	if err := os.MkdirAll(workdir, 0o700); err != nil {
		return
	}
	f, err := os.OpenFile(auditPath(workdir), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	data, _ := json.Marshal(e)
	f.Write(append(data, '\n'))
}

// RecentAudit returns the last n audit entries, newest last.
func RecentAudit(workdir string, n int) []AuditEntry {
	data, err := os.ReadFile(auditPath(workdir))
	if err != nil {
		return nil
	}
	var out []AuditEntry
	for _, line := range splitLines(data) {
		var e AuditEntry
		if json.Unmarshal(line, &e) == nil {
			out = append(out, e)
		}
	}
	if len(out) > n {
		out = out[len(out)-n:]
	}
	return out
}

func splitLines(data []byte) [][]byte {
	var out [][]byte
	start := 0
	for i, b := range data {
		if b == '\n' {
			if i > start {
				out = append(out, data[start:i])
			}
			start = i + 1
		}
	}
	if start < len(data) {
		out = append(out, data[start:])
	}
	return out
}
