package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/timae/ses/internal/model"
	"github.com/timae/ses/internal/redact"
	"github.com/timae/ses/internal/shareserver"
)

var (
	shareDryRun  bool
	shareRedact  string
	shareExpires string
	shareName    string

	shareLoginURL   string
	shareLoginToken string
)

var shareCmd = &cobra.Command{
	Use:   "share <id>",
	Short: "Share a session via an expiring link",
	Long: `Upload a redacted transcript to your configured share server and
return a time-limited URL.

Run 'ses share login' once to configure the server URL and bearer token.
Use --dry-run to preview the scrubbed transcript without uploading.`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) == 0 {
			return cmd.Help()
		}
		session, err := store.GetSession(args[0])
		if err != nil {
			return err
		}
		messages, err := loadTranscript(session)
		if err != nil {
			return fmt.Errorf("loading transcript: %w", err)
		}
		mode, err := parseRedactMode(shareRedact)
		if err != nil {
			return err
		}
		scrubbed, report := redact.Redact(messages, redact.Options{Mode: mode})

		if shareDryRun {
			printDryRun(session, scrubbed, report, mode)
			return nil
		}
		return runUpload(session, scrubbed, report, mode)
	},
}

var shareLoginCmd = &cobra.Command{
	Use:   "login",
	Short: "Configure the share server URL and bearer token",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, _ := loadShareConfig()
		if shareLoginURL != "" {
			cfg.URL = strings.TrimRight(shareLoginURL, "/")
		}
		if shareLoginToken != "" {
			cfg.Token = shareLoginToken
		}
		if cfg.URL == "" || cfg.Token == "" {
			return fmt.Errorf("both --url and --token are required (run once to configure)")
		}
		if err := saveShareConfig(cfg); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "configured %s\n", cfg.URL)
		return nil
	},
}

var shareListCmd = &cobra.Command{
	Use:   "list",
	Short: "List share links you've created from this machine",
	RunE: func(cmd *cobra.Command, args []string) error {
		entries, err := loadShareLog()
		if err != nil {
			return err
		}
		now := time.Now()
		active := entries[:0]
		for _, e := range entries {
			if e.ExpiresAt.After(now) {
				active = append(active, e)
			}
		}
		if len(active) == 0 {
			fmt.Fprintln(os.Stderr, "no active shares")
			return nil
		}
		sort.Slice(active, func(i, j int) bool {
			return active[i].ExpiresAt.Before(active[j].ExpiresAt)
		})
		for _, e := range active {
			label := e.Name
			if label == "" {
				label = e.SessionShort
			}
			fmt.Printf("%s  expires %s  %s\n", e.URL, humanUntil(e.ExpiresAt), label)
		}
		return nil
	},
}

var shareRevokeCmd = &cobra.Command{
	Use:   "revoke <share-id>",
	Short: "Delete a share before it expires",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadShareConfig()
		if err != nil {
			return err
		}
		id := args[0]
		req, _ := http.NewRequest(http.MethodDelete, cfg.URL+"/v1/shares/"+id, nil)
		req.Header.Set("Authorization", "Bearer "+cfg.Token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return fmt.Errorf("revoke request: %w", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 300 {
			body, _ := io.ReadAll(resp.Body)
			return fmt.Errorf("revoke failed: %s: %s", resp.Status, strings.TrimSpace(string(body)))
		}
		removeFromShareLog(id)
		fmt.Fprintf(os.Stderr, "revoked %s\n", id)
		return nil
	},
}

// --- upload path ---------------------------------------------------------

func runUpload(session *model.Session, messages []model.Message, report redact.Report, mode redact.Mode) error {
	url, err := uploadShare(session, messages, report, mode, shareName, shareExpires)
	if err != nil {
		return err
	}
	// URL on stdout so `ses share <id> | pbcopy` works.
	fmt.Println(url)
	return nil
}

// uploadShare uploads a redacted snapshot share and returns its URL. Used by
// `ses share` and by the fleet worker to publish result sessions.
func uploadShare(session *model.Session, messages []model.Message, report redact.Report, mode redact.Mode, name, expires string) (string, error) {
	cfg, err := loadShareConfig()
	if err != nil {
		return "", err
	}
	lifetime, err := parseLifetime(expires)
	if err != nil {
		return "", err
	}

	// Tell the user what they're about to upload unless suppressed.
	fmt.Fprintf(os.Stderr, "uploading %s (%d messages, redaction=%s, %d chars scrubbed, expires in %s)\n",
		session.ShortID, len(messages), redactModeLabel(mode), report.Bytes, lifetime)

	body := map[string]any{
		"name":               name,
		"expires_in_seconds": int64(lifetime.Seconds()),
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
		"messages": toShareMessages(messages),
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	const maxBody = 10 << 20
	if len(buf) > maxBody {
		return "", fmt.Errorf("payload is %d bytes, exceeds 10 MB limit — try --redact=strict or share a smaller range", len(buf))
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
		Name:         name,
		SessionShort: session.ShortID,
		CreatedAt:    time.Now(),
		ExpiresAt:    parsed.ExpiresAt,
	})
	return parsed.URL, nil
}

func toShareMessages(messages []model.Message) []shareserver.ShareMsg {
	out := make([]shareserver.ShareMsg, len(messages))
	for i, m := range messages {
		out[i] = shareserver.ShareMsg{
			Role:     m.Role,
			Content:  m.Content,
			ToolName: m.ToolName,
			FilePath: m.FilePath,
		}
	}
	return out
}

// --- lifetime parsing ----------------------------------------------------
//
// time.ParseDuration doesn't understand "d" or "w"; folks will type "7d".

func parseLifetime(s string) (time.Duration, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return 0, fmt.Errorf("empty --expires")
	}
	last := s[len(s)-1]
	switch last {
	case 'd', 'w':
		n, err := strconv.Atoi(s[:len(s)-1])
		if err != nil || n <= 0 {
			return 0, fmt.Errorf("invalid --expires %q", s)
		}
		if last == 'd' {
			return time.Duration(n) * 24 * time.Hour, nil
		}
		return time.Duration(n) * 7 * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid --expires %q (try 1h, 24h, 7d)", s)
	}
	return d, nil
}

