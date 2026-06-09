// Package fleetclient is the CLI-side HTTP client for the fleet coordinator.
package fleetclient

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/timae/ses/internal/fleet"
)

type Config struct {
	URL string `json:"url"`
	Key string `json:"key"`
}

func ConfigPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".ses", "fleet.json")
}

func LoadConfig() (Config, error) {
	var cfg Config
	data, err := os.ReadFile(ConfigPath())
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, fmt.Errorf("no fleet config — run `ses fleet login --url <base> --key <api-key>` first")
		}
		return cfg, err
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("corrupt fleet config at %s: %w", ConfigPath(), err)
	}
	return cfg, nil
}

func SaveConfig(cfg Config) error {
	path := ConfigPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(cfg, "", "  ")
	return os.WriteFile(path, data, 0o600)
}

// Configured reports whether a fleet config exists, without erroring — used
// by opportunistic callers (watch daemon heartbeat) that no-op when the seat
// isn't enrolled.
func Configured() bool {
	_, err := os.Stat(ConfigPath())
	return err == nil
}

type Client struct {
	cfg  Config
	http *http.Client
}

func New(cfg Config) *Client {
	return &Client{
		cfg:  Config{URL: strings.TrimRight(cfg.URL, "/"), Key: cfg.Key},
		http: &http.Client{Timeout: 30 * time.Second},
	}
}

// Load returns a client from the saved ~/.ses/fleet.json.
func Load() (*Client, error) {
	cfg, err := LoadConfig()
	if err != nil {
		return nil, err
	}
	return New(cfg), nil
}

func (c *Client) do(method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, c.cfg.URL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.Key)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNoContent {
		return nil
	}
	if resp.StatusCode >= 400 {
		var e struct {
			Error string `json:"error"`
		}
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			return fmt.Errorf("%s %s: %s (HTTP %d)", method, path, e.Error, resp.StatusCode)
		}
		return fmt.Errorf("%s %s: HTTP %d", method, path, resp.StatusCode)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func (c *Client) Heartbeat(hb fleet.Heartbeat) error {
	return c.do("POST", "/v1/heartbeat", hb, nil)
}

func (c *Client) Capacity() ([]fleet.SeatCapacity, error) {
	var out []fleet.SeatCapacity
	err := c.do("GET", "/v1/capacity", nil, &out)
	return out, err
}

func (c *Client) Enqueue(req fleet.EnqueueRequest) (fleet.Task, error) {
	var out fleet.Task
	err := c.do("POST", "/v1/tasks", req, &out)
	return out, err
}

func (c *Client) ListTasks(status string, mine bool) ([]fleet.Task, error) {
	q := "?status=" + status
	if mine {
		q += "&mine=1"
	}
	var out []fleet.Task
	err := c.do("GET", "/v1/tasks"+q, nil, &out)
	return out, err
}

type TaskDetail struct {
	Task   fleet.Task        `json:"task"`
	Events []fleet.TaskEvent `json:"events"`
}

func (c *Client) GetTask(id string) (TaskDetail, error) {
	var out TaskDetail
	err := c.do("GET", "/v1/tasks/"+id, nil, &out)
	return out, err
}

// Claim returns (nil, nil) when nothing is claimable.
func (c *Client) Claim(req fleet.ClaimRequest) (*fleet.Task, error) {
	data, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequest("POST", c.cfg.URL+"/v1/tasks/claim", bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.cfg.Key)
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("claim: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var t fleet.Task
	if err := json.NewDecoder(resp.Body).Decode(&t); err != nil {
		return nil, err
	}
	return &t, nil
}

func (c *Client) RenewLease(taskID string, req fleet.LeaseRequest) (fleet.LeaseResponse, error) {
	var out fleet.LeaseResponse
	err := c.do("POST", "/v1/tasks/"+taskID+"/lease", req, &out)
	return out, err
}

func (c *Client) ReportResult(taskID string, req fleet.ResultRequest) error {
	return c.do("POST", "/v1/tasks/"+taskID+"/result", req, nil)
}

func (c *Client) Cancel(taskID string) (fleet.Task, error) {
	var out fleet.Task
	err := c.do("POST", "/v1/tasks/"+taskID+"/cancel", nil, &out)
	return out, err
}

type UsageReport struct {
	Since string                  `json:"since"`
	Until string                  `json:"until"`
	Users []fleet.UserUsageReport `json:"users"`
}

func (c *Client) Report(since, until string) (UsageReport, error) {
	q := ""
	if since != "" || until != "" {
		q = "?since=" + since + "&until=" + until
	}
	var out UsageReport
	err := c.do("GET", "/v1/usage/report"+q, nil, &out)
	return out, err
}
