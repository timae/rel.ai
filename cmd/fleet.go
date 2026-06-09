package cmd

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/fatih/color"
	"github.com/spf13/cobra"
	"github.com/timae/ses/internal/fleet"
	"github.com/timae/ses/internal/fleetclient"
)

var fleetCmd = &cobra.Command{
	Use:   "fleet",
	Short: "Team capacity board and task queue",
	Long: `Commands for the fleet coordinator: see who has token headroom,
enqueue background tasks, and track their progress.

Tasks are executed by teammates' opt-in workers (see 'ses away') on their
own machines under their own credentials — capacity is shared by moving
work to idle seats, never by sharing accounts.`,
}

// --- login -------------------------------------------------------------------

var (
	fleetLoginURL string
	fleetLoginKey string
)

var fleetLoginCmd = &cobra.Command{
	Use:   "login",
	Short: "Store the fleet coordinator URL and your API key",
	RunE: func(cmd *cobra.Command, args []string) error {
		if fleetLoginURL == "" || fleetLoginKey == "" {
			return fmt.Errorf("both --url and --key are required (ask your fleet admin for a key)")
		}
		if err := fleetclient.SaveConfig(fleetclient.Config{URL: fleetLoginURL, Key: fleetLoginKey}); err != nil {
			return err
		}
		// Send a first heartbeat so the seat appears on the board immediately.
		if err := fleetclient.MaybeHeartbeat(store, rootVersion(), fleet.WorkerStatus{State: "off"}); err != nil {
			fmt.Fprintf(os.Stderr, "saved config, but first heartbeat failed: %v\n", err)
			return nil
		}
		fmt.Printf("Logged in to %s (config: %s)\n", fleetLoginURL, fleetclient.ConfigPath())
		return nil
	},
}

// --- board -------------------------------------------------------------------

var fleetBoardCmd = &cobra.Command{
	Use:   "board",
	Short: "Show the team capacity board",
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := fleetclient.Load()
		if err != nil {
			return err
		}
		seats, err := c.Capacity()
		if err != nil {
			return err
		}
		if len(seats) == 0 {
			fmt.Println("No seats enrolled yet.")
			return nil
		}

		bold := color.New(color.Bold).SprintFunc()
		dim := color.New(color.Faint).SprintFunc()
		green := color.New(color.FgGreen).SprintFunc()
		yellow := color.New(color.FgYellow).SprintFunc()

		fmt.Printf("%-28s %-10s %-10s %-10s %-8s %s\n",
			bold("SEAT"), bold("USED 7D"), bold("BUDGET"), bold("AVAIL"), bold("WORKER"), bold("LAST SEEN"))
		for _, s := range seats {
			name := s.Email
			avail := fmtTokenCount(s.AvailableTokens)
			if s.WeeklyBudget == 0 {
				avail = dim("n/a")
			} else if s.AvailableTokens > s.WeeklyBudget/2 {
				avail = green(avail)
			}
			worker := s.WorkerState
			switch s.WorkerState {
			case "running":
				worker = yellow("running")
			case "idle":
				worker = green("idle")
			default:
				worker = dim("off")
			}
			lastSeen := dim("never")
			if s.LastSeen != "" {
				if t, err := time.Parse(time.RFC3339, s.LastSeen); err == nil {
					age := time.Since(t)
					lastSeen = formatAge(age)
					if age > 24*time.Hour {
						lastSeen = yellow(lastSeen + " (stale)")
					}
				}
			}
			fmt.Printf("%-28s %-10s %-10s %-10s %-8s %s\n",
				name, fmtTokenCount(s.Tokens7d), fmtTokenCount(s.WeeklyBudget), avail, worker, lastSeen)
			if s.RunningTask != "" {
				fmt.Printf("%-28s %s\n", "", dim("└ task "+s.RunningTask))
			}
		}
		return nil
	},
}

// --- enqueue -----------------------------------------------------------------

var (
	enqRepo       string
	enqBranch     string
	enqPrompt     string
	enqPromptFile string
	enqHandoff    string
	enqMaxTokens  int64
	enqMaxTurns   int
	enqPriority   int
	enqTitle      string
	enqExpires    string
)

