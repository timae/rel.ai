package fleet

import (
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"fmt"
	"strings"
	"time"
)

const (
	StatusQueued   = "queued"
	StatusClaimed  = "claimed"
	StatusRunning  = "running"
	StatusDone     = "done"
	StatusFailed   = "failed"
	StatusCanceled = "canceled"
	StatusExpired  = "expired"
)

type Task struct {
	ID              string `json:"id"`
	CreatedBy       string `json:"created_by"` // email
	Title           string `json:"title"`
	Prompt          string `json:"prompt,omitempty"`
	RepoURL         string `json:"repo_url"`
	BaseBranch      string `json:"base_branch"`
	HandoffURL      string `json:"handoff_url,omitempty"`
	MaxTokens       int64  `json:"max_tokens"`
	MaxTurns        int    `json:"max_turns"`
	Priority        int    `json:"priority"`
	Status          string `json:"status"`
	Attempts        int    `json:"attempts"`
	MaxAttempts     int    `json:"max_attempts"`
	CancelRequested bool   `json:"cancel_requested"`
	ClaimedBy       string `json:"claimed_by"` // email
	ClaimedHost     string `json:"claimed_host"`
	CreatedAt       string `json:"created_at"`
	ExpiresAt       string `json:"expires_at,omitempty"`
	StartedAt       string `json:"started_at,omitempty"`
	FinishedAt      string `json:"finished_at,omitempty"`
	ResultBranch    string `json:"result_branch,omitempty"`
	ResultShareURL  string `json:"result_share_url,omitempty"`
	TokensUsed      int64  `json:"tokens_used"`
	NumTurns        int    `json:"num_turns"`
	Error           string `json:"error,omitempty"`
}

type TaskEvent struct {
	At     string `json:"at"`
	Actor  string `json:"actor"`
	Event  string `json:"event"`
	Detail string `json:"detail,omitempty"`
}

func newTaskID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

type EnqueueRequest struct {
	Title            string `json:"title"`
	Prompt           string `json:"prompt"`
	RepoURL          string `json:"repo_url"`
	BaseBranch       string `json:"base_branch"`
	HandoffURL       string `json:"handoff_url"`
	MaxTokens        int64  `json:"max_tokens"`
	MaxTurns         int    `json:"max_turns"`
	Priority         int    `json:"priority"`
	ExpiresInSeconds int64  `json:"expires_in_seconds"`
}

func (s *Store) EnqueueTask(user User, req EnqueueRequest) (Task, error) {
	id, err := newTaskID()
	if err != nil {
		return Task{}, err
	}
	if req.MaxTurns <= 0 {
		req.MaxTurns = 30
	}
	if req.Priority <= 0 {
		req.Priority = 5
	}
	expiresAt := ""
	if req.ExpiresInSeconds > 0 {
		expiresAt = time.Now().UTC().Add(time.Duration(req.ExpiresInSeconds) * time.Second).Format(time.RFC3339)
	}
	_, err = s.db.Exec(`INSERT INTO tasks
		(id, created_by, title, prompt, repo_url, base_branch, handoff_url,
		 max_tokens, max_turns, priority, status, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'queued', ?, ?)`,
		id, user.ID, req.Title, req.Prompt, req.RepoURL, req.BaseBranch, req.HandoffURL,
		req.MaxTokens, req.MaxTurns, req.Priority, now(), nullable(expiresAt))
	if err != nil {
		return Task{}, err
	}
	s.addEvent(id, user.Email, "enqueued", "")
	return s.GetTask(id)
}

const taskColumns = `t.id, COALESCE(uc.email, ''), t.title, t.prompt, t.repo_url, t.base_branch, t.handoff_url,
	t.max_tokens, t.max_turns, t.priority, t.status, t.attempts, t.max_attempts, t.cancel_requested,
	COALESCE(uw.email, ''), t.claimed_host, t.created_at,
	COALESCE(t.expires_at, ''), COALESCE(t.started_at, ''), COALESCE(t.finished_at, ''),
	t.result_branch, t.result_share_url, t.tokens_used, t.num_turns, t.error`

const taskJoins = `FROM tasks t
	LEFT JOIN users uc ON uc.id = t.created_by
	LEFT JOIN users uw ON uw.id = t.claimed_by`

func scanTask(row interface{ Scan(...any) error }) (Task, error) {
	var t Task
	var cancel int
	err := row.Scan(&t.ID, &t.CreatedBy, &t.Title, &t.Prompt, &t.RepoURL, &t.BaseBranch, &t.HandoffURL,
		&t.MaxTokens, &t.MaxTurns, &t.Priority, &t.Status, &t.Attempts, &t.MaxAttempts, &cancel,
		&t.ClaimedBy, &t.ClaimedHost, &t.CreatedAt,
		&t.ExpiresAt, &t.StartedAt, &t.FinishedAt,
		&t.ResultBranch, &t.ResultShareURL, &t.TokensUsed, &t.NumTurns, &t.Error)
	t.CancelRequested = cancel == 1
	return t, err
}

