package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/fatih/color"
	"github.com/spf13/cobra"
	"github.com/timae/ses/internal/fleet"
	"github.com/timae/ses/internal/fleetclient"
	"github.com/timae/ses/internal/model"
	"github.com/timae/ses/internal/redact"
	"github.com/timae/ses/internal/scanner"
	"github.com/timae/ses/internal/worker"
)

const awayAgentLabel = "io.timae.ses.away"

var (
	awayInstall   bool
	awayUninstall bool
)

var awayCmd = &cobra.Command{
	Use:   "away",
	Short: "Donate idle token capacity: run the opt-in fleet worker",
	Long: `Run the fleet worker on this machine. While you're off, it claims
queued team tasks that match your policy, runs Claude Code headlessly under
YOUR account on YOUR machine, pushes a result branch, and reports back.

Nothing runs without your standing consent: the worker only touches repos
you allowlisted, stays inside your daily token cap and time windows, and
keeps a local audit log (~/.ses/worker/audit.jsonl). Stop it any time with
'ses away off'. Your credentials never leave this machine.

Configure first:
  ses fleet login --url <coordinator> --key <your-key>
  ses away config --allow-repo git@github.com:acme/ --max-tokens-per-day 2000000
  ses away on && ses away --install   # or run 'ses away' in the foreground`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if awayInstall {
			return installAwayDaemon()
		}
		if awayUninstall {
			return uninstallAwayDaemon()
		}
		return runAwayWorker()
	},
}

