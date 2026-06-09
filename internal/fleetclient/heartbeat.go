package fleetclient

import (
	"os"
	"time"

	"github.com/timae/ses/internal/db"
	"github.com/timae/ses/internal/fleet"
)

// heartbeatWindowDays is how much per-day history each heartbeat carries.
// The server upserts by (user, day), so re-sending a sliding window keeps
// both sides converged even after local rescans rewrite history.
const heartbeatWindowDays = 14

// BuildHeartbeat assembles the seat's usage heartbeat from the local index.
func BuildHeartbeat(store *db.DB, version string, worker fleet.WorkerStatus) (fleet.Heartbeat, error) {
	hostname, _ := os.Hostname()
	hb := fleet.Heartbeat{
		Hostname:   hostname,
		SesVersion: version,
		Worker:     worker,
	}

	since := time.Now().AddDate(0, 0, -(heartbeatWindowDays - 1)).Format("2006-01-02")
	days, err := store.UsageByDay(since, "")
	if err != nil {
		return hb, err
	}
	for _, d := range days {
		hb.UsageDays = append(hb.UsageDays, fleet.UsageDay{
			Day:                 d.Day,
			InputTokens:         d.InputTokens,
			OutputTokens:        d.OutputTokens,
			CacheCreationTokens: d.CacheCreationTokens,
			CacheReadTokens:     d.CacheReadTokens,
			APICalls:            d.APICalls,
			Sessions:            d.Sessions,
		})
	}
	return hb, nil
}

// MaybeHeartbeat sends a heartbeat if the seat is enrolled in a fleet
// (fleet.json exists); otherwise it is a silent no-op. Errors are returned
// for optional logging but callers may ignore them — heartbeats are
// best-effort telemetry.
func MaybeHeartbeat(store *db.DB, version string, worker fleet.WorkerStatus) error {
	if !Configured() {
		return nil
	}
	c, err := Load()
	if err != nil {
		return err
	}
	hb, err := BuildHeartbeat(store, version, worker)
	if err != nil {
		return err
	}
	return c.Heartbeat(hb)
}
