package fleet

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
)

const maxBodyBytes = 1 << 20 // 1 MB; task prompts are text, not transcripts

type Config struct {
	// AdminToken guards /v1/admin/*. It is separate from user API keys so the
	// bootstrap path works before any user exists.
	AdminToken string
	Logger     *log.Logger
}

type Server struct {
	cfg   Config
	store *Store
}

func NewServer(cfg Config, store *Store) (*Server, error) {
	if cfg.AdminToken == "" {
		return nil, errors.New("AdminToken is required")
	}
	if cfg.Logger == nil {
		cfg.Logger = log.Default()
	}
	return &Server{cfg: cfg, store: store}, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/heartbeat", s.withUser(s.handleHeartbeat))
	mux.HandleFunc("GET /v1/capacity", s.withUser(s.handleCapacity))
	mux.HandleFunc("GET /v1/usage/report", s.withUser(s.handleUsageReport))

	mux.HandleFunc("POST /v1/tasks", s.withUser(s.handleEnqueue))
	mux.HandleFunc("GET /v1/tasks", s.withUser(s.handleListTasks))
	mux.HandleFunc("GET /v1/tasks/{id}", s.withUser(s.handleGetTask))
	mux.HandleFunc("POST /v1/tasks/claim", s.withUser(s.handleClaim))
	mux.HandleFunc("POST /v1/tasks/{id}/lease", s.withUser(s.handleLease))
	mux.HandleFunc("POST /v1/tasks/{id}/result", s.withUser(s.handleResult))
	mux.HandleFunc("POST /v1/tasks/{id}/cancel", s.withUser(s.handleCancel))

	mux.HandleFunc("POST /v1/admin/users", s.withAdmin(s.handleCreateUser))
	mux.HandleFunc("PATCH /v1/admin/users/{email}", s.withAdmin(s.handleUpdateUser))

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	return mux
}

// --- middleware -------------------------------------------------------------

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(h, prefix) {
		return ""
	}
	return h[len(prefix):]
}

func (s *Server) withUser(h func(http.ResponseWriter, *http.Request, User)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := bearerToken(r)
		if key == "" {
			s.replyError(w, http.StatusUnauthorized, "missing bearer token")
			return
		}
		u, err := s.store.AuthUser(key)
		if err != nil {
			if errors.Is(err, ErrUnauthorized) {
				s.replyError(w, http.StatusUnauthorized, "invalid or disabled api key")
				return
			}
			s.cfg.Logger.Printf("auth: %v", err)
			s.replyError(w, http.StatusInternalServerError, "auth error")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		h(w, r, u)
	}
}

func (s *Server) withAdmin(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		got := bearerToken(r)
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.cfg.AdminToken)) != 1 {
			s.replyError(w, http.StatusUnauthorized, "admin token required")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		h(w, r)
	}
}

// --- telemetry ----------------------------------------------------------------

func (s *Server) handleHeartbeat(w http.ResponseWriter, r *http.Request, u User) {
	var hb Heartbeat
	if err := json.NewDecoder(r.Body).Decode(&hb); err != nil {
		s.replyError(w, http.StatusBadRequest, "invalid json: %v", err)
		return
	}
	if hb.Hostname == "" {
		s.replyError(w, http.StatusBadRequest, "hostname is required")
		return
	}
	if err := s.store.RecordHeartbeat(u.ID, hb); err != nil {
		s.cfg.Logger.Printf("heartbeat %s: %v", u.Email, err)
		s.replyError(w, http.StatusInternalServerError, "storage error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleCapacity(w http.ResponseWriter, r *http.Request, _ User) {
	caps, err := s.store.Capacity()
	if err != nil {
		s.cfg.Logger.Printf("capacity: %v", err)
		s.replyError(w, http.StatusInternalServerError, "storage error")
		return
	}
	s.replyJSON(w, http.StatusOK, caps)
}

func (s *Server) handleUsageReport(w http.ResponseWriter, r *http.Request, u User) {
	if !u.IsAdmin {
		s.replyError(w, http.StatusForbidden, "admin only")
		return
	}
	since := r.URL.Query().Get("since")
	until := r.URL.Query().Get("until")
	if until == "" {
		until = time.Now().Format("2006-01-02")
	}
	if since == "" {
		since = time.Now().AddDate(0, 0, -27).Format("2006-01-02")
	}
	report, err := s.store.UsageReport(since, until)
	if err != nil {
		s.cfg.Logger.Printf("usage report: %v", err)
		s.replyError(w, http.StatusInternalServerError, "storage error")
		return
	}
	s.replyJSON(w, http.StatusOK, map[string]any{"since": since, "until": until, "users": report})
}

// --- tasks ----------------------------------------------------------------------

func (s *Server) handleEnqueue(w http.ResponseWriter, r *http.Request, u User) {
	var req EnqueueRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.replyError(w, http.StatusBadRequest, "invalid json: %v", err)
		return
	}
	if strings.TrimSpace(req.Prompt) == "" || strings.TrimSpace(req.RepoURL) == "" {
		s.replyError(w, http.StatusBadRequest, "prompt and repo_url are required")
		return
	}
	if req.MaxTokens <= 0 {
		s.replyError(w, http.StatusBadRequest, "max_tokens is required (the task's token budget)")
		return
	}
	t, err := s.store.EnqueueTask(u, req)
	if err != nil {
		s.cfg.Logger.Printf("enqueue: %v", err)
		s.replyError(w, http.StatusInternalServerError, "storage error")
		return
	}
	s.replyJSON(w, http.StatusCreated, t)
}

func (s *Server) handleListTasks(w http.ResponseWriter, r *http.Request, u User) {
	var createdBy int64
	if r.URL.Query().Get("mine") == "1" {
		createdBy = u.ID
	}
	tasks, err := s.store.ListTasks(r.URL.Query().Get("status"), createdBy)
	if err != nil {
		s.cfg.Logger.Printf("list tasks: %v", err)
		s.replyError(w, http.StatusInternalServerError, "storage error")
		return
	}
	s.replyJSON(w, http.StatusOK, tasks)
}

func (s *Server) handleGetTask(w http.ResponseWriter, r *http.Request, _ User) {
	t, err := s.store.GetTask(r.PathValue("id"))
	if errors.Is(err, ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.replyError(w, http.StatusInternalServerError, "storage error")
		return
	}
	events, _ := s.store.TaskEvents(t.ID)
	s.replyJSON(w, http.StatusOK, map[string]any{"task": t, "events": events})
}

func (s *Server) handleClaim(w http.ResponseWriter, r *http.Request, u User) {
	var req ClaimRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.replyError(w, http.StatusBadRequest, "invalid json: %v", err)
		return
	}
	if req.Hostname == "" {
		s.replyError(w, http.StatusBadRequest, "hostname is required")
		return
	}
	t, err := s.store.ClaimTask(u, req)
	if errors.Is(err, ErrNotFound) {
		w.WriteHeader(http.StatusNoContent) // nothing claimable right now
		return
	}
	if err != nil {
		s.cfg.Logger.Printf("claim: %v", err)
		s.replyError(w, http.StatusInternalServerError, "storage error")
		return
	}
	s.replyJSON(w, http.StatusOK, t)
}

