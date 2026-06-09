package worker

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/timae/ses/internal/fleet"
)

// RunResult is what one task execution produced, regardless of outcome.
type RunResult struct {
	Status     string // done|failed
	Branch     string
	SessionID  string // headless Claude Code session id, for the result share
	TokensUsed int64  // weighted
	NumTurns   int
	LogTail    string
	Error      string
}

// LeaseFunc renews the coordinator lease mid-run and returns true when a
// remote cancel was requested.
type LeaseFunc func(tokensUsed int64) (cancel bool, err error)

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

func slugify(s string) string {
	s = slugRe.ReplaceAllString(strings.ToLower(s), "-")
	s = strings.Trim(s, "-")
	if len(s) > 32 {
		s = s[:32]
	}
	if s == "" {
		s = "task"
	}
	return s
}

const workerPreamble = `# Unattended fleet task

You are running unattended on a teammate's machine; nobody will answer
questions. Work autonomously to complete the task below.

Rules:
- Commit your work in logical steps with clear commit messages.
- Do NOT push; the harness pushes your branch when you finish.
- Do not touch anything outside this repository checkout.
- Finish with a short summary of what you did and what (if anything) remains.
`

// RunTask executes one claimed task: clone, branch, headless claude run with
// live budget/turn/cancel enforcement, commit + push, cleanup.
func RunTask(ctx context.Context, task fleet.Task, contextBlob string, p Policy, lease LeaseFunc) RunResult {
	branch := fmt.Sprintf("fleet/%s-%s", shortID(task.ID), slugify(task.Title))
	res := RunResult{Status: fleet.StatusFailed, Branch: ""}

	taskDir := filepath.Join(p.Workdir, "tasks", task.ID)
	repoDir := filepath.Join(taskDir, "repo")
	if err := os.MkdirAll(taskDir, 0o700); err != nil {
		res.Error = fmt.Sprintf("creating task dir: %v", err)
		return res
	}

	// Fresh clone: the isolation boundary. Uses the seat-holder's own git
	// credentials (ssh agent / credential helper), same as any manual clone.
	cloneArgs := []string{"clone", "--single-branch"}
	if task.BaseBranch != "" {
		cloneArgs = append(cloneArgs, "--branch", task.BaseBranch)
	}
	cloneArgs = append(cloneArgs, task.RepoURL, repoDir)
	if out, err := gitRun(ctx, "", cloneArgs...); err != nil {
		res.Error = fmt.Sprintf("clone failed: %v: %s", err, tailStr(out, 500))
		return res
	}
	if out, err := gitRun(ctx, repoDir, "checkout", "-b", branch); err != nil {
		res.Error = fmt.Sprintf("branch failed: %v: %s", err, tailStr(out, 500))
		return res
	}
	res.Branch = branch

	// Context file: handoff blob (if any) + unattended-run preamble.
	contextPath := filepath.Join(taskDir, "context.md")
	blob := workerPreamble
	if contextBlob != "" {
		blob += "\n---\n\n" + contextBlob
	}
	if err := os.WriteFile(contextPath, []byte(blob), 0o600); err != nil {
		res.Error = fmt.Sprintf("writing context: %v", err)
		return res
	}

	runCtx, cancelRun := context.WithTimeout(ctx, time.Duration(p.TaskTimeoutMinutes)*time.Minute)
	defer cancelRun()

	args := []string{
		"-p", task.Prompt,
		"--output-format", "stream-json",
		"--verbose",
		"--permission-mode", "acceptEdits",
		"--append-system-prompt-file", contextPath,
	}
	if p.AllowedTools != "" {
		args = append(args, "--allowedTools", p.AllowedTools)
	}
	cmd := exec.CommandContext(runCtx, "claude", args...)
	cmd.Dir = repoDir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // kill the whole tree
	cmd.Cancel = func() error {
		// SIGTERM the process group first; CommandContext falls back to Kill.
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	}
	cmd.WaitDelay = 10 * time.Second

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		res.Error = fmt.Sprintf("stdout pipe: %v", err)
		return res
	}
	cmd.Stderr = nil

	if err := cmd.Start(); err != nil {
		res.Error = fmt.Sprintf("starting claude: %v (is Claude Code installed?)", err)
		return res
	}

	mon := monitorRun(runCtx, stdout, task, p, lease, cancelRun)
	waitErr := cmd.Wait()

	res.SessionID = mon.sessionID
	res.TokensUsed = mon.tokens
	res.NumTurns = mon.turns
	res.LogTail = mon.logTail()

	switch {
	case mon.budgetExceeded:
		res.Error = fmt.Sprintf("budget exceeded: %d weighted tokens (cap %d)", mon.tokens, task.MaxTokens)
	case mon.turnsExceeded:
		res.Error = fmt.Sprintf("turn cap exceeded: %d turns (cap %d)", mon.turns, task.MaxTurns)
	case mon.canceled:
		res.Error = "canceled remotely"
	case runCtx.Err() == context.DeadlineExceeded:
		res.Error = fmt.Sprintf("timed out after %dm", p.TaskTimeoutMinutes)
	case waitErr != nil:
		res.Error = fmt.Sprintf("claude exited: %v: %s", waitErr, tailStr(mon.logTail(), 500))
	case mon.resultErr != "":
		res.Error = "run reported error: " + mon.resultErr
	}

	// Even a failed/cut-off run may hold useful work — commit and push what
	// exists so nothing done under the seat-holder's account is invisible.
	pushed := pushWork(ctx, repoDir, branch, task, &res)

	if res.Error == "" {
		res.Status = fleet.StatusDone
		_ = os.RemoveAll(taskDir) // keep failures around for debugging
	} else if !pushed {
		res.Branch = ""
	}
	return res
}

// pushWork commits any dirty state and pushes the branch if it has commits
// beyond the base. Returns whether a branch was actually published.
func pushWork(ctx context.Context, repoDir, branch string, task fleet.Task, res *RunResult) bool {
	if out, _ := gitRun(ctx, repoDir, "status", "--porcelain"); strings.TrimSpace(string(out)) != "" {
		gitRun(ctx, repoDir, "add", "-A")
		gitRun(ctx, repoDir, "commit", "-m", fmt.Sprintf("fleet task %s: %s (autocommit)", shortID(task.ID), task.Title))
	}
	// Anything to publish?
	out, err := gitRun(ctx, repoDir, "rev-list", "--count", "HEAD", "^@{u}")
	if err != nil {
		// No upstream (fresh branch): check against the clone's default head.
		out, err = gitRun(ctx, repoDir, "rev-list", "--count", "HEAD", "^origin/HEAD")
	}
	if err == nil && strings.TrimSpace(string(out)) == "0" {
		return false
	}
	if pout, perr := gitRun(ctx, repoDir, "push", "-u", "origin", branch); perr != nil {
		if res.Error == "" {
			res.Error = fmt.Sprintf("push failed: %v: %s", perr, tailStr(pout, 500))
		}
		return false
	}
	return true
}

func gitRun(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func tailStr(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
