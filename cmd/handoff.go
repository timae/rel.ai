package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/timae/ses/internal/model"
	"github.com/timae/ses/internal/redact"
	"github.com/timae/ses/internal/resume"
	"github.com/timae/ses/internal/shareserver"
)

var (
	handoffNote    string
	handoffExpires string
	handoffChain   bool
	handoffRedact  string
)

var handoffCmd = &cobra.Command{
	Use:   "handoff <id>",
	Short: "Hand off a session to a teammate via a single-use link",
	Long: `Package a session (redacted transcript + linked-session chain +
files touched + a note) and upload it as a single-use handoff.

The recipient runs 'ses resume --from <url>' on their machine to claim the
handoff and launch Claude Code with the full context pre-loaded. The link
is deleted after the first successful claim.

Run 'ses share login' once to configure the share server.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		url, err := createHandoff(args[0], handoffNote, handoffExpires, handoffRedact, handoffChain)
		if err != nil {
			return err
		}
		fmt.Println(url)
		return nil
	},
}

// createHandoff packages a session as a single-use handoff share and returns
// its URL. Used by `ses handoff` and by `ses fleet enqueue --handoff`.
func createHandoff(shortID, note, expires, redactMode string, chain bool) (string, error) {
	session, err := store.GetSession(shortID)
	if err != nil {
		return "", err
	}
	messages, err := loadTranscript(session)
	if err != nil {
		return "", fmt.Errorf("loading transcript: %w", err)
	}
	mode, err := parseRedactMode(redactMode)
	if err != nil {
		return "", err
	}
	scrubbed, report := redact.Redact(messages, redact.Options{Mode: mode})

	var linkedBundles []shareserver.LinkedSession
	if chain {
		linked, err := store.GetLinkedSessions(session.ShortID)
		if err != nil {
			return "", fmt.Errorf("loading linked sessions: %w", err)
		}
		for _, ls := range linked {
			lsMessages, err := loadTranscript(&ls.Session)
			if err != nil {
				fmt.Fprintf(os.Stderr, "skipping linked session %s: %v\n", ls.Session.ShortID, err)
				continue
			}
			scrubbedLinked, _ := redact.Redact(lsMessages, redact.Options{Mode: mode})
			linkedBundles = append(linkedBundles, shareserver.LinkedSession{
				ShortID:     ls.Session.ShortID,
				FirstPrompt: truncate(ls.Session.FirstPrompt, 200),
				StartedAt:   ls.Session.StartedAt,
				Reason:      ls.Reason,
				Brief:       resume.GenerateBrief(&ls.Session, scrubbedLinked),
			})
		}
	}

	cfg, err := loadShareConfig()
	if err != nil {
		return "", err
	}
	lifetime, err := parseLifetime(expires)
	if err != nil {
		return "", err
	}

	files := make([]shareserver.ShareFile, 0, len(session.Files))
	for _, f := range session.Files {
		scrubbedPath, _ := redact.Redact([]model.Message{{Content: f.FilePath}}, redact.Options{Mode: mode})
		files = append(files, shareserver.ShareFile{
			Path:   scrubbedPath[0].Content,
			Action: f.Action,
		})
	}

	fmt.Fprintf(os.Stderr, "handoff for %s (%d messages, %d linked, redaction=%s, %d chars scrubbed, expires in %s)\n",
		session.ShortID, len(scrubbed), len(linkedBundles), redactModeLabel(mode), report.Bytes, lifetime)

	body := map[string]any{
		"name":               truncate(session.FirstPrompt, 80),
		"kind":               shareserver.KindHandoff,
		"expires_in_seconds": int64(lifetime.Seconds()),
		"single_use":         true,
		"handoff_note":       note,
		"linked_sessions":    linkedBundles,
		"files":              files,
		"session": shareserver.ShareSession{
			ShortID:      session.ShortID,
			Project:      session.Project,
			Source:       string(session.SourceType),
			Model:        session.Model,
			GitBranch:    session.GitBranch,
			StartedAt:    session.StartedAt,
			EndedAt:      session.EndedAt,
			MessageCount: session.MessageCount,
			ToolCalls:    session.ToolCallCount,
			FirstPrompt:  session.FirstPrompt,
		},
		"messages": toShareMessages(scrubbed),
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	const maxBody = 10 << 20
	if len(buf) > maxBody {
		return "", fmt.Errorf("payload is %d bytes, exceeds 10 MB limit — try --redact=strict or drop --chain", len(buf))
	}
	req, _ := http.NewRequest(http.MethodPost, cfg.URL+"/v1/shares", bytes.NewReader(buf))
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("upload request: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("upload failed: %s: %s", resp.Status, strings.TrimSpace(string(respBody)))
	}
	var parsed struct {
		ID        string    `json:"id"`
		URL       string    `json:"url"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return "", fmt.Errorf("decoding response: %w", err)
	}
	appendShareLog(shareLogEntry{
		ID:           parsed.ID,
		URL:          parsed.URL,
		Name:         "handoff: " + truncate(session.FirstPrompt, 40),
		SessionShort: session.ShortID,
		CreatedAt:    time.Now(),
		ExpiresAt:    parsed.ExpiresAt,
	})
	return parsed.URL, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-3] + "..."
}

func init() {
	handoffCmd.Flags().StringVar(&handoffNote, "note", "", "free-text note to the recipient (what's the state, what's next)")
	handoffCmd.Flags().StringVar(&handoffExpires, "expires", "24h", "link lifetime (handoffs default to 24h, not 7d)")
	handoffCmd.Flags().BoolVar(&handoffChain, "chain", false, "include briefs for linked prior sessions in the bundle")
	handoffCmd.Flags().StringVar(&handoffRedact, "redact", "default", "redaction mode: off|default|strict")
	rootCmd.AddCommand(handoffCmd)
}
