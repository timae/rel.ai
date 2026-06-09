package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/fatih/color"
	"github.com/fsnotify/fsnotify"
	"github.com/spf13/cobra"
	"github.com/timae/ses/internal/display"
	"github.com/timae/ses/internal/fleet"
	"github.com/timae/ses/internal/fleetclient"
	"github.com/timae/ses/internal/scanner"
)

var (
	watchQuiet     bool
	watchInstall   bool
	watchUninstall bool
	watchStatus    bool
)

const (
	launchAgentLabel = "io.timae.ses.watch"
	// legacyWatchLabel is the pre-v0.6 label, kept only so install/uninstall
	// flows can clean up an old agent on machines upgraded from older builds.
	legacyWatchLabel = "ai.rel.ses.watch"
)

var watchCmd = &cobra.Command{
	Use:   "watch",
	Short: "Watch for new sessions and auto-index them",
	Long: `Watch for new sessions and auto-index them in real-time.

Daemon management (macOS):
  ses watch --install     Install as a LaunchAgent (starts on login)
  ses watch --uninstall   Remove the LaunchAgent
  ses watch --status      Check if the daemon is running`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if watchInstall {
			return installDaemon()
		}
		if watchUninstall {
			return uninstallDaemon()
		}
		if watchStatus {
			return daemonStatus()
		}

		return runWatcher()
	},
}

func runWatcher() error {
	home, _ := os.UserHomeDir()
	claudeDir := filepath.Join(home, ".claude", "projects")
	codexDir := filepath.Join(home, ".codex", "sessions")

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("creating watcher: %w", err)
	}
	defer watcher.Close()

	watched := 0
	for _, dir := range []string{claudeDir, codexDir} {
		if err := watchRecursive(watcher, dir); err == nil {
			watched++
		}
	}

	if watched == 0 {
		return fmt.Errorf("no session directories found to watch")
	}

	if !watchQuiet {
		color.New(color.FgCyan).Println("Watching for new sessions... (Ctrl+C to stop)")
	}

	var debounceTimer *time.Timer
	pendingFiles := make(map[string]bool)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	claudeScanner := scanner.NewClaudeScanner(filepath.Join(home, ".claude"))
	codexScanner := scanner.NewCodexScanner(filepath.Join(home, ".codex"))

	// Best-effort fleet heartbeat so non-worker seats still show up on the
	// capacity board. No-op unless `ses fleet login` has been run.
	heartbeat := time.NewTicker(15 * time.Minute)
	defer heartbeat.Stop()
	sendHeartbeat := func() {
		if err := fleetclient.MaybeHeartbeat(store, rootVersion(), fleet.WorkerStatus{State: "off"}); err != nil && !watchQuiet {
			fmt.Fprintf(os.Stderr, "fleet heartbeat: %v\n", err)
		}
	}
	sendHeartbeat()

	for {
		select {
		case <-heartbeat.C:
			sendHeartbeat()
		case event, ok := <-watcher.Events:
			if !ok {
				return nil
			}

			if !strings.HasSuffix(event.Name, ".jsonl") {
				if event.Has(fsnotify.Create) {
					if info, err := os.Stat(event.Name); err == nil && info.IsDir() {
						watcher.Add(event.Name)
					}
				}
				continue
			}

			if !event.Has(fsnotify.Create) && !event.Has(fsnotify.Write) {
				continue
			}

			pendingFiles[event.Name] = true

			if debounceTimer != nil {
				debounceTimer.Stop()
			}
			debounceTimer = time.AfterFunc(500*time.Millisecond, func() {
				for path := range pendingFiles {
					processFile(path, claudeScanner, codexScanner)
				}
				pendingFiles = make(map[string]bool)
			})

		case err, ok := <-watcher.Errors:
			if !ok {
				return nil
			}
			if !watchQuiet {
				fmt.Fprintf(os.Stderr, "watch error: %v\n", err)
			}

		case <-sigCh:
			if !watchQuiet {
				fmt.Println("\nStopped watching.")
			}
			return nil
		}
	}
}

func processFile(path string, claudeScanner *scanner.ClaudeScanner, codexScanner *scanner.CodexScanner) {
	info, err := os.Stat(path)
	if err != nil {
		return
	}

	var sf scanner.SessionFile
	var sc scanner.Scanner

	home, _ := os.UserHomeDir()
	claudeProjects := filepath.Join(home, ".claude", "projects")
	codexSessions := filepath.Join(home, ".codex", "sessions")

	if strings.HasPrefix(path, claudeProjects) {
		sessionID := strings.TrimSuffix(filepath.Base(path), ".jsonl")
		sf = scanner.SessionFile{
			TranscriptPath: path,
			SourceType:     "claude",
			SourceID:       sessionID,
			Mtime:          info.ModTime().Unix(),
			Size:           info.Size(),
		}
		sc = claudeScanner
	} else if strings.HasPrefix(path, codexSessions) {
		sessionID := scanner.ExtractCodexSessionID(filepath.Base(path))
		if sessionID == "" {
			return
		}
		sf = scanner.SessionFile{
			TranscriptPath: path,
			SourceType:     "codex",
			SourceID:       sessionID,
			Mtime:          info.ModTime().Unix(),
			Size:           info.Size(),
		}
		sc = codexScanner
	} else {
		return
	}

	session, messages, err := sc.Parse(sf)
	if err != nil {
		return
	}

	if err := store.InsertSession(session, messages); err != nil {
		return
	}

	if !watchQuiet {
		project := display.Truncate(session.Project, 40)
		prompt := display.Truncate(session.FirstPrompt, 50)
		color.New(color.FgGreen).Printf("+ %s ", session.ShortID[:8])
		fmt.Printf("(%s) %s — %s\n", session.SourceType, project, prompt)
	}
}