func runAwayWorker() error {
	p, err := worker.LoadPolicy()
	if err != nil {
		return err
	}
	if !p.Enabled {
		return fmt.Errorf("worker is disabled — run `ses away on` first")
	}
	if _, err := os.Stat(p.StopFile()); err == nil {
		return fmt.Errorf("STOP file present (%s) — run `ses away on` to clear it", p.StopFile())
	}
	client, err := fleetclient.Load()
	if err != nil {
		return err
	}

	// Heartbeats from the worker include local usage rollups so the seat's
	// capacity stays current on the board while its owner is away.
	worker.SetHeartbeatFunc(func(c *fleetclient.Client, ws fleet.WorkerStatus) error {
		hb, err := fleetclient.BuildHeartbeat(store, rootVersion(), ws)
		if err != nil {
			return err
		}
		return c.Heartbeat(hb)
	})

	hooks := worker.Hooks{
		ConsumeHandoff:     consumeHandoffContext,
		ShareResultSession: shareResultSession,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return worker.Run(ctx, client, p, hooks)
}

// consumeHandoffContext claims a single-use handoff URL and renders the same
// context blob a human recipient of `ses resume --from` would get.
func consumeHandoffContext(rawURL string) (string, error) {
	base, id, err := parseShareURL(rawURL)
	if err != nil {
		return "", err
	}
	share, err := consumeHandoff(base, id)
	if err != nil {
		return "", err
	}
	savedPath, _ := saveHandoff(share)
	return buildHandoffContextBlob(share, toModelMessages(share.Messages), savedPath), nil
}

// shareResultSession indexes the headless run's transcript and uploads a
// redacted snapshot share so the enqueuer can read what happened.
func shareResultSession(sessionID, title string) (string, error) {
	home, _ := os.UserHomeDir()
	claudeHome := filepath.Join(home, ".claude")

	// The headless run just wrote its transcript; index it.
	cs := scanner.NewClaudeScanner(claudeHome)
	files, err := cs.Discover()
	if err != nil {
		return "", err
	}
	for _, sf := range files {
		if sf.SourceID != sessionID {
			continue
		}
		session, messages, err := cs.Parse(sf)
		if err != nil {
			return "", fmt.Errorf("parsing result transcript: %w", err)
		}
		if err := store.InsertSession(session, messages); err != nil {
			return "", fmt.Errorf("indexing result session: %w", err)
		}
		scrubbed, report := redact.Redact(transcriptMessages(messages), redact.Options{Mode: redact.ModeDefault})
		name := "fleet result: " + title
		return uploadShare(session, scrubbed, report, redact.ModeDefault, name, "7d")
	}
	return "", fmt.Errorf("transcript for session %s not found under %s", sessionID, claudeHome)
}

// transcriptMessages drops tool_use placeholder messages, mirroring what
// loadTranscript feeds the share path.
func transcriptMessages(in []model.Message) []model.Message {
	out := make([]model.Message, 0, len(in))
	for _, m := range in {
		if m.Role == "user" || m.Role == "assistant" || m.Role == "tool_result" {
			out = append(out, m)
		}
	}
	return out
}

// --- on / off / status / config ---------------------------------------------

var awayOnCmd = &cobra.Command{
	Use:   "on",
	Short: "Enable the worker (clears the STOP kill switch)",
	RunE: func(cmd *cobra.Command, args []string) error {
		p, err := worker.LoadPolicy()
		if err != nil {
			return err
		}
		p.Enabled = true
		if err := worker.SavePolicy(p); err != nil {
			return err
		}
		os.Remove(p.StopFile())
		fmt.Println("Worker enabled. Run `ses away` (foreground) or `ses away --install` (daemon).")
		return nil
	},
}

var awayOffCmd = &cobra.Command{
	Use:   "off",
	Short: "Stop the worker immediately (kill switch)",
	RunE: func(cmd *cobra.Command, args []string) error {
		p, err := worker.LoadPolicy()
		if err != nil {
			return err
		}
		p.Enabled = false
		if err := worker.SavePolicy(p); err != nil {
			return err
		}
		if err := os.MkdirAll(p.Workdir, 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(p.StopFile(), []byte(time.Now().Format(time.RFC3339)+"\n"), 0o600); err != nil {
			return err
		}
		// A daemonized worker would exit and be restarted by launchd in a
		// stopped loop; boot it out entirely.
		if runtime.GOOS == "darwin" {
			bootoutAgent(awayAgentLabel)
		}
		color.New(color.FgYellow).Println("Worker stopped: STOP file written, daemon (if any) unloaded.")
		fmt.Println("A task in flight is terminated and reported as failed; the coordinator requeues it.")
		return nil
	},
}

var awayStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show worker policy, daemon state, and recent activity",
	RunE: func(cmd *cobra.Command, args []string) error {
		p, err := worker.LoadPolicy()
		if err != nil {
			return err
		}
		state := "disabled"
		if p.Enabled {
			state = "enabled"
		}
		if _, err := os.Stat(p.StopFile()); err == nil {
			state += " (STOP file present)"
		}
		fmt.Printf("Worker:        %s\n", state)
		fmt.Printf("Repos allowed: %v\n", p.ReposAllowed)
		fmt.Printf("Daily cap:     %s weighted tokens\n", fmtTokenCount(p.MaxTokensPerDay))
		if len(p.Windows) > 0 {
			fmt.Printf("Windows:       %v\n", p.Windows)
		} else {
			fmt.Printf("Windows:       always\n")
		}
		fmt.Printf("Task timeout:  %dm\n", p.TaskTimeoutMinutes)
		fmt.Printf("Allowed tools: %s\n", p.AllowedTools)
		fmt.Printf("Config:        %s\n", worker.PolicyPath())
		fmt.Printf("Audit log:     %s\n", filepath.Join(p.Workdir, "audit.jsonl"))

		if entries := worker.RecentAudit(p.Workdir, 10); len(entries) > 0 {
			fmt.Println("\nRecent activity:")
			for _, e := range entries {
				line := fmt.Sprintf("  %s  %-16s", e.At.Format("2006-01-02 15:04"), e.Event)
				if e.TaskID != "" {
					line += " " + e.TaskID
				}
				if e.TokensUsed > 0 {
					line += fmt.Sprintf(" (%s tokens)", fmtTokenCount(e.TokensUsed))
				}
				if e.Error != "" {
					line += " — " + e.Error
				}
				fmt.Println(line)
			}
		}
		return nil
	},
}

var (
	cfgAllowRepos   []string
	cfgMaxPerDay    int64
	cfgWindows      []string
	cfgTools        string
	cfgTimeout      int
	cfgClearRepos   bool
	cfgClearWindows bool
)

var awayConfigCmd = &cobra.Command{
	Use:   "config",
	Short: "Set the worker policy (repo allowlist, caps, schedule)",
	RunE: func(cmd *cobra.Command, args []string) error {
		p, err := worker.LoadPolicy()
		if err != nil {
			return err
		}
		if cfgClearRepos {
			p.ReposAllowed = nil
		}
		p.ReposAllowed = append(p.ReposAllowed, cfgAllowRepos...)
		if cfgClearWindows {
			p.Windows = nil
		}
		p.Windows = append(p.Windows, cfgWindows...)
		if cmd.Flags().Changed("max-tokens-per-day") {
			p.MaxTokensPerDay = cfgMaxPerDay
		}
		if cmd.Flags().Changed("allowed-tools") {
			p.AllowedTools = cfgTools
		}
		if cmd.Flags().Changed("timeout") {
			p.TaskTimeoutMinutes = cfgTimeout
		}
		if err := worker.SavePolicy(p); err != nil {
			return err
		}
		fmt.Printf("Policy saved to %s\n", worker.PolicyPath())
		return nil
	},
}

// --- LaunchAgent ---------------------------------------------------------------

func awayAgentPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", awayAgentLabel+".plist")
}

