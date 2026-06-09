package model

import "time"

type Source string

const (
	SourceClaude Source = "claude"
	SourceCodex  Source = "codex"
)

type Session struct {
	ID              int64
	ShortID         string
	SourceType      Source
	SourceID        string
	PID             int
	Project         string
	CWD             string
	GitBranch       string
	GitCommit       string
	StartedAt       time.Time
	EndedAt         time.Time
	MessageCount    int
	ToolCallCount   int
	FirstPrompt     string
	LastAssistant   string
	Model           string
	TranscriptPath  string
	ScannedAt       time.Time
	TranscriptMtime time.Time
	TranscriptSize  int64
	Tags            []string
	Files           []SessionFile
	UserPrompts     []string

	// Token telemetry, aggregated from per-message usage in the transcript.
	InputTokens         int64
	OutputTokens        int64
	CacheCreationTokens int64
	CacheReadTokens     int64
	APICallCount        int
	UsageDays           []UsageDay
}

// UsageDay is a per-day, per-model token rollup within one session. Long
// sessions span days; weekly-capacity math needs per-day attribution.
type UsageDay struct {
	Day                 string // "2006-01-02", local time
	Model               string
	InputTokens         int64
	OutputTokens        int64
	CacheCreationTokens int64
	CacheReadTokens     int64
	APICalls            int
}

type SessionFile struct {
	FilePath string
	Action   string // "read", "write", "edit"
}

type Message struct {
	Role      string
	Content   string
	Timestamp time.Time
	ToolName  string
	FilePath  string
}