var fleetEnqueueCmd = &cobra.Command{
	Use:   "enqueue",
	Short: "Queue a background task for an idle worker to pick up",
	Long: `Queue a task. A teammate's opt-in worker will clone the repo, run
Claude Code headlessly with your prompt, push a result branch, and report
back with a link to the result session.

Attach context from one of your indexed sessions with --handoff <short-id>;
the worker consumes the single-use handoff exactly like a human recipient.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if enqPrompt == "" && enqPromptFile != "" {
			data, err := os.ReadFile(enqPromptFile)
			if err != nil {
				return err
			}
			enqPrompt = string(data)
		}
		if strings.TrimSpace(enqPrompt) == "" {
			return fmt.Errorf("--prompt or --prompt-file is required")
		}
		if enqRepo == "" {
			return fmt.Errorf("--repo is required")
		}

		lifetime, err := parseLifetime(enqExpires)
		if err != nil {
			return err
		}

		handoffURL := ""
		if enqHandoff != "" {
			// The handoff share must outlive the queued task.
			handoffURL, err = createHandoff(enqHandoff, "attached to fleet task: "+enqTitle, enqExpires, "default", false)
			if err != nil {
				return fmt.Errorf("creating handoff bundle: %w", err)
			}
		}

		c, err := fleetclient.Load()
		if err != nil {
			return err
		}
		t, err := c.Enqueue(fleet.EnqueueRequest{
			Title:            enqTitle,
			Prompt:           enqPrompt,
			RepoURL:          enqRepo,
			BaseBranch:       enqBranch,
			HandoffURL:       handoffURL,
			MaxTokens:        enqMaxTokens,
			MaxTurns:         enqMaxTurns,
			Priority:         enqPriority,
			ExpiresInSeconds: int64(lifetime.Seconds()),
		})
		if err != nil {
			return err
		}
		fmt.Printf("Queued task %s (priority %d, budget %s tokens)\n", t.ID, t.Priority, fmtTokenCount(t.MaxTokens))
		return nil
	},
}

// --- tasks / task / cancel ------------------------------------------------------

var (
	tasksStatus string
	tasksMine   bool
)

var fleetTasksCmd = &cobra.Command{
	Use:   "tasks",
	Short: "List fleet tasks",
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := fleetclient.Load()
		if err != nil {
			return err
		}
		tasks, err := c.ListTasks(tasksStatus, tasksMine)
		if err != nil {
			return err
		}
		if len(tasks) == 0 {
			fmt.Println("No tasks.")
			return nil
		}
		bold := color.New(color.Bold).SprintFunc()
		fmt.Printf("%-24s %-9s %-22s %-18s %s\n", bold("ID"), bold("STATUS"), bold("TITLE"), bold("BY"), bold("WORKER"))
		for _, t := range tasks {
			title := t.Title
			if title == "" {
				title = "(untitled)"
			}
			worker := t.ClaimedBy
			if worker != "" && t.ClaimedHost != "" {
				worker += "@" + t.ClaimedHost
			}
			fmt.Printf("%-24s %-9s %-22s %-18s %s\n",
				t.ID, t.Status, truncate(title, 20), truncate(t.CreatedBy, 16), worker)
		}
		return nil
	},
}

var fleetTaskCmd = &cobra.Command{
	Use:   "task <id>",
	Short: "Show a task's details and event log",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := fleetclient.Load()
		if err != nil {
			return err
		}
		d, err := c.GetTask(args[0])
		if err != nil {
			return err
		}
		t := d.Task
		bold := color.New(color.Bold).SprintFunc()
		fmt.Printf("%s %s\n", bold("Task"), t.ID)
		printField("Status", t.Status)
		printField("Title", t.Title)
		printField("By", t.CreatedBy)
		printField("Repo", t.RepoURL)
		printField("Base branch", t.BaseBranch)
		printField("Budget", fmtTokenCount(t.MaxTokens)+" tokens, "+fmt.Sprintf("%d turns", t.MaxTurns))
		printField("Handoff", t.HandoffURL)
		printField("Worker", strings.TrimPrefix(t.ClaimedBy+"@"+t.ClaimedHost, "@"))
		printField("Result branch", t.ResultBranch)
		printField("Result session", t.ResultShareURL)
		if t.TokensUsed > 0 {
			printField("Tokens used", fmtTokenCount(t.TokensUsed))
		}
		printField("Error", t.Error)
		if t.Prompt != "" {
			fmt.Printf("\n%s\n%s\n", bold("Prompt"), t.Prompt)
		}
		if len(d.Events) > 0 {
			fmt.Printf("\n%s\n", bold("Events"))
			for _, e := range d.Events {
				detail := ""
				if e.Detail != "" {
					detail = " — " + truncate(strings.ReplaceAll(e.Detail, "\n", " "), 100)
				}
				fmt.Printf("  %s  %-18s %s%s\n", e.At, e.Event, e.Actor, detail)
			}
		}
		return nil
	},
}

var fleetCancelCmd = &cobra.Command{
	Use:   "cancel <id>",
	Short: "Cancel a queued task (or request cancel of a running one)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := fleetclient.Load()
		if err != nil {
			return err
		}
		t, err := c.Cancel(args[0])
		if err != nil {
			return err
		}
		if t.Status == fleet.StatusCanceled {
			fmt.Printf("Task %s canceled.\n", t.ID)
		} else {
			fmt.Printf("Cancel requested for %s (currently %s) — the worker stops at its next lease renewal.\n", t.ID, t.Status)
		}
		return nil
	},
}

// --- report ---------------------------------------------------------------------

var (
	reportSince string
	reportUntil string
)

var fleetReportCmd = &cobra.Command{
	Use:   "report",
	Short: "Per-seat utilization report for license right-sizing (admin)",
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := fleetclient.Load()
		if err != nil {
			return err
		}
		rep, err := c.Report(reportSince, reportUntil)
		if err != nil {
			return err
		}
		bold := color.New(color.Bold).SprintFunc()
		fmt.Printf("Utilization %s — %s\n\n", rep.Since, rep.Until)
		fmt.Printf("%-28s %-12s %-10s %-12s %-8s %s\n",
			bold("SEAT"), bold("WEIGHTED"), bold("DAYS"), bold("SESSIONS"), bold("UTIL%"), bold("BUDGET/WK"))
		for _, u := range rep.Users {
			fmt.Printf("%-28s %-12s %-10d %-12d %-8d %s\n",
				u.Email, fmtTokenCount(u.WeightedTokens), u.ActiveDays, u.Sessions,
				u.UtilizationPct, fmtTokenCount(u.WeeklyBudget))
		}
		return nil
	},
}

// --- helpers ----------------------------------------------------------------------

func printField(label, value string) {
	if value == "" {
		return
	}
	fmt.Printf("  %-16s %s\n", label, value)
}

func fmtTokenCount(n int64) string {
	switch {
	case n >= 1_000_000_000:
		return fmt.Sprintf("%.1fB", float64(n)/1e9)
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	}
	return fmt.Sprintf("%d", n)
}

func formatAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}

// rootVersion returns the CLI version for heartbeats; kept indirect so a
// build-time -ldflags override on cmd.Version flows through.
func rootVersion() string {
	if rootCmd.Version != "" {
		return rootCmd.Version
	}
	return "dev"
}

func init() {
	fleetLoginCmd.Flags().StringVar(&fleetLoginURL, "url", "", "fleet coordinator base URL")
	fleetLoginCmd.Flags().StringVar(&fleetLoginKey, "key", "", "your personal API key")

	fleetEnqueueCmd.Flags().StringVar(&enqRepo, "repo", "", "git repo URL the worker should clone (required)")
	fleetEnqueueCmd.Flags().StringVar(&enqBranch, "branch", "", "base branch (default: repo default)")
	fleetEnqueueCmd.Flags().StringVar(&enqPrompt, "prompt", "", "task prompt for the headless run")
	fleetEnqueueCmd.Flags().StringVar(&enqPromptFile, "prompt-file", "", "read the prompt from a file")
	fleetEnqueueCmd.Flags().StringVar(&enqHandoff, "handoff", "", "attach context from one of your sessions (short id)")
	fleetEnqueueCmd.Flags().Int64Var(&enqMaxTokens, "max-tokens", 500_000, "weighted token budget for the run")
	fleetEnqueueCmd.Flags().IntVar(&enqMaxTurns, "max-turns", 30, "max agent turns")
	fleetEnqueueCmd.Flags().IntVar(&enqPriority, "priority", 5, "1 (high) … 9 (low)")
	fleetEnqueueCmd.Flags().StringVar(&enqTitle, "title", "", "short human-readable title")
	fleetEnqueueCmd.Flags().StringVar(&enqExpires, "expires", "168h", "give up if unclaimed for this long")

	fleetTasksCmd.Flags().StringVar(&tasksStatus, "status", "", "filter: queued|claimed|running|done|failed|canceled|expired")
	fleetTasksCmd.Flags().BoolVar(&tasksMine, "mine", false, "only tasks I enqueued")

	fleetReportCmd.Flags().StringVar(&reportSince, "since", "", "from date YYYY-MM-DD (default: 28 days ago)")
	fleetReportCmd.Flags().StringVar(&reportUntil, "until", "", "until date YYYY-MM-DD (default: today)")

	fleetCmd.AddCommand(fleetLoginCmd, fleetBoardCmd, fleetEnqueueCmd, fleetTasksCmd, fleetTaskCmd, fleetCancelCmd, fleetReportCmd)
	rootCmd.AddCommand(fleetCmd)
}
