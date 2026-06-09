package fleet

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

const adminToken = "test-admin-token"

func newTestServer(t *testing.T) (*httptest.Server, *Store) {
	t.Helper()
	store, err := OpenStore(filepath.Join(t.TempDir(), "fleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	srv, err := NewServer(Config{AdminToken: adminToken}, store)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, store
}

func request(t *testing.T, method, url, token string, body any, out any) int {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req, err := http.NewRequest(method, url, &buf)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil && resp.StatusCode < 300 && resp.StatusCode != http.StatusNoContent {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatalf("%s %s: decoding response: %v", method, url, err)
		}
	}
	return resp.StatusCode
}

func provisionUser(t *testing.T, ts *httptest.Server, email string, budget int64, isAdmin bool) string {
	t.Helper()
	var resp struct {
		APIKey string `json:"api_key"`
	}
	code := request(t, "POST", ts.URL+"/v1/admin/users", adminToken,
		map[string]any{"email": email, "weekly_budget_tokens": budget, "is_admin": isAdmin}, &resp)
	if code != http.StatusCreated {
		t.Fatalf("provisioning %s: HTTP %d", email, code)
	}
	if resp.APIKey == "" {
		t.Fatal("no api key returned")
	}
	return resp.APIKey
}

func TestAuthRequired(t *testing.T) {
	ts, _ := newTestServer(t)
	if code := request(t, "GET", ts.URL+"/v1/capacity", "", nil, nil); code != http.StatusUnauthorized {
		t.Errorf("no token: got %d, want 401", code)
	}
	if code := request(t, "GET", ts.URL+"/v1/capacity", "bogus", nil, nil); code != http.StatusUnauthorized {
		t.Errorf("bad token: got %d, want 401", code)
	}
	if code := request(t, "POST", ts.URL+"/v1/admin/users", "bogus", map[string]any{"email": "x@y.z"}, nil); code != http.StatusUnauthorized {
		t.Errorf("bad admin token: got %d, want 401", code)
	}
}

func TestHeartbeatAndCapacity(t *testing.T) {
	ts, _ := newTestServer(t)
	key := provisionUser(t, ts, "tim@nine.ch", 10_000_000, false)

	today := time.Now().Format("2006-01-02")
	hb := Heartbeat{
		Hostname: "tims-mbp",
		UsageDays: []UsageDay{
			{Day: today, InputTokens: 1000, OutputTokens: 2000, CacheReadTokens: 100_000, APICalls: 10, Sessions: 2},
		},
		Worker: WorkerStatus{Enabled: true, State: "idle"},
	}
	if code := request(t, "POST", ts.URL+"/v1/heartbeat", key, hb, nil); code != http.StatusNoContent {
		t.Fatalf("heartbeat: HTTP %d", code)
	}
	// Idempotent: same day again must not double-count.
	if code := request(t, "POST", ts.URL+"/v1/heartbeat", key, hb, nil); code != http.StatusNoContent {
		t.Fatalf("second heartbeat: HTTP %d", code)
	}

	var caps []SeatCapacity
	if code := request(t, "GET", ts.URL+"/v1/capacity", key, nil, &caps); code != http.StatusOK {
		t.Fatalf("capacity: HTTP %d", code)
	}
	if len(caps) != 1 {
		t.Fatalf("expected 1 seat, got %d", len(caps))
	}
	c := caps[0]
	// weighted = 1000 + 2000 + 0 + 100000/10 = 13000
	if c.Tokens7d != 13000 {
		t.Errorf("Tokens7d = %d, want 13000", c.Tokens7d)
	}
	if c.AvailableTokens != 10_000_000-13000 {
		t.Errorf("AvailableTokens = %d, want %d", c.AvailableTokens, 10_000_000-13000)
	}
	if c.WorkerState != "idle" {
		t.Errorf("WorkerState = %q, want idle", c.WorkerState)
	}
}

func TestTaskLifecycle(t *testing.T) {
	ts, _ := newTestServer(t)
	enqKey := provisionUser(t, ts, "alice@nine.ch", 0, false)
	wrkKey := provisionUser(t, ts, "bob@nine.ch", 0, false)

	var task Task
	code := request(t, "POST", ts.URL+"/v1/tasks", enqKey, EnqueueRequest{
		Title:     "fix flaky test",
		Prompt:    "Fix the flaky TestFoo in pkg/foo.",
		RepoURL:   "https://github.com/acme/widget",
		MaxTokens: 100_000,
	}, &task)
	if code != http.StatusCreated {
		t.Fatalf("enqueue: HTTP %d", code)
	}
	if task.Status != StatusQueued {
		t.Fatalf("status = %q, want queued", task.Status)
	}

	// Claim with a non-matching repo allowlist → nothing claimable.
	var claimed Task
	code = request(t, "POST", ts.URL+"/v1/tasks/claim", wrkKey, ClaimRequest{
		Hostname: "bobs-mbp", ReposAllowed: []string{"https://github.com/other/"},
	}, &claimed)
	if code != http.StatusNoContent {
		t.Fatalf("claim with wrong allowlist: HTTP %d, want 204", code)
	}

	// Claim with matching prefix.
	code = request(t, "POST", ts.URL+"/v1/tasks/claim", wrkKey, ClaimRequest{
		Hostname: "bobs-mbp", ReposAllowed: []string{"https://github.com/acme/"},
	}, &claimed)
	if code != http.StatusOK {
		t.Fatalf("claim: HTTP %d", code)
	}
	if claimed.ID != task.ID || claimed.Status != StatusClaimed {
		t.Fatalf("claimed = %+v", claimed)
	}
	if claimed.Prompt == "" {
		t.Error("claimed task must include the prompt")
	}

	// Lease renewal moves it to running and reports no cancel.
	var lease LeaseResponse
	code = request(t, "POST", ts.URL+"/v1/tasks/"+task.ID+"/lease", wrkKey,
		LeaseRequest{TokensUsed: 5000, State: "running"}, &lease)
	if code != http.StatusOK || lease.Cancel {
		t.Fatalf("lease: HTTP %d cancel=%v", code, lease.Cancel)
	}

	// Enqueuer requests cancel; next lease renewal sees it.
	code = request(t, "POST", ts.URL+"/v1/tasks/"+task.ID+"/cancel", enqKey, nil, nil)
	if code != http.StatusOK {
		t.Fatalf("cancel: HTTP %d", code)
	}
	code = request(t, "POST", ts.URL+"/v1/tasks/"+task.ID+"/lease", wrkKey,
		LeaseRequest{TokensUsed: 6000, State: "running"}, &lease)
	if code != http.StatusOK || !lease.Cancel {
		t.Fatalf("lease after cancel: HTTP %d cancel=%v, want cancel=true", code, lease.Cancel)
	}

	// Worker reports failure due to cancel.
	code = request(t, "POST", ts.URL+"/v1/tasks/"+task.ID+"/result", wrkKey,
		ResultRequest{Status: "failed", Error: "canceled by enqueuer", TokensUsed: 6000}, nil)
	if code != http.StatusNoContent {
		t.Fatalf("result: HTTP %d", code)
	}

	var detail struct {
		Task   Task        `json:"task"`
		Events []TaskEvent `json:"events"`
	}
	code = request(t, "GET", ts.URL+"/v1/tasks/"+task.ID, enqKey, nil, &detail)
	if code != http.StatusOK {
		t.Fatalf("get task: HTTP %d", code)
	}
	if detail.Task.Status != StatusFailed {
		t.Errorf("final status = %q, want failed", detail.Task.Status)
	}
	if len(detail.Events) < 4 { // enqueued, claimed, cancel_requested, failed
		t.Errorf("expected >= 4 events, got %d: %+v", len(detail.Events), detail.Events)
	}
}

func TestConcurrentClaimSingleWinner(t *testing.T) {
	ts, _ := newTestServer(t)
	enqKey := provisionUser(t, ts, "alice@nine.ch", 0, false)

	var task Task
	request(t, "POST", ts.URL+"/v1/tasks", enqKey, EnqueueRequest{
		Prompt: "p", RepoURL: "https://github.com/acme/x", MaxTokens: 1000,
	}, &task)

	const n = 8
	keys := make([]string, n)
	for i := range keys {
		keys[i] = provisionUser(t, ts, fmt.Sprintf("w%d@nine.ch", i), 0, false)
	}

	var wg sync.WaitGroup
	wins := make(chan string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(key string) {
			defer wg.Done()
			var claimed Task
			code := request(t, "POST", ts.URL+"/v1/tasks/claim", key,
				ClaimRequest{Hostname: "h"}, &claimed)
			if code == http.StatusOK {
				wins <- claimed.ID
			}
		}(keys[i])
	}
	wg.Wait()
	close(wins)

	var winners int
	for range wins {
		winners++
	}
	if winners != 1 {
		t.Errorf("expected exactly 1 winner, got %d", winners)
	}
}

func TestSweepRequeuesAndFails(t *testing.T) {
	ts, store := newTestServer(t)
	enqKey := provisionUser(t, ts, "alice@nine.ch", 0, false)
	wrkKey := provisionUser(t, ts, "bob@nine.ch", 0, false)

	var task Task
	request(t, "POST", ts.URL+"/v1/tasks", enqKey, EnqueueRequest{
		Prompt: "p", RepoURL: "https://github.com/acme/x", MaxTokens: 1000,
	}, &task)

	// Claim with a 1-second lease, let it expire, sweep → requeued (attempt 1 of 2).
	var claimed Task
	request(t, "POST", ts.URL+"/v1/tasks/claim", wrkKey, ClaimRequest{Hostname: "h", LeaseSeconds: 1}, &claimed)
	time.Sleep(1100 * time.Millisecond)
	requeued, _, failed, err := store.Sweep()
	if err != nil {
		t.Fatal(err)
	}
	if requeued != 1 || failed != 0 {
		t.Fatalf("first sweep: requeued=%d failed=%d, want 1/0", requeued, failed)
	}

	// Second claim + expiry exhausts max_attempts → failed.
	request(t, "POST", ts.URL+"/v1/tasks/claim", wrkKey, ClaimRequest{Hostname: "h", LeaseSeconds: 1}, &claimed)
	time.Sleep(1100 * time.Millisecond)
	requeued, _, failed, err = store.Sweep()
	if err != nil {
		t.Fatal(err)
	}
	if requeued != 0 || failed != 1 {
		t.Fatalf("second sweep: requeued=%d failed=%d, want 0/1", requeued, failed)
	}

	got, _ := store.GetTask(task.ID)
	if got.Status != StatusFailed || got.Error != "worker lost" {
		t.Errorf("task = %s/%q, want failed/worker lost", got.Status, got.Error)
	}
}

func TestDisabledUserIsKillSwitch(t *testing.T) {
	ts, _ := newTestServer(t)
	key := provisionUser(t, ts, "mallory@nine.ch", 0, false)

	if code := request(t, "GET", ts.URL+"/v1/capacity", key, nil, nil); code != http.StatusOK {
		t.Fatalf("before disable: HTTP %d", code)
	}
	disabled := true
	code := request(t, "PATCH", ts.URL+"/v1/admin/users/mallory@nine.ch", adminToken,
		map[string]any{"disabled": &disabled}, nil)
	if code != http.StatusOK {
		t.Fatalf("disable: HTTP %d", code)
	}
	if code := request(t, "GET", ts.URL+"/v1/capacity", key, nil, nil); code != http.StatusUnauthorized {
		t.Errorf("after disable: HTTP %d, want 401", code)
	}
}

func TestUsageReport(t *testing.T) {
	ts, _ := newTestServer(t)
	adminKey := provisionUser(t, ts, "admin@nine.ch", 0, true)
	userKey := provisionUser(t, ts, "tim@nine.ch", 7_000_000, false)

	today := time.Now().Format("2006-01-02")
	request(t, "POST", ts.URL+"/v1/heartbeat", userKey, Heartbeat{
		Hostname:  "h",
		UsageDays: []UsageDay{{Day: today, InputTokens: 500_000, OutputTokens: 500_000, Sessions: 3}},
	}, nil)

	// Non-admin is rejected.
	if code := request(t, "GET", ts.URL+"/v1/usage/report", userKey, nil, nil); code != http.StatusForbidden {
		t.Errorf("non-admin report: HTTP %d, want 403", code)
	}

	var rep struct {
		Users []UserUsageReport `json:"users"`
	}
	q := fmt.Sprintf("?since=%s&until=%s", today, today)
	if code := request(t, "GET", ts.URL+"/v1/usage/report"+q, adminKey, nil, &rep); code != http.StatusOK {
		t.Fatalf("report: HTTP %d", code)
	}
	if len(rep.Users) != 2 {
		t.Fatalf("expected 2 users, got %d", len(rep.Users))
	}
	top := rep.Users[0]
	if top.Email != "tim@nine.ch" || top.WeightedTokens != 1_000_000 {
		t.Errorf("top user = %s/%d, want tim@nine.ch/1000000", top.Email, top.WeightedTokens)
	}
	// 1 day of a 7M weekly budget = 1M pro-rated; 1M used → 100%.
	if top.UtilizationPct != 100 {
		t.Errorf("utilization = %d%%, want 100", top.UtilizationPct)
	}
}