func (s *Store) GetTask(id string) (Task, error) {
	t, err := scanTask(s.db.QueryRow(
		fmt.Sprintf("SELECT %s %s WHERE t.id = ?", taskColumns, taskJoins), id))
	if err == sql.ErrNoRows {
		return Task{}, ErrNotFound
	}
	return t, err
}

// ListTasks omits prompt bodies (they may contain sensitive text); GetTask
// returns the full record.
func (s *Store) ListTasks(status string, createdBy int64) ([]Task, error) {
	q := fmt.Sprintf("SELECT %s %s", taskColumns, taskJoins)
	var conds []string
	var args []any
	if status != "" {
		conds = append(conds, "t.status = ?")
		args = append(args, status)
	}
	if createdBy > 0 {
		conds = append(conds, "t.created_by = ?")
		args = append(args, createdBy)
	}
	if len(conds) > 0 {
		q += " WHERE " + strings.Join(conds, " AND ")
	}
	q += " ORDER BY t.created_at DESC LIMIT 200"

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		t.Prompt = ""
		out = append(out, t)
	}
	return out, rows.Err()
}

type ClaimRequest struct {
	Hostname     string   `json:"hostname"`
	ReposAllowed []string `json:"repos_allowed"` // prefix match; empty = allow all
	MaxTokens    int64    `json:"max_tokens"`    // claim nothing above this budget
	LeaseSeconds int64    `json:"lease_seconds"`
}

// ClaimTask picks the highest-priority queued task matching the worker's
// policy. Candidates are selected first, then claimed with a guarded UPDATE
// (status='queued' recheck), so concurrent claimers cannot double-claim;
// losers just move to the next candidate.
func (s *Store) ClaimTask(user User, req ClaimRequest) (Task, error) {
	if req.LeaseSeconds <= 0 {
		req.LeaseSeconds = 600
	}
	rows, err := s.db.Query(`SELECT id, repo_url FROM tasks
		WHERE status = 'queued' AND cancel_requested = 0
			AND (expires_at IS NULL OR expires_at > ?)
			AND (? <= 0 OR max_tokens <= ?)
		ORDER BY priority, created_at LIMIT 50`,
		now(), req.MaxTokens, req.MaxTokens)
	if err != nil {
		return Task{}, err
	}
	type cand struct{ id, repo string }
	var cands []cand
	for rows.Next() {
		var c cand
		if err := rows.Scan(&c.id, &c.repo); err != nil {
			rows.Close()
			return Task{}, err
		}
		cands = append(cands, c)
	}
	rows.Close()

	for _, c := range cands {
		if !repoAllowed(c.repo, req.ReposAllowed) {
			continue
		}
		lease := time.Now().UTC().Add(time.Duration(req.LeaseSeconds) * time.Second).Format(time.RFC3339)
		res, err := s.db.Exec(`UPDATE tasks SET status = 'claimed', claimed_by = ?, claimed_host = ?,
			attempts = attempts + 1, lease_expires_at = ?, started_at = COALESCE(started_at, ?)
			WHERE id = ? AND status = 'queued'`,
			user.ID, req.Hostname, lease, now(), c.id)
		if err != nil {
			return Task{}, err
		}
		if n, _ := res.RowsAffected(); n == 1 {
			s.addEvent(c.id, user.Email, "claimed", "by "+user.Email+"@"+req.Hostname)
			return s.GetTask(c.id)
		}
	}
	return Task{}, ErrNotFound // nothing claimable
}

func repoAllowed(repo string, allowed []string) bool {
	if len(allowed) == 0 {
		return true
	}
	for _, p := range allowed {
		if p != "" && strings.HasPrefix(repo, p) {
			return true
		}
	}
	return false
}

type LeaseRequest struct {
	TokensUsed   int64  `json:"tokens_used"`
	State        string `json:"state"` // claimed|running
	LeaseSeconds int64  `json:"lease_seconds"`
}

type LeaseResponse struct {
	Cancel bool `json:"cancel"`
}

