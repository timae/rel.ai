package scanner

import (
	"sort"
	"strings"

	"github.com/timae/ses/internal/model"
)

// usageAccumulator sums per-message token usage with message-id dedup.
//
// Claude Code writes one transcript line per content block of an assistant
// turn; all lines of the same turn share message.id and carry an identical
// usage object. Counting every line would inflate totals ~2-3x, so a usage
// object is counted only the first time its message id appears. Lines with
// an empty id and non-nil usage are counted individually as a fallback.
type usageAccumulator struct {
	seen  map[string]bool
	daily map[string]*model.UsageDay // key: day + "\x00" + model

	input, output, cacheCreation, cacheRead int64
	apiCalls                                int
}

func newUsageAccumulator() *usageAccumulator {
	return &usageAccumulator{
		seen:  make(map[string]bool),
		daily: make(map[string]*model.UsageDay),
	}
}

func (a *usageAccumulator) Add(msgID, modelName, day string, u *claudeUsage) {
	if u == nil {
		return
	}
	// Synthetic/internal messages carry all-zero usage; skip them entirely so
	// they don't show up as bogus model buckets.
	if u.InputTokens == 0 && u.OutputTokens == 0 &&
		u.CacheCreationInputTokens == 0 && u.CacheReadInputTokens == 0 {
		return
	}
	if strings.HasPrefix(modelName, "<") {
		modelName = ""
	}
	if msgID != "" {
		if a.seen[msgID] {
			return
		}
		a.seen[msgID] = true
	}

	a.input += u.InputTokens
	a.output += u.OutputTokens
	a.cacheCreation += u.CacheCreationInputTokens
	a.cacheRead += u.CacheReadInputTokens
	a.apiCalls++

	key := day + "\x00" + modelName
	d, ok := a.daily[key]
	if !ok {
		d = &model.UsageDay{Day: day, Model: modelName}
		a.daily[key] = d
	}
	d.InputTokens += u.InputTokens
	d.OutputTokens += u.OutputTokens
	d.CacheCreationTokens += u.CacheCreationInputTokens
	d.CacheReadTokens += u.CacheReadInputTokens
	d.APICalls++
}

func (a *usageAccumulator) ApplyTo(s *model.Session) {
	s.InputTokens = a.input
	s.OutputTokens = a.output
	s.CacheCreationTokens = a.cacheCreation
	s.CacheReadTokens = a.cacheRead
	s.APICallCount = a.apiCalls

	for _, d := range a.daily {
		s.UsageDays = append(s.UsageDays, *d)
	}
	sort.Slice(s.UsageDays, func(i, j int) bool {
		if s.UsageDays[i].Day != s.UsageDays[j].Day {
			return s.UsageDays[i].Day < s.UsageDays[j].Day
		}
		return s.UsageDays[i].Model < s.UsageDays[j].Model
	})
}
