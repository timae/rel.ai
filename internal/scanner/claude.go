package scanner

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/timae/ses/internal/model"
)

type ClaudeScanner struct {
	Home        string // ~/.claude
	sessionMeta map[string]claudeSessionMeta
	historyMap  map[string]claudeHistoryEntry
}

func NewClaudeScanner(home string) *ClaudeScanner {
	return &ClaudeScanner{
		Home:        home,
		sessionMeta: make(map[string]claudeSessionMeta),
		historyMap:  make(map[string]claudeHistoryEntry),
	}
}

// claudeSessionMeta from sessions/{PID}.json
type claudeSessionMeta struct {
	PID       int    `json:"pid"`
	SessionID string `json:"sessionId"`
	CWD       string `json:"cwd"`
	StartedAt int64  `json:"startedAt"` // ms epoch
	Kind      string `json:"kind"`
	Name      string `json:"name"`
}

// claudeHistoryEntry from history.jsonl
type claudeHistoryEntry struct {
	Display   string `json:"display"`
	Timestamp int64  `json:"timestamp"` // ms epoch
	Project   string `json:"project"`
	SessionID string `json:"sessionId"`
}

// claudeTranscriptLine from projects/*/sessionId.jsonl
type claudeTranscriptLine struct {
	Type      string             `json:"type"` // "user", "assistant", "file-history-snapshot", "progress"
	UUID      string             `json:"uuid"`
	SessionID string             `json:"sessionId"`
	Timestamp string             `json:"timestamp"`
	CWD       string             `json:"cwd"`
	GitBranch string             `json:"gitBranch"`
	Version   string             `json:"version"`
	Message   claudeMessage      `json:"message"`
}

type claudeMessage struct {
	ID      string            `json:"id"`
	Role    string            `json:"role"`
	Model   string            `json:"model"`
	Usage   *claudeUsage      `json:"usage"`
	Content json.RawMessage   `json:"content"`
}

type claudeUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
}

