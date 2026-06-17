package helps

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

// Anthropic unified rate-limit header names. These are returned on subscription/OAuth
// (Claude Pro/Max) responses; plain API-key responses use the older tiered
// anthropic-ratelimit-* headers and won't carry these.
const (
	anthropicUnifiedStatusHeader      = "Anthropic-Ratelimit-Unified-Status"
	anthropicUnifiedClaimHeader       = "Anthropic-Ratelimit-Unified-Representative-Claim"
	anthropicUnifiedWeeklyResetHeader = "Anthropic-Ratelimit-Unified-7d-Reset"
	anthropicUnifiedWeeklyUtilHeader  = "Anthropic-Ratelimit-Unified-7d-Utilization"
	anthropicUnified5hResetHeader     = "Anthropic-Ratelimit-Unified-5h-Reset"
	anthropicUnified5hUtilHeader      = "Anthropic-Ratelimit-Unified-5h-Utilization"
)

// ParseAnthropicUnifiedRateLimits extracts the Anthropic unified rate-limit snapshot from
// response headers. It returns ok=false when none of the unified headers are present (for
// example API-key responses), so callers can skip attaching a snapshot.
//
// The reset headers are parsed as RFC3339, a Unix timestamp, or a relative seconds value,
// since the exact wire format should be confirmed against a live response; the parsing is
// centralized here so only this function needs updating.
func ParseAnthropicUnifiedRateLimits(h http.Header, now time.Time) (cliproxyexecutor.RateLimitSnapshot, bool) {
	if h == nil {
		return cliproxyexecutor.RateLimitSnapshot{}, false
	}

	status := strings.TrimSpace(h.Get(anthropicUnifiedStatusHeader))
	claim := strings.TrimSpace(h.Get(anthropicUnifiedClaimHeader))
	weeklyReset, hasWeeklyReset := parseRateLimitReset(h.Get(anthropicUnifiedWeeklyResetHeader), now)
	fiveHourReset, hasFiveHourReset := parseRateLimitReset(h.Get(anthropicUnified5hResetHeader), now)
	weeklyUtil, hasWeeklyUtil := parseRateLimitFraction(h.Get(anthropicUnifiedWeeklyUtilHeader))
	fiveHourUtil, hasFiveHourUtil := parseRateLimitFraction(h.Get(anthropicUnified5hUtilHeader))

	if status == "" && claim == "" && !hasWeeklyReset && !hasFiveHourReset && !hasWeeklyUtil && !hasFiveHourUtil {
		return cliproxyexecutor.RateLimitSnapshot{}, false
	}

	snapshot := cliproxyexecutor.RateLimitSnapshot{
		Status:               strings.ToLower(status),
		RepresentativeWindow: strings.ToLower(claim),
		WeeklyUtilization:    weeklyUtil,
		FiveHourUtilization:  fiveHourUtil,
	}
	if hasWeeklyReset {
		snapshot.WeeklyResetAt = weeklyReset
	}
	if hasFiveHourReset {
		snapshot.FiveHourResetAt = fiveHourReset
	}
	return snapshot, true
}

// ParseAnthropicOAuthUsageRateLimits extracts the same reset-aware quota signal from
// Anthropic's OAuth usage endpoint body used by the management UI.
func ParseAnthropicOAuthUsageRateLimits(body []byte, now time.Time) (cliproxyexecutor.RateLimitSnapshot, bool) {
	if len(body) == 0 {
		return cliproxyexecutor.RateLimitSnapshot{}, false
	}

	var snapshot cliproxyexecutor.RateLimitSnapshot
	var hasAny bool

	if reset, util, ok := parseAnthropicUsageWindow(body, "five_hour", now); ok {
		snapshot.FiveHourResetAt = reset
		snapshot.FiveHourUtilization = util
		hasAny = true
	}

	weeklyReset, weeklyUtil, hasWeekly := parseAnthropicUsageWindow(body, "seven_day", now)
	if !hasWeekly {
		weeklyReset, weeklyUtil, hasWeekly = bestAnthropicWeeklyUsageWindow(body, now)
	}
	if hasWeekly {
		snapshot.WeeklyResetAt = weeklyReset
		snapshot.WeeklyUtilization = weeklyUtil
		hasAny = true
	}

	if !hasAny {
		return cliproxyexecutor.RateLimitSnapshot{}, false
	}

	snapshot.Status = "allowed"
	switch {
	case snapshot.FiveHourUtilization >= 1 && !snapshot.FiveHourResetAt.IsZero():
		snapshot.Status = "rate_limited"
		snapshot.RepresentativeWindow = "five_hour"
	case snapshot.WeeklyUtilization >= 1 && !snapshot.WeeklyResetAt.IsZero():
		snapshot.Status = "rate_limited"
		snapshot.RepresentativeWindow = "seven_day"
	}
	return snapshot, true
}

func parseAnthropicUsageWindow(body []byte, key string, now time.Time) (time.Time, float64, bool) {
	if len(body) == 0 || key == "" {
		return time.Time{}, 0, false
	}
	prefix := key + "."
	util, hasUtil := parseRateLimitFraction(gjson.GetBytes(body, prefix+"utilization").String())
	reset, hasReset := parseRateLimitReset(firstNonEmptyGJSONString(
		gjson.GetBytes(body, prefix+"resets_at"),
		gjson.GetBytes(body, prefix+"reset_at"),
	), now)
	if !hasUtil && !hasReset {
		return time.Time{}, 0, false
	}
	return reset, util, true
}

func bestAnthropicWeeklyUsageWindow(body []byte, now time.Time) (time.Time, float64, bool) {
	keys := []string{
		"seven_day_oauth_apps",
		"seven_day_opus",
		"seven_day_sonnet",
		"seven_day_cowork",
		"iguana_necktie",
	}
	var bestReset time.Time
	var bestUtil float64
	var ok bool
	for _, key := range keys {
		reset, util, hasWindow := parseAnthropicUsageWindow(body, key, now)
		if !hasWindow {
			continue
		}
		if !ok || util > bestUtil {
			bestReset = reset
			bestUtil = util
			ok = true
		}
	}
	return bestReset, bestUtil, ok
}

func firstNonEmptyGJSONString(values ...gjson.Result) string {
	for _, value := range values {
		if !value.Exists() {
			continue
		}
		if s := strings.TrimSpace(value.String()); s != "" {
			return s
		}
	}
	return ""
}

// parseRateLimitReset parses a reset header into an absolute time. It accepts an RFC3339
// timestamp, an absolute Unix timestamp, or a relative "seconds from now" value.
func parseRateLimitReset(raw string, now time.Time) (time.Time, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, false
	}
	if ts, err := time.Parse(time.RFC3339, raw); err == nil {
		return ts, true
	}
	if n, err := strconv.ParseInt(raw, 10, 64); err == nil && n > 0 {
		// Large values are absolute Unix timestamps; small values are seconds-until-reset.
		if n > 1_000_000_000 {
			return time.Unix(n, 0), true
		}
		return now.Add(time.Duration(n) * time.Second), true
	}
	return time.Time{}, false
}

// parseRateLimitFraction parses a utilization header into a 0..1 fraction. Values greater
// than 1 are treated as percentages and divided by 100.
func parseRateLimitFraction(raw string) (float64, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, false
	}
	if v > 1 {
		v /= 100
	}
	if v < 0 {
		v = 0
	}
	return v, true
}