func (s *Server) handleLease(w http.ResponseWriter, r *http.Request, u User) {
	var req LeaseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.replyError(w, http.StatusBadRequest, "invalid json: %v", err)
		return
	}
	resp, err := s.store.RenewLease(u, r.PathValue("id"), req)
	if errors.Is(err, ErrNotFound) {
		s.replyError(w, http.StatusConflict, "task is not held by you (lease lost?)")
		return
	}
	if err != nil {
		s.replyError(w, http.StatusInternalServerError, "storage error")
		return
	}
	s.replyJSON(w, http.StatusOK, resp)
}

func (s *Server) handleResult(w http.ResponseWriter, r *http.Request, u User) {
	var req ResultRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.replyError(w, http.StatusBadRequest, "invalid json: %v", err)
		return
	}
	err := s.store.ReportResult(u, r.PathValue("id"), req)
	if errors.Is(err, ErrNotFound) {
		s.replyError(w, http.StatusConflict, "task is not held by you")
		return
	}
	if err != nil {
		s.replyError(w, http.StatusBadRequest, "%v", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request, u User) {
	t, err := s.store.CancelTask(u, r.PathValue("id"))
	switch {
	case errors.Is(err, ErrNotFound):
		http.NotFound(w, r)
	case errors.Is(err, ErrUnauthorized):
		s.replyError(w, http.StatusForbidden, "only the enqueuer or an admin can cancel")
	case err != nil:
		s.replyError(w, http.StatusBadRequest, "%v", err)
	default:
		s.replyJSON(w, http.StatusOK, t)
	}
}

// --- admin -----------------------------------------------------------------------

type createUserRequest struct {
	Email              string `json:"email"`
	Name               string `json:"name"`
	WeeklyBudgetTokens int64  `json:"weekly_budget_tokens"`
	IsAdmin            bool   `json:"is_admin"`
}

func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	var req createUserRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.replyError(w, http.StatusBadRequest, "invalid json: %v", err)
		return
	}
	if !strings.Contains(req.Email, "@") {
		s.replyError(w, http.StatusBadRequest, "valid email is required")
		return
	}
	u, rawKey, err := s.store.CreateUser(req.Email, req.Name, req.WeeklyBudgetTokens, req.IsAdmin)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			s.replyError(w, http.StatusConflict, "user %s already exists", req.Email)
			return
		}
		s.cfg.Logger.Printf("create user: %v", err)
		s.replyError(w, http.StatusInternalServerError, "storage error")
		return
	}
	s.replyJSON(w, http.StatusCreated, map[string]any{
		"user":    u,
		"api_key": rawKey, // shown exactly once
	})
}

type updateUserRequest struct {
	WeeklyBudgetTokens *int64 `json:"weekly_budget_tokens"`
	Disabled           *bool  `json:"disabled"`
	RotateKey          bool   `json:"rotate_key"`
}

func (s *Server) handleUpdateUser(w http.ResponseWriter, r *http.Request) {
	var req updateUserRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.replyError(w, http.StatusBadRequest, "invalid json: %v", err)
		return
	}
	newKey, err := s.store.UpdateUser(r.PathValue("email"), req.WeeklyBudgetTokens, req.Disabled, req.RotateKey)
	if errors.Is(err, ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.cfg.Logger.Printf("update user: %v", err)
		s.replyError(w, http.StatusInternalServerError, "storage error")
		return
	}
	resp := map[string]any{"ok": true}
	if newKey != "" {
		resp["api_key"] = newKey
	}
	s.replyJSON(w, http.StatusOK, resp)
}

// --- helpers ------------------------------------------------------------------------

func (s *Server) replyJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) replyError(w http.ResponseWriter, code int, format string, args ...any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": fmt.Sprintf(format, args...)})
}