func installAwayDaemon() error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("daemon install is only supported on macOS (run `ses away` under systemd on Linux)")
	}
	p, err := worker.LoadPolicy()
	if err != nil {
		return err
	}
	if !p.Enabled {
		return fmt.Errorf("enable the worker first: ses away on")
	}
	if len(p.ReposAllowed) == 0 {
		return fmt.Errorf("configure a repo allowlist first: ses away config --allow-repo <prefix>")
	}
	bin, err := sesBinary()
	if err != nil {
		return fmt.Errorf("finding ses binary: %w", err)
	}
	home, _ := os.UserHomeDir()
	logPath := filepath.Join(home, ".ses", "away.log")

	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>%s</string>
    <key>ProgramArguments</key>
    <array>
        <string>%s</string>
        <string>away</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>StandardOutPath</key>
    <string>%s</string>
    <key>StandardErrorPath</key>
    <string>%s</string>
    <key>ProcessType</key>
    <string>Background</string>
</dict>
</plist>
`, awayAgentLabel, bin, logPath, logPath)

	plistPath := awayAgentPath()
	bootoutAgent(awayAgentLabel)
	if err := os.MkdirAll(filepath.Dir(plistPath), 0755); err != nil {
		return fmt.Errorf("creating LaunchAgents dir: %w", err)
	}
	if err := os.WriteFile(plistPath, []byte(plist), 0644); err != nil {
		return fmt.Errorf("writing plist: %w", err)
	}
	if err := bootstrapAgent(plistPath, awayAgentLabel); err != nil {
		return fmt.Errorf("loading LaunchAgent: %w", err)
	}

	color.New(color.FgGreen).Println("Worker daemon installed and started.")
	fmt.Printf("  Plist: %s\n  Log:   %s\n", plistPath, logPath)
	fmt.Println("\nStop it any time with `ses away off`.")
	return nil
}

func uninstallAwayDaemon() error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("daemon uninstall is only supported on macOS")
	}
	plistPath := awayAgentPath()
	if _, err := os.Stat(plistPath); os.IsNotExist(err) {
		fmt.Println("Worker daemon is not installed.")
		return nil
	}
	bootoutAgent(awayAgentLabel)
	if err := os.Remove(plistPath); err != nil {
		return fmt.Errorf("removing plist: %w", err)
	}
	color.New(color.FgYellow).Println("Worker daemon uninstalled.")
	return nil
}

func init() {
	awayCmd.Flags().BoolVar(&awayInstall, "install", false, "install as macOS LaunchAgent (starts on login)")
	awayCmd.Flags().BoolVar(&awayUninstall, "uninstall", false, "remove the LaunchAgent")

	awayConfigCmd.Flags().StringSliceVar(&cfgAllowRepos, "allow-repo", nil, "repo URL prefix to allow (repeatable)")
	awayConfigCmd.Flags().BoolVar(&cfgClearRepos, "clear-repos", false, "reset the repo allowlist before applying --allow-repo")
	awayConfigCmd.Flags().Int64Var(&cfgMaxPerDay, "max-tokens-per-day", 2_000_000, "weighted token cap per local day")
	awayConfigCmd.Flags().StringSliceVar(&cfgWindows, "window", nil, `time window like "Sat 00:00-24:00" (repeatable; empty = always)`)
	awayConfigCmd.Flags().BoolVar(&cfgClearWindows, "clear-windows", false, "reset windows before applying --window")
	awayConfigCmd.Flags().StringVar(&cfgTools, "allowed-tools", "", "--allowedTools value passed to claude")
	awayConfigCmd.Flags().IntVar(&cfgTimeout, "timeout", 90, "per-task timeout in minutes")

	awayCmd.AddCommand(awayOnCmd, awayOffCmd, awayStatusCmd, awayConfigCmd)
	rootCmd.AddCommand(awayCmd)
}