// --- config & share log --------------------------------------------------

type shareConfig struct {
	URL   string `json:"url"`
	Token string `json:"token"`
}

func shareConfigPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".ses", "share.json")
}

func loadShareConfig() (shareConfig, error) {
	var cfg shareConfig
	data, err := os.ReadFile(shareConfigPath())
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, fmt.Errorf("no share config — run `ses share login --url <base> --token <bearer>` first")
		}
		return cfg, err
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("corrupt share config at %s: %w", shareConfigPath(), err)
	}
	return cfg, nil
}

func saveShareConfig(cfg shareConfig) error {
	path := shareConfigPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(cfg, "", "  ")
	return os.WriteFile(path, data, 0o600)
}

type shareLogEntry struct {
	ID           string    `json:"id"`
	URL          string    `json:"url"`
	Name         string    `json:"name,omitempty"`
	SessionShort string    `json:"session_short"`
	CreatedAt    time.Time `json:"created_at"`
	ExpiresAt    time.Time `json:"expires_at"`
}

func shareLogPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".ses", "shares.log")
}

func loadShareLog() ([]shareLogEntry, error) {
	data, err := os.ReadFile(shareLogPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var entries []shareLogEntry
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var e shareLogEntry
		if json.Unmarshal([]byte(line), &e) == nil {
			entries = append(entries, e)
		}
	}
	return entries, nil
}

func appendShareLog(e shareLogEntry) {
	path := shareLogPath()
	_ = os.MkdirAll(filepath.Dir(path), 0o700)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	data, _ := json.Marshal(e)
	f.Write(data)
	f.Write([]byte("\n"))
}

func removeFromShareLog(id string) {
	entries, err := loadShareLog()
	if err != nil {
		return
	}
	kept := entries[:0]
	for _, e := range entries {
		if e.ID != id {
			kept = append(kept, e)
		}
	}
	f, err := os.OpenFile(shareLogPath(), os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	for _, e := range kept {
		data, _ := json.Marshal(e)
		f.Write(data)
		f.Write([]byte("\n"))
	}
}

// --- misc formatting -----------------------------------------------------

func parseRedactMode(s string) (redact.Mode, error) {
	switch strings.ToLower(s) {
	case "", "default":
		return redact.ModeDefault, nil
	case "off":
		return redact.ModeOff, nil
	case "strict":
		return redact.ModeStrict, nil
	}
	return 0, fmt.Errorf("invalid --redact %q (want off|default|strict)", s)
}

func printDryRun(session *model.Session, messages []model.Message, report redact.Report, mode redact.Mode) {
	fmt.Printf("# Share preview: %s\n\n", session.ShortID)
	fmt.Printf("Project:   %s\n", session.Project)
	fmt.Printf("Source:    %s\n", session.SourceType)
	fmt.Printf("Messages:  %d\n", len(messages))
	fmt.Printf("Redaction: %s\n\n", redactModeLabel(mode))

	fmt.Println("## Redaction report")
	if report.Total() == 0 {
		fmt.Println("(no matches — nothing was scrubbed)")
	} else {
		keys := make([]string, 0, len(report.Counts))
		for k := range report.Counts {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Printf("  %-14s %d\n", k, report.Counts[k])
		}
		fmt.Printf("  %-14s %d chars removed\n", "total:", report.Bytes)
	}

	fmt.Println("\n## Scrubbed transcript")
	for _, m := range messages {
		fmt.Printf("\n--- %s ---\n", m.Role)
		fmt.Println(m.Content)
	}
}

func redactModeLabel(m redact.Mode) string {
	switch m {
	case redact.ModeOff:
		return "off (no scrubbing)"
	case redact.ModeStrict:
		return "strict"
	default:
		return "default"
	}
}

func humanUntil(t time.Time) string {
	d := time.Until(t).Round(time.Minute)
	if d < time.Hour {
		return fmt.Sprintf("in %dm", int(d.Minutes()))
	}
	if d < 48*time.Hour {
		return fmt.Sprintf("in %dh", int(d.Hours()))
	}
	return fmt.Sprintf("in %dd", int(d.Hours()/24))
}

func init() {
	shareCmd.Flags().BoolVar(&shareDryRun, "dry-run", false, "print the redacted transcript to stdout without uploading")
	shareCmd.Flags().StringVar(&shareRedact, "redact", "default", "redaction mode: off|default|strict")
	shareCmd.Flags().StringVar(&shareExpires, "expires", "7d", "link lifetime, e.g. 1h, 24h, 7d")
	shareCmd.Flags().StringVar(&shareName, "name", "", "human label for the share")

	shareLoginCmd.Flags().StringVar(&shareLoginURL, "url", "", "base URL of the share server")
	shareLoginCmd.Flags().StringVar(&shareLoginToken, "token", "", "bearer token to authenticate uploads")

	shareCmd.AddCommand(shareLoginCmd)
	shareCmd.AddCommand(shareListCmd)
	shareCmd.AddCommand(shareRevokeCmd)
	rootCmd.AddCommand(shareCmd)
}
