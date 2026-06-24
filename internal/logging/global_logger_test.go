package logging

import (
	"strings"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"
)

func TestLogFormatterPrintsVersionField(t *testing.T) {
	entry := log.NewEntry(log.New())
	entry.Time = time.Date(2026, 6, 9, 11, 10, 2, 0, time.Local)
	entry.Level = log.InfoLevel
	entry.Message = "fetched latest antigravity version"
	entry.Data["version"] = "2.1.0"

	formatted, errFormat := (&LogFormatter{}).Format(entry)
	if errFormat != nil {
		t.Fatalf("Format() error = %v", errFormat)
	}

	line := string(formatted)
	if !strings.Contains(line, "version=2.1.0") {
		t.Fatalf("formatted line %q missing version field", line)
	}
}

func TestLogFormatterPrintsResetAwareQuotaFields(t *testing.T) {
	entry := log.NewEntry(log.New())
	entry.Time = time.Date(2026, 6, 24, 17, 1, 40, 0, time.Local)
	entry.Level = log.DebugLevel
	entry.Message = "reset-aware: selected credential by soonest weekly reset"
	entry.Data["provider"] = "mixed"
	entry.Data["model"] = "claude-sonnet-4-6"
	entry.Data["auth_id"] = "claude-a"
	entry.Data["weekly_reset_at"] = "2026-06-25T10:00:00Z"
	entry.Data["weekly_reset_in_seconds"] = int64(3600)
	entry.Data["five_hour_reset_at"] = "2026-06-24T20:00:00Z"
	entry.Data["five_hour_reset_in_seconds"] = int64(900)
	entry.Data["five_hour_utilization"] = 0.5
	entry.Data["currently_limited"] = false
	entry.Data["candidate_auth_ids"] = []string{"claude-a"}
	entry.Data["candidate_weekly_utilization"] = []float64{0.5}

	formatted, errFormat := (&LogFormatter{}).Format(entry)
	if errFormat != nil {
		t.Fatalf("Format() error = %v", errFormat)
	}

	line := string(formatted)
	for _, want := range []string{
		"auth_id=claude-a",
		"weekly_reset_at=2026-06-25T10:00:00Z",
		"weekly_reset_in_seconds=3600",
		"five_hour_reset_at=2026-06-24T20:00:00Z",
		"five_hour_reset_in_seconds=900",
		"five_hour_utilization=0.5",
		"currently_limited=false",
		"candidate_auth_ids=[claude-a]",
		"candidate_weekly_utilization=[0.5]",
	} {
		if !strings.Contains(line, want) {
			t.Fatalf("formatted line %q missing %q", line, want)
		}
	}
}
