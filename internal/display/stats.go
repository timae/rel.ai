package display

import (
	"fmt"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/fatih/color"
	"github.com/timae/ses/internal/db"
)

func StatsDashboard(s *db.Stats) {
	w := 42
	line := strings.Repeat("═", w)
	thin := strings.Repeat("─", w)

	fmt.Println("╔" + line + "╗")
	centerBox(w, "SESSION STATISTICS", true)
	fmt.Println("╠" + line + "╣")

	fieldBox(w, "Total sessions", fmt.Sprintf("%d", s.Total))
	fieldBox(w, "This week", fmt.Sprintf("%d", s.ThisWeek))
	fieldBox(w, "Avg duration", formatDuration(s.AvgDurationSec))
	fieldBox(w, "Avg messages/session", fmt.Sprintf("%.0f", s.AvgMessages))
	fieldBox(w, "Total tool calls", fmt.Sprintf("%d", s.TotalToolCalls))

	// By source
	if len(s.BySource) > 0 {
		fmt.Println("╠" + line + "╣")
		centerBox(w, "BY SOURCE", false)
		fmt.Println("║" + strings.Repeat(" ", w) + "║")
		for name, count := range s.BySource {
			pct := 0
			if s.Total > 0 {
				pct = count * 100 / s.Total
			}
			fieldBox(w, "  "+name, fmt.Sprintf("%d  (%d%%)", count, pct))
		}
	}

	// By model
	if len(s.ByModel) > 0 {
		fmt.Println("╠" + line + "╣")
		centerBox(w, "BY MODEL", false)
		fmt.Println("║" + strings.Repeat(" ", w) + "║")
		for name, count := range s.ByModel {
			fieldBox(w, "  "+name, fmt.Sprintf("%d", count))
		}
	}

	// Top projects
	if len(s.TopProjects) > 0 {
		fmt.Println("╠" + line + "╣")
		centerBox(w, "TOP PROJECTS", false)
		fmt.Println("║" + strings.Repeat(" ", w) + "║")
		for _, p := range s.TopProjects {
			name := shortenPath(p.Name)
			if len(name) > 30 {
				name = "..." + name[len(name)-27:]
			}
			fieldBox(w, "  "+name, fmt.Sprintf("%d", p.Count))
		}
	}

	// Activity heatmap
	if len(s.DailyActivity) > 0 {
		fmt.Println("╠" + line + "╣")
		centerBox(w, "ACTIVITY (last 7 days)", false)
		fmt.Println("║" + strings.Repeat(" ", w) + "║")

		maxCount := 0
		for _, d := range s.DailyActivity {
			if d.Count > maxCount {
				maxCount = d.Count
			}
		}

		barWidth := 12
		today := time.Now().Format("Mon")
		for _, d := range s.DailyActivity {
			filled := 0
			if maxCount > 0 {
				filled = d.Count * barWidth / maxCount
			}
			bar := strings.Repeat("█", filled) + strings.Repeat("░", barWidth-filled)

			marker := " "
			if d.Day == today {
				marker = "*"
			}

			content := fmt.Sprintf(" %s%s %s  %s  %-3d", marker, d.Day, d.Date, bar, d.Count)
			padded := padRightRunes(content, w)
			fmt.Fprintf(os.Stdout, "║%s║\n", padded)
		}
	}

	// Token usage
	if s.Tokens.APICalls > 0 {
		fmt.Println("╠" + line + "╣")
		centerBox(w, "TOKENS", false)
		fmt.Println("║" + strings.Repeat(" ", w) + "║")
		fieldBox(w, "  Input", formatTokens(s.Tokens.InputTokens))
		fieldBox(w, "  Output", formatTokens(s.Tokens.OutputTokens))
		fieldBox(w, "  Cache write", formatTokens(s.Tokens.CacheCreationTokens))
		fieldBox(w, "  Cache read", formatTokens(s.Tokens.CacheReadTokens))
		cacheable := s.Tokens.CacheReadTokens + s.Tokens.CacheCreationTokens
		if cacheable > 0 {
			hitPct := s.Tokens.CacheReadTokens * 100 / cacheable
			fieldBox(w, "  Cache hit rate", fmt.Sprintf("%d%%", hitPct))
		}
		for name, t := range s.TokensByModel {
			if len(name) > 24 {
				name = name[:24]
			}
			fieldBox(w, "  "+name, formatTokens(t.InputTokens+t.OutputTokens))
		}
	}

	// Top tags
	if len(s.TopTags) > 0 {
		fmt.Println("╠" + line + "╣")
		centerBox(w, "TOP TAGS", false)
		fmt.Println("║" + strings.Repeat(" ", w) + "║")
		var tagStrs []string
		for _, t := range s.TopTags {
			tagStrs = append(tagStrs, fmt.Sprintf("#%s (%d)", t.Name, t.Count))
		}
		tagLine := strings.Join(tagStrs, "  ")
		if len(tagLine) > w-4 {
			tagLine = tagLine[:w-7] + "..."
		}
		padded := padRight("  "+tagLine, w)
		fmt.Fprintf(os.Stdout, "║%s║\n", padded)
	}

	_ = thin
	fmt.Println("╚" + strings.Repeat("═", w) + "╝")
}

func centerBox(w int, text string, isBold bool) {
	pad := (w - len(text)) / 2
	if pad < 0 {
		pad = 0
	}
	line := strings.Repeat(" ", pad) + text + strings.Repeat(" ", w-pad-len(text))
	if isBold {
		fmt.Fprintf(os.Stdout, "║%s║\n", color.New(color.Bold).Sprint(line))
	} else {
		fmt.Fprintf(os.Stdout, "║%s║\n", line)
	}
}

func fieldBox(w int, label, value string) {
	gap := w - len(label) - len(value) - 2
	if gap < 1 {
		gap = 1
	}
	line := " " + label + strings.Repeat(" ", gap) + value + " "
	if len(line) < w {
		line += strings.Repeat(" ", w-len(line))
	}
	if len(line) > w {
		line = line[:w]
	}
	fmt.Fprintf(os.Stdout, "║%s║\n", line)
}

func padRight(s string, w int) string {
	if len(s) >= w {
		return s[:w]
	}
	return s + strings.Repeat(" ", w-len(s))
}

func padRightRunes(s string, w int) string {
	runeCount := utf8.RuneCountInString(s)
	if runeCount >= w {
		// Truncate by runes
		runes := []rune(s)
		return string(runes[:w])
	}
	return s + strings.Repeat(" ", w-runeCount)
}

func formatTokens(n int64) string {
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

func formatDuration(seconds float64) string {
	d := time.Duration(seconds) * time.Second
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	if h > 0 {
		return fmt.Sprintf("%dh %dm", h, m)
	}
	return fmt.Sprintf("%dm", m)
}
