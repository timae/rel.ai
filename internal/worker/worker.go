package worker

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/timae/ses/internal/fleet"
	"github.com/timae/ses/internal/fleetclient"
)

// Hooks are the cmd-level operations the worker needs but that live with the
// CLI's share/handoff plumbing: turning a handoff URL into a context blob,
// and publishing the result session as a redacted share link.
type Hooks struct {
	// ConsumeHandoff claims the single-use handoff and renders its context
	// blob. Optional failure is non-fatal: the task still runs on prompt only.
	ConsumeHandoff func(url string) (string, error)
	// ShareResultSession indexes the headless run's transcript and uploads a
	// redacted snapshot share, returning its URL. Best-effort.
	ShareResultSession func(sessionID, title string) (string, error)
	// WorkerSpentToday returns weighted tokens this worker already burned
	// today (from the audit log), for the daily cap.
	Log func(format string, args ...any)
}

// Run is the worker poll loop. It exits when ctx is canceled or the STOP
// file appears.
func Run(ctx context.Context, client *fleetclient.Client, p Policy, hooks Hooks) error {
	logf := hooks.Log
	if logf == nil {
		logf = func(format string, args ...any) { fmt.Fprintf(os.Stderr, format+"\n", args...) }
	}
	if len(p.ReposAllowed) == 0 {
		return fmt.Errorf("no repos_allowed configured — run `ses away config --allow-repo <prefix>` first; the worker claims nothing without an explicit allowlist")
	}
	hostname, _ := os.Hostname()

	logf("worker started (repos: %v, cap %d tokens/day, windows: %v)", p.ReposAllowed, p.MaxTokensPerDay, p.Windows)
	appendAudit(p.Workdir, AuditEntry{Event: "worker_started"})

	heartbeatTicker := time.NewTicker(15 * time.Minute)
	defer heartbeatTicker.Stop()
	sendHeartbeat := func(state, runningTask string) {
		hb := fleet.WorkerStatus{Enabled: true, State: state, RunningTask: runningTask}
		if err := heartbeatFn(client, hb); err != nil {
			logf("heartbeat: %v", err)
		}
	}
	sendHeartbeat("idle", "")

	poll := time.NewTicker(time.Duration(p.PollSeconds) * time.Second)
	defer poll.Stop()

	for {
		select {
		case <-ctx.Done():
			appendAudit(p.Workdir, AuditEntry{Event: "worker_stopped"})
			sendHeartbeat("off", "")
			return nil
		case <-heartbeatTicker.C:
			sendHeartbeat("idle", "")
		case <-poll.C:
		}

		if _, err := os.Stat(p.StopFile()); err == nil {
			logf("STOP file present (%s) — exiting", p.StopFile())
			appendAudit(p.Workdir, AuditEntry{Event: "worker_stopped", Error: "stop file"})
			sendHeartbeat("off", "")
			return nil
		}
		if !p.InWindow(time.Now()) {
			continue
		}
		spent := spentToday(p.Workdir)
		if p.MaxTokensPerDay > 0 && spent >= p.MaxTokensPerDay {
			continue // daily budget exhausted; try again tomorrow
		}

		budget := p.MaxTokensPerDay - spent
		task, err := client.Claim(fleet.ClaimRequest{
			Hostname:     hostname,
			ReposAllowed: p.ReposAllowed,
			MaxTokens:    budget,
			LeaseSeconds: p.LeaseSeconds,
		})
		if err != nil {
			logf("claim: %v", err)
			continue
		}
		if task == nil {
			continue // queue empty
		}

		logf("claimed task %s (%s) repo=%s budget=%d tokens", task.ID, task.Title, task.RepoURL, task.MaxTokens)
		appendAudit(p.Workdir, AuditEntry{TaskID: task.ID, Title: task.Title, RepoURL: task.RepoURL, Event: "claimed"})
		sendHeartbeat("running", task.ID)

		runOne(ctx, client, p, hooks, *task, logf)

		sendHeartbeat("idle", "")
	}
}

func runOne(ctx context.Context, client *fleetclient.Client, p Policy, hooks Hooks, task fleet.Task, logf func(string, ...any)) {
	contextBlob := ""
	if task.HandoffURL != "" && hooks.ConsumeHandoff != nil {
		blob, err := hooks.ConsumeHandoff(task.HandoffURL)
		if err != nil {
			logf("task %s: consuming handoff failed (continuing on prompt only): %v", task.ID, err)
		} else {
			contextBlob = blob
		}
	}

	lease := func(tokens int64) (bool, error) {
		resp, err := client.RenewLease(task.ID, fleet.LeaseRequest{
			TokensUsed:   tokens,
			State:        fleet.StatusRunning,
			LeaseSeconds: p.LeaseSeconds,
		})
		return resp.Cancel, err
	}

	appendAudit(p.Workdir, AuditEntry{TaskID: task.ID, Event: "started"})
	res := RunTask(ctx, task, contextBlob, p, lease)

	shareURL := ""
	if res.SessionID != "" && hooks.ShareResultSession != nil {
		url, err := hooks.ShareResultSession(res.SessionID, task.Title)
		if err != nil {
			logf("task %s: sharing result session failed: %v", task.ID, err)
		} else {
			shareURL = url
		}
	}

	if err := client.ReportResult(task.ID, fleet.ResultRequest{
		Status:         res.Status,
		ResultBranch:   res.Branch,
		ResultShareURL: shareURL,
		TokensUsed:     res.TokensUsed,
		NumTurns:       res.NumTurns,
		LogTail:        res.LogTail,
		Error:          res.Error,
	}); err != nil {
		logf("task %s: reporting result: %v", task.ID, err)
	}

	appendAudit(p.Workdir, AuditEntry{
		TaskID: task.ID, Title: task.Title, RepoURL: task.RepoURL, Branch: res.Branch,
		Event: res.Status, TokensUsed: res.TokensUsed, NumTurns: res.NumTurns, Error: res.Error,
	})
	logf("task %s finished: %s (branch=%s tokens=%d turns=%d) %s",
		task.ID, res.Status, res.Branch, res.TokensUsed, res.NumTurns, res.Error)
}

// spentToday sums weighted tokens from today's audit entries — the local,
// server-independent enforcement of the daily donation cap.
func spentToday(workdir string) int64 {
	var total int64
	today := time.Now().Format("2006-01-02")
	for _, e := range RecentAudit(workdir, 500) {
		if e.At.Format("2006-01-02") == today {
			total += e.TokensUsed
		}
	}
	return total
}

// heartbeatFn is indirect so cmd can wire the local-DB usage heartbeat in;
// the default sends a worker-status-only heartbeat.
var heartbeatFn = func(client *fleetclient.Client, ws fleet.WorkerStatus) error {
	hostname, _ := os.Hostname()
	return client.Heartbeat(fleet.Heartbeat{Hostname: hostname, Worker: ws})
}

// SetHeartbeatFunc replaces the heartbeat sender (used by cmd to include
// local usage rollups).
func SetHeartbeatFunc(f func(client *fleetclient.Client, ws fleet.WorkerStatus) error) {
	if f != nil {
		heartbeatFn = f
	}
}