type claudeContentBlock struct {
	Type      string          `json:"type"` // "text", "tool_use", "tool_result", "thinking"
	Text      string          `json:"text"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ID        string          `json:"id"`           // present on tool_use
	ToolUseID string          `json:"tool_use_id"`  // present on tool_result, pairs with tool_use.ID
	Content   json.RawMessage `json:"content"`      // tool_result body: string or [{type,text}]
	IsError   bool            `json:"is_error"`
}

// maxToolOutputBytes caps what we store per tool_result in the DB. Raw
// transcripts on disk still have the full content; this cap just keeps the
// index lean — ses is for eyeball retrieval, not a full replay store.
const maxToolOutputBytes = 256 * 1024

func (s *ClaudeScanner) Discover() ([]SessionFile, error) {
	// Build session metadata map from sessions/*.json
	sessionMeta := make(map[string]claudeSessionMeta)
	metaFiles, _ := filepath.Glob(filepath.Join(s.Home, "sessions", "*.json"))
	for _, f := range metaFiles {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var m claudeSessionMeta
		if json.Unmarshal(data, &m) == nil && m.SessionID != "" {
			sessionMeta[m.SessionID] = m
		}
	}

	// Build history map
	historyMap := make(map[string]claudeHistoryEntry)
	histFile := filepath.Join(s.Home, "history.jsonl")
	if f, err := os.Open(histFile); err == nil {
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1024*1024), 1024*1024)
		for sc.Scan() {
			var e claudeHistoryEntry
			if json.Unmarshal(sc.Bytes(), &e) == nil && e.SessionID != "" {
				historyMap[e.SessionID] = e
			}
		}
		f.Close()
	}

	// Find transcript files
	projectDirs, _ := filepath.Glob(filepath.Join(s.Home, "projects", "*"))
	var files []SessionFile

	for _, dir := range projectDirs {
		transcripts, _ := filepath.Glob(filepath.Join(dir, "*.jsonl"))
		for _, tp := range transcripts {
			sessionID := strings.TrimSuffix(filepath.Base(tp), ".jsonl")
			info, err := os.Stat(tp)
			if err != nil {
				continue
			}

			files = append(files, SessionFile{
				TranscriptPath: tp,
				SourceType:     model.SourceClaude,
				SourceID:       sessionID,
				Mtime:          info.ModTime().Unix(),
				Size:           info.Size(),
			})
		}
	}

	// Store maps for use during Parse
	s.sessionMeta = sessionMeta
	s.historyMap = historyMap

	return files, nil
}

var _ Scanner = (*ClaudeScanner)(nil)

func (s *ClaudeScanner) Parse(sf SessionFile) (*model.Session, []model.Message, error) {

	f, err := os.Open(sf.TranscriptPath)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()

	session := &model.Session{
		ShortID:         ShortID(model.SourceClaude, sf.SourceID),
		SourceType:      model.SourceClaude,
		SourceID:        sf.SourceID,
		TranscriptPath:  sf.TranscriptPath,
		TranscriptMtime: time.Unix(sf.Mtime, 0),
		TranscriptSize:  sf.Size,
	}

	// Apply metadata from sessions/*.json
	if meta, ok := s.sessionMeta[sf.SourceID]; ok {
		session.PID = meta.PID
		session.CWD = meta.CWD
		session.StartedAt = time.UnixMilli(meta.StartedAt)
	}

	// Apply project path from history
	if hist, ok := s.historyMap[sf.SourceID]; ok {
		session.Project = hist.Project
	}

	// If no project from history, derive from directory name
	if session.Project == "" {
		dirName := filepath.Base(filepath.Dir(sf.TranscriptPath))
		session.Project = decodeProjectPath(dirName)
	}

	var messages []model.Message
	filesMap := make(map[string]string) // path -> action
	toolUses := make(map[string]toolCallInfo) // tool_use.id -> info, for pairing with tool_result
	usage := newUsageAccumulator()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 2*1024*1024), 2*1024*1024)

	for sc.Scan() {
		var line claudeTranscriptLine
		if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
			continue
		}

		switch line.Type {
		case "user":
			// Extract user text + any tool_result blocks. The API encodes tool
			// outputs as user-role messages with tool_result content blocks.
			text, results := extractClaudeUserContent(line.Message.Content, toolUses)
			if text != "" {
				session.UserPrompts = append(session.UserPrompts, text)
				session.MessageCount++
				messages = append(messages, model.Message{
					Role:    "user",
					Content: text,
				})
				if session.FirstPrompt == "" {
					session.FirstPrompt = text
				}
			}
			for _, r := range results {
				messages = append(messages, model.Message{
					Role:     "tool_result",
					Content:  r.Content,
					ToolName: r.Name,
					FilePath: r.FilePath,
				})
			}

			// Capture metadata from first user message
			if line.CWD != "" && session.CWD == "" {
				session.CWD = line.CWD
			}
			if line.GitBranch != "" {
				session.GitBranch = line.GitBranch
			}
			if line.Timestamp != "" {
				if t, err := time.Parse(time.RFC3339Nano, line.Timestamp); err == nil {
					if session.StartedAt.IsZero() {
						session.StartedAt = t
					}
					session.EndedAt = t
				}
			}

		case "assistant":
			session.MessageCount++
			text, toolCalls := extractClaudeAssistantContent(line.Message.Content)

			if text != "" {
				session.LastAssistant = text
				messages = append(messages, model.Message{
					Role:    "assistant",
					Content: text,
				})
			}

			// Track model (skip synthetic/internal markers)
			if line.Message.Model != "" && !strings.HasPrefix(line.Message.Model, "<") {
				session.Model = line.Message.Model
			}

			if line.Message.Usage != nil {
				day := ""
				if t, err := time.Parse(time.RFC3339Nano, line.Timestamp); err == nil {
					day = t.Local().Format("2006-01-02")
				} else if !session.StartedAt.IsZero() {
					day = session.StartedAt.Local().Format("2006-01-02")
				}
				usage.Add(line.Message.ID, line.Message.Model, day, line.Message.Usage)
			}

			// Process tool calls
			for _, tc := range toolCalls {
				session.ToolCallCount++
				messages = append(messages, model.Message{
					Role:     "tool_use",
					ToolName: tc.Name,
					FilePath: tc.FilePath,
				})
				if tc.ID != "" {
					toolUses[tc.ID] = tc
				}
				if tc.FilePath != "" {
					action := "read"
					switch tc.Name {
					case "Write":
						action = "write"
					case "Edit":
						action = "edit"
					}
					filesMap[tc.FilePath] = action
				}
			}
		}
	}

	// Populate files
	for path, action := range filesMap {
		session.Files = append(session.Files, model.SessionFile{
			FilePath: path,
			Action:   action,
		})
	}

	if session.CWD == "" {
		session.CWD = session.Project
	}

	usage.ApplyTo(session)

	return session, messages, nil
}

type toolCallInfo struct {
	ID       string
	Name     string
	FilePath string
}

type toolResultInfo struct {
	Name     string
	FilePath string
	Content  string
}

func extractClaudeAssistantContent(raw json.RawMessage) (text string, tools []toolCallInfo) {
	var blocks []claudeContentBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return "", nil
	}

	for _, b := range blocks {
		switch b.Type {
		case "text":
			if b.Text != "" {
				text = b.Text
			}
		case "tool_use":
			tc := toolCallInfo{ID: b.ID, Name: b.Name}
			// Try to extract file_path from input
			var input map[string]any
			if json.Unmarshal(b.Input, &input) == nil {
				if fp, ok := input["file_path"].(string); ok {
					tc.FilePath = fp
				}
			}
			tools = append(tools, tc)
		}
	}
	return text, tools
}

// extractClaudeUserContent pulls the user's typed text out of a user-role
// message and also returns any tool_result blocks in it. Tool results are
// paired back to the originating tool_use via tool_use_id so we carry the
// tool name and file path forward.
func extractClaudeUserContent(raw json.RawMessage, toolUses map[string]toolCallInfo) (text string, results []toolResultInfo) {
	// A user message can be a bare string (typed prompt) or an array of
	// content blocks (tool results live in the array form).
	var str string
	if json.Unmarshal(raw, &str) == nil {
		return str, nil
	}

	var blocks []claudeContentBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return "", nil
	}

	for _, b := range blocks {
		switch b.Type {
		case "text":
			if b.Text != "" && text == "" {
				text = b.Text
			}
		case "tool_result":
			body := flattenToolResultContent(b.Content)
			if body == "" {
				continue
			}
			if len(body) > maxToolOutputBytes {
				body = body[:maxToolOutputBytes] + "\n…[truncated by ses]"
			}
			r := toolResultInfo{Content: body}
			if info, ok := toolUses[b.ToolUseID]; ok {
				r.Name = info.Name
				r.FilePath = info.FilePath
			}
			results = append(results, r)
		}
	}
	return text, results
}

// flattenToolResultContent handles the two shapes the API uses for a
// tool_result's `content` field: a plain string, or an array of
// {type:"text", text:"..."} blocks.
func flattenToolResultContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var str string
	if json.Unmarshal(raw, &str) == nil {
		return str
	}
	var blocks []claudeContentBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var sb strings.Builder
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			if sb.Len() > 0 {
				sb.WriteByte('\n')
			}
			sb.WriteString(b.Text)
		}
	}
	return sb.String()
}

func decodeProjectPath(dirName string) string {
	// -Users-tim-Documents-Claude-foo -> /Users/tim/Documents/Claude/foo
	if !strings.HasPrefix(dirName, "-") {
		return dirName
	}
	return "/" + strings.ReplaceAll(dirName[1:], "-", "/")
}