func watchRecursive(watcher *fsnotify.Watcher, root string) error {
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			return watcher.Add(path)
		}
		return nil
	})
}

// --- Daemon management ---

func launchAgentPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", launchAgentLabel+".plist")
}

func sesBinary() (string, error) {
	// Prefer the installed binary
	if path, err := exec.LookPath("ses"); err == nil {
		return path, nil
	}
	// Fall back to current executable
	return os.Executable()
}

func installDaemon() error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("daemon install is only supported on macOS (use systemd on Linux)")
	}

	bin, err := sesBinary()
	if err != nil {
		return fmt.Errorf("finding ses binary: %w", err)
	}

	home, _ := os.UserHomeDir()
	logPath := filepath.Join(home, ".ses", "watch.log")

	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>%s</string>
    <key>ProgramArguments</key>
    <array>
        <string>%s</string>
        <string>watch</string>
        <string>--quiet</string>
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
`, launchAgentLabel, bin, logPath, logPath)

	plistPath := launchAgentPath()

	// Migrate any pre-v0.6 agent + its plist so upgraders don't end up
	// with two daemons running side by side.
	migrateLegacyAgent(legacyWatchLabel)

	// Unload existing if present (tolerant — may not be loaded).
	bootoutAgent(launchAgentLabel)

	if err := os.MkdirAll(filepath.Dir(plistPath), 0755); err != nil {
		return fmt.Errorf("creating LaunchAgents dir: %w", err)
	}

	if err := os.WriteFile(plistPath, []byte(plist), 0644); err != nil {
		return fmt.Errorf("writing plist: %w", err)
	}

	if err := bootstrapAgent(plistPath, launchAgentLabel); err != nil {
		return fmt.Errorf("loading LaunchAgent: %w", err)
	}

	color.New(color.FgGreen).Println("Daemon installed and started.")
	fmt.Printf("  Binary:  %s\n", bin)
	fmt.Printf("  Plist:   %s\n", plistPath)
	fmt.Printf("  Log:     %s\n", logPath)
	fmt.Println("\nSessions will be indexed automatically from now on.")
	fmt.Println("The daemon starts on login and restarts if it crashes.")
	return nil
}

func uninstallDaemon() error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("daemon uninstall is only supported on macOS")
	}

	plistPath := launchAgentPath()

	if _, err := os.Stat(plistPath); os.IsNotExist(err) {
		fmt.Println("Daemon is not installed.")
		return nil
	}

	bootoutAgent(launchAgentLabel)
	migrateLegacyAgent(legacyWatchLabel)

	if err := os.Remove(plistPath); err != nil {
		return fmt.Errorf("removing plist: %w", err)
	}

	color.New(color.FgYellow).Println("Daemon uninstalled.")
	return nil
}

func daemonStatus() error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("daemon status is only supported on macOS")
	}

	plistPath := launchAgentPath()
	if _, err := os.Stat(plistPath); os.IsNotExist(err) {
		fmt.Println("Daemon is not installed.")
		fmt.Println("Run `ses watch --install` to install it.")
		return nil
	}

	out, err := exec.Command("launchctl", "list", launchAgentLabel).Output()
	if err != nil {
		color.New(color.FgYellow).Println("Daemon is installed but not running.")
		fmt.Println("Run `ses watch --install` to restart it.")
		return nil
	}

	color.New(color.FgGreen).Println("Daemon is running.")
	// Parse PID from launchctl output
	lines := strings.Split(string(out), "\n")
	for _, line := range lines {
		if strings.Contains(line, "PID") {
			fmt.Printf("  %s\n", strings.TrimSpace(line))
		}
	}

	home, _ := os.UserHomeDir()
	logPath := filepath.Join(home, ".ses", "watch.log")
	if info, err := os.Stat(logPath); err == nil {
		fmt.Printf("  Log: %s (%s)\n", logPath, display.FormatTime(info.ModTime()))
	}

	return nil
}

func init() {
	watchCmd.Flags().BoolVar(&watchQuiet, "quiet", false, "suppress output")
	watchCmd.Flags().BoolVar(&watchInstall, "install", false, "install as macOS LaunchAgent (starts on login)")
	watchCmd.Flags().BoolVar(&watchUninstall, "uninstall", false, "remove the LaunchAgent")
	watchCmd.Flags().BoolVar(&watchStatus, "status", false, "check daemon status")
	rootCmd.AddCommand(watchCmd)
}
