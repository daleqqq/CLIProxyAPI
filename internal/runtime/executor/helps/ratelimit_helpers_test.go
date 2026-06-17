package helps

import (
	"net/http"
	"strconv"
	"testing"
	"time"
)

func TestParseAnthropicUnifiedRateLimits_Present(t *testing.T) {
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	weekly := now.Add(48 * time.Hour)

	h := http.Header{}
	h.Set("anthropic-ratelimit-unified-status", "allowed")
	h.Set("anthropic-ratelimit-unified-representative-claim", "five_hour")
	h.Set("anthropic-ratelimit-unified-7d-reset", weekly.Format(time.RFC3339))
	h.Set("anthropic-ratelimit-unified-7d-utilization", "31")   // percent -> 0.31
	h.Set("anthropic-ratelimit-unified-5h-utilization", "0.93") // already a fraction

	snap, ok := ParseAnthropicUnifiedRateLimits(h, now)
	if !ok {
		t.Fatalf("ParseAnthropicUnifiedRateLimits() ok = false, want true")
	}
	if snap.Status != "allowed" {
		t.Fatalf("Status = %q, want allowed", snap.Status)
	}
	if snap.RepresentativeWindow != "five_hour" {
		t.Fatalf("RepresentativeWindow = %q, want five_hour", snap.RepresentativeWindow)
	}
	if !snap.WeeklyResetAt.Equal(weekly) {
		t.Fatalf("WeeklyResetAt = %v, want %v", snap.WeeklyResetAt, weekly)
	}
	if snap.WeeklyUtilization < 0.30 || snap.WeeklyUtilization > 0.32 {
		t.Fatalf("WeeklyUtilization = %v, want ~0.31", snap.WeeklyUtilization)
	}
	if snap.FiveHourUtilization < 0.92 || snap.FiveHourUtilization > 0.94 {
		t.Fatalf("FiveHourUtilization = %v, want ~0.93", snap.FiveHourUtilization)
	}
}

func TestParseAnthropicUnifiedRateLimits_Absent(t *testing.T) {
	h := http.Header{}
	h.Set("anthropic-ratelimit-requests-remaining", "100") // tiered (API-key) headers only
	if _, ok := ParseAnthropicUnifiedRateLimits(h, time.Now()); ok {
		t.Fatalf("ParseAnthropicUnifiedRateLimits() ok = true, want false (no unified headers)")
	}
	if _, ok := ParseAnthropicUnifiedRateLimits(nil, time.Now()); ok {
		t.Fatalf("ParseAnthropicUnifiedRateLimits(nil) ok = true, want false")
	}
}

func TestParseAnthropicOAuthUsageRateLimits(t *testing.T) {
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	fiveHour := now.Add(2 * time.Hour)
	weekly := now.Add(48 * time.Hour)
	body := []byte(`{
		"five_hour":{"utilization":93,"resets_at":"` + fiveHour.Format(time.RFC3339) + `"},
		"seven_day":{"utilization":0.31,"resets_at":"` + weekly.Format(time.RFC3339) + `"}
	}`)

	snap, ok := ParseAnthropicOAuthUsageRateLimits(body, now)
	if !ok {
		t.Fatalf("ParseAnthropicOAuthUsageRateLimits() ok = false, want true")
	}
	if snap.Status != "allowed" {
		t.Fatalf("Status = %q, want allowed", snap.Status)
	}
	if !snap.FiveHourResetAt.Equal(fiveHour) {
		t.Fatalf("FiveHourResetAt = %v, want %v", snap.FiveHourResetAt, fiveHour)
	}
	if !snap.WeeklyResetAt.Equal(weekly) {
		t.Fatalf("WeeklyResetAt = %v, want %v", snap.WeeklyResetAt, weekly)
	}
	if snap.FiveHourUtilization < 0.92 || snap.FiveHourUtilization > 0.94 {
		t.Fatalf("FiveHourUtilization = %v, want ~0.93", snap.FiveHourUtilization)
	}
	if snap.WeeklyUtilization < 0.30 || snap.WeeklyUtilization > 0.32 {
		t.Fatalf("WeeklyUtilization = %v, want ~0.31", snap.WeeklyUtilization)
	}
}

func TestParseAnthropicOAuthUsageRateLimits_RateLimited(t *testing.T) {
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	fiveHour := now.Add(time.Hour)
	body := []byte(`{"five_hour":{"utilization":100,"resets_at":"` + fiveHour.Format(time.RFC3339) + `"}}`)

	snap, ok := ParseAnthropicOAuthUsageRateLimits(body, now)
	if !ok {
		t.Fatalf("ParseAnthropicOAuthUsageRateLimits() ok = false, want true")
	}
	if snap.Status != "rate_limited" {
		t.Fatalf("Status = %q, want rate_limited", snap.Status)
	}
	if snap.RepresentativeWindow != "five_hour" {
		t.Fatalf("RepresentativeWindow = %q, want five_hour", snap.RepresentativeWindow)
	}
}

func TestParseRateLimitReset_Formats(t *testing.T) {
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

	t.Run("rfc3339", func(t *testing.T) {
		want := now.Add(time.Hour)
		got, ok := parseRateLimitReset(want.Format(time.RFC3339), now)
		if !ok || !got.Equal(want) {
			t.Fatalf("parseRateLimitReset(rfc3339) = %v, %v; want %v", got, ok, want)
		}
	})

	t.Run("relative seconds", func(t *testing.T) {
		got, ok := parseRateLimitReset("3600", now)
		if !ok || !got.Equal(now.Add(time.Hour)) {
			t.Fatalf("parseRateLimitReset(relative) = %v, %v; want now+1h", got, ok)
		}
	})

	t.Run("unix timestamp", func(t *testing.T) {
		ts := now.Add(72 * time.Hour).Unix()
		got, ok := parseRateLimitReset(strconv.FormatInt(ts, 10), now)
		if !ok || got.Unix() != ts {
			t.Fatalf("parseRateLimitReset(unix) = %v, %v; want unix %d", got, ok, ts)
		}
	})

	t.Run("empty", func(t *testing.T) {
		if _, ok := parseRateLimitReset("", now); ok {
			t.Fatalf("parseRateLimitReset(empty) ok = true, want false")
		}
	})
}