// RenewLease extends the worker's hold on a task and is the channel through
// which a remote cancel reaches the worker.
func (s *Store) RenewLease(user User, taskID string, req LeaseRequest) (LeaseResponse, error) {
	if req.LeaseSeconds <= 0 {
		req.LeaseSeconds = 600
	}
	status := StatusClaimed
	if req.State == StatusRunning {
		status = StatusRunning
	}
	lease := time.Now().UTC().Add(time.Duration(req.LeaseSeconds) * time.Second).Format(time.RFC3339)
	res, err := s.db.Exec(`UPDATE tasks SET lease_expires_at = ?, tokens_used = ?, status = ?
		WHERE id = ? AND claimed_by = ? AND status IN ('claimed', 'running')`,
		lease, req.TokensUsed, status, taskID, user.ID)
	if err != nil {
		return LeaseResponse{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return LeaseResponse{}, ErrNotFound
	}
	var cancel int
	if err := s.db.QueryRow("SELECT cancel_requested FROM tasks WHERE id = ?", taskID).Scan(&cancel); err != nil {
		return LeaseResponse{}, err
	}
	return LeaseResponse{Cancel: cancel == 1}, nil
}

type ResultRequest struct {
	Status         string `json:"status"` // done|failed
	ResultBranch   string `json:"result_branch"`
	ResultShareURL string `json:"result_share_url"`
	TokensUsed     int64  `json:"tokens_used"`
	NumTurns       int    `json:"num_turns"`
	LogTail        string `json:"log_tail"`
	Error          string `json:"error"`
}

func (s *Store) ReportResult(user User, taskID string, req ResultRequest) error {
	if req.Status != StatusDone && req.Status != StatusFailed {
		return fmt.Errorf("invalid result status %q", req.Status)
	}
	res, err := s.db.Exec(`UPDATE tasks SET status = ?, finished_at = ?, result_branch = ?,
		result_share_url = ?, tokens_used = ?, num_turns = ?, error = ?
		WHERE id = ? AND claimed_by = ? AND status IN ('claimed', 'running')`,
		req.Status, now(), req.ResultBranch, req.ResultShareURL,
		req.TokensUsed, req.NumTurns, req.Error, taskID, user.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	detail := req.Error
	if detail == "" {
		detail = req.ResultBranch
	}
	if req.LogTail != "" {
		detail += "\n" + tail(req.LogTail, 4000)
	}
	s.addEvent(taskID, user.Email, req.Status, detail)
	return nil
}

// CancelTask cancels a queued task immediately; for claimed/running tasks it
// sets cancel_requested, which the worker picks up at the next lease renewal.
// Only the enqueuer or an admin may cancel.
func (s *Store) CancelTask(user User, taskID string) (Task, error) {
	t, err := s.GetTask(taskID)
	if err != nil {
		return Task{}, err
	}
	if !user.IsAdmin && t.CreatedBy != user.Email {
		return Task{}, ErrUnauthorized
	}
	switch t.Status {
	case StatusQueued:
		_, err = s.db.Exec(`UPDATE tasks SET status = 'canceled', finished_at = ? WHERE id = ? AND status = 'queued'`,
			now(), taskID)
	case StatusClaimed, StatusRunning:
		_, err = s.db.Exec(`UPDATE tasks SET cancel_requested = 1 WHERE id = ?`, taskID)
	default:
		return t, fmt.Errorf("task is already %s", t.Status)
	}
	if err != nil {
		return Task{}, err
	}
	s.addEvent(taskID, user.Email, "cancel_requested", "")
	return s.GetTask(taskID)
}

// Sweep expires stale queued tasks and recovers tasks whose worker
// disappeared: lease-expired tasks requeue while attempts remain, otherwise
// they fail with "worker lost".
func (s *Store) Sweep() (requeued, expired, failed int, err error) {
	n := now()

	res, err := s.db.Exec(`UPDATE tasks SET status = 'expired', finished_at = ?
		WHERE status = 'queued' AND expires_at IS NOT NULL AND expires_at <= ?`, n, n)
	if err != nil {
		return 0, 0, 0, err
	}
	if c, _ := res.RowsAffected(); c > 0 {
		expired = int(c)
	}

	rows, err := s.db.Query(`SELECT id, attempts, max_attempts FROM tasks
		WHERE status IN ('claimed', 'running') AND lease_expires_at IS NOT NULL AND lease_expires_at <= ?`, n)
	if err != nil {
		return requeued, expired, failed, err
	}
	type stale struct {
		id                    string
		attempts, maxAttempts int
	}
	var stales []stale
	for rows.Next() {
		var st stale
		if err := rows.Scan(&st.id, &st.attempts, &st.maxAttempts); err != nil {
			rows.Close()
			return requeued, expired, failed, err
		}
		stales = append(stales, st)
	}
	rows.Close()

	for _, st := range stales {
		if st.attempts < st.maxAttempts {
			_, err = s.db.Exec(`UPDATE tasks SET status = 'queued', claimed_by = NULL, claimed_host = '',
				lease_expires_at = NULL WHERE id = ? AND status IN ('claimed', 'running')`, st.id)
			if err == nil {
				s.addEvent(st.id, "system", "requeued", "lease expired")
				requeued++
			}
		} else {
			_, err = s.db.Exec(`UPDATE tasks SET status = 'failed', finished_at = ?, error = 'worker lost'
				WHERE id = ? AND status IN ('claimed', 'running')`, n, st.id)
			if err == nil {
				s.addEvent(st.id, "system", "failed", "worker lost after max attempts")
				failed++
			}
		}
	}
	return requeued, expired, failed, nil
}

func (s *Store) TaskEvents(taskID string) ([]TaskEvent, error) {
	rows, err := s.db.Query("SELECT at, actor, event, detail FROM task_events WHERE task_id = ? ORDER BY id", taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TaskEvent
	for rows.Next() {
		var e TaskEvent
		if err := rows.Scan(&e.At, &e.Actor, &e.Event, &e.Detail); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) addEvent(taskID, actor, event, detail string) {
	s.db.Exec("INSERT INTO task_events (task_id, at, actor, event, detail) VALUES (?, ?, ?, ?, ?)",
		taskID, now(), actor, event, detail)
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
