package auth

import (
	"context"
	"math"
	"sort"
	"time"

	log "github.com/sirupsen/logrus"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// weeklyWindow is the length of the provider weekly usage window (7 days).
const weeklyWindow = 7 * 24 * time.Hour

// ResetAwareSelector prefers the credential with the soonest weekly reset that still has
// weekly quota remaining. It delegates ties or candidates with no usable observed weekly
// window to the wrapped fallback strategy (round-robin/fill-first).
//
// It only changes behavior for credentials that carry an observed weekly window (currently
// Claude OAuth/subscription auths, populated from Anthropic unified rate-limit headers).
// Everything else — Codex, API-key Claude, other providers — has no weekly window and falls
// straight through to the fallback, preserving the existing selection policy.
type ResetAwareSelector struct {
	fallback Selector
}

// NewResetAwareSelector wraps the given fallback selector with weekly reset-aware ranking.
func NewResetAwareSelector(fallback Selector) *ResetAwareSelector {
	if fallback == nil {
		fallback = &RoundRobinSelector{}
	}
	return &ResetAwareSelector{fallback: fallback}
}

// Pick selects the candidate with the soonest weekly reset and remaining weekly quota,
// skipping any credential the provider recently reported as rate-limited until its
// representative window resets. Ties and credentials with no usable weekly window fall back
// to the wrapped selector.
func (s *ResetAwareSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	now := time.Now()
	fallbackAvailable, err := getAvailableAuths(auths, provider, model, now)
	if err != nil {
		return nil, err
	}
	available, err := getAllAvailableAuths(auths, provider, model, now)
	if err != nil {
		return nil, err
	}
	available = preferCodexWebsocketAuths(ctx, provider, available)
	fallbackAvailable = preferCodexWebsocketAuths(ctx, provider, fallbackAvailable)
	entry := selectorLogEntry(ctx)

	// Drop credentials whose representative window is currently rate-limited (per the last
	// observed snapshot) until it resets, so we avoid routing into a known cap before it
	// would 429. If that removes everyone, fall through with the full set since the
	// snapshots may be stale.
	usable := make([]*Auth, 0, len(available))
	for _, candidate := range available {
		if candidate != nil && !candidate.Quota.LimitedUntil.IsZero() && candidate.Quota.LimitedUntil.After(now) {
			continue
		}
		usable = append(usable, candidate)
	}
	if len(usable) == 0 {
		usable = available
	}

	// Rank usable credentials that carry a known weekly window by soonest reset.
	var best []*Auth
	var bestReset time.Time
	hasKnown := false
	for _, candidate := range usable {
		reset, ok := effectiveWeeklyResetWithQuota(candidate, now)
		if !ok {
			continue
		}
		switch {
		case !hasKnown || reset.Before(bestReset):
			hasKnown = true
			bestReset = reset
			best = append(best[:0], candidate)
		case reset.Equal(bestReset):
			best = append(best, candidate)
		}
	}

	if !hasKnown {
		fields := resetAwareCandidatesLogFields(usable, now)
		fields["provider"] = provider
		fields["model"] = model
		fields["available_count"] = len(available)
		fields["usable_count"] = len(usable)
		fields["limited_count"] = len(available) - len(usable)
		fields["fallback_strategy"] = fallbackSelectorName(s.fallback)
		entry.WithFields(fields).Debug("reset-aware: no usable weekly window with quota remaining, using fallback")
		return s.fallback.Pick(ctx, provider, model, opts, fallbackAvailable)
	}
	if len(best) == 1 {
		fields := resetAwareQuotaLogFields(best[0], now, bestReset)
		fields["provider"] = provider
		fields["model"] = model
		fields["available_count"] = len(available)
		fields["usable_count"] = len(usable)
		fields["limited_count"] = len(available) - len(usable)
		entry.WithFields(fields).Debug("reset-aware: selected credential by soonest weekly reset")
		return best[0], nil
	}
	if len(best) == len(usable) {
		fields := resetAwareTieLogFields(best, now, bestReset)
		fields["provider"] = provider
		fields["model"] = model
		fields["available_count"] = len(available)
		fields["usable_count"] = len(usable)
		fields["limited_count"] = len(available) - len(usable)
		fields["fallback_strategy"] = fallbackSelectorName(s.fallback)
		entry.WithFields(fields).Debug("reset-aware: all usable candidates tied at weekly reset, using fallback")
		return s.fallback.Pick(ctx, provider, model, opts, usable)
	}
	fields := resetAwareTieLogFields(best, now, bestReset)
	fields["provider"] = provider
	fields["model"] = model
	fields["available_count"] = len(available)
	fields["usable_count"] = len(usable)
	fields["limited_count"] = len(available) - len(usable)
	fields["fallback_strategy"] = fallbackSelectorName(s.fallback)
	entry.WithFields(fields).Debug("reset-aware: tied candidates at weekly reset, using fallback")
	return s.fallback.Pick(ctx, provider, model, opts, best)
}

func getAllAvailableAuths(auths []*Auth, provider, model string, now time.Time) ([]*Auth, error) {
	availableByPriority, cooldownCount, earliest := collectAvailableByPriority(auths, model, now)
	if len(availableByPriority) == 0 {
		if cooldownCount == len(auths) && !earliest.IsZero() {
			providerForError := provider
			if providerForError == "mixed" {
				providerForError = ""
			}
			resetIn := earliest.Sub(now)
			if resetIn < 0 {
				resetIn = 0
			}
			return nil, newModelCooldownError(model, providerForError, resetIn)
		}
		return nil, &Error{Code: "auth_unavailable", Message: "no auth available"}
	}

	available := make([]*Auth, 0, len(auths))
	for _, bucket := range availableByPriority {
		available = append(available, bucket...)
	}
	if len(available) > 1 {
		sort.Slice(available, func(i, j int) bool { return available[i].ID < available[j].ID })
	}
	return available, nil
}

func selectorUsesResetAwareRanking(selector Selector) bool {
	switch typed := selector.(type) {
	case *ResetAwareSelector:
		return true
	case *SessionAffinitySelector:
		return selectorUsesResetAwareRanking(typed.fallback)
	default:
		return false
	}
}

// Stop releases resources held by the wrapped fallback selector, if any.
func (s *ResetAwareSelector) Stop() {
	if stoppable, ok := s.fallback.(StoppableSelector); ok {
		stoppable.Stop()
	}
}

// effectiveWeeklyResetWithQuota returns the next weekly reset for an auth whose weekly quota
// is not exhausted. The reset is rolled forward in 7-day increments until it is in the
// future so a stale observation stays usable. It returns ok=false when the auth has no
// observed weekly reset or its weekly utilization is at or above 100%.
func effectiveWeeklyResetWithQuota(auth *Auth, now time.Time) (time.Time, bool) {
	if auth == nil {
		return time.Time{}, false
	}
	reset := auth.Quota.WeeklyResetAt
	if reset.IsZero() {
		return time.Time{}, false
	}
	utilization := auth.Quota.WeeklyUtilization
	switch {
	case math.IsNaN(utilization) || utilization < 0:
		utilization = 0
	case utilization > 1:
		utilization = 1
	}
	if utilization >= 1 {
		return time.Time{}, false
	}
	for !reset.After(now) {
		reset = reset.Add(weeklyWindow)
	}
	return reset, true
}

func resetAwareQuotaLogFields(auth *Auth, now, effectiveWeeklyReset time.Time) log.Fields {
	fields := log.Fields{}
	if auth == nil {
		return fields
	}
	fields["auth_id"] = auth.ID
	if auth.Label != "" {
		fields["auth_label"] = auth.Label
	}
	fields["weekly_utilization"] = auth.Quota.WeeklyUtilization
	fields["weekly_remaining"] = quotaRemainingFraction(auth.Quota.WeeklyUtilization)
	if !effectiveWeeklyReset.IsZero() {
		fields["weekly_reset_at"] = effectiveWeeklyReset.Format(time.RFC3339)
		fields["weekly_reset_in_seconds"] = resetInSeconds(effectiveWeeklyReset, now)
	}
	if !auth.Quota.FiveHourResetAt.IsZero() {
		fields["five_hour_reset_at"] = auth.Quota.FiveHourResetAt.Format(time.RFC3339)
		fields["five_hour_reset_in_seconds"] = resetInSeconds(auth.Quota.FiveHourResetAt, now)
	}
	fields["five_hour_utilization"] = auth.Quota.FiveHourUtilization
	fields["five_hour_remaining"] = quotaRemainingFraction(auth.Quota.FiveHourUtilization)
	if !auth.Quota.LimitedUntil.IsZero() {
		fields["limited_until"] = auth.Quota.LimitedUntil.Format(time.RFC3339)
		fields["limited_in_seconds"] = resetInSeconds(auth.Quota.LimitedUntil, now)
	}
	fields["currently_limited"] = !auth.Quota.LimitedUntil.IsZero() && auth.Quota.LimitedUntil.After(now)
	if auth.Quota.UnifiedStatus != "" {
		fields["quota_status"] = auth.Quota.UnifiedStatus
	}
	return fields
}

func resetAwareTieLogFields(auths []*Auth, now, effectiveWeeklyReset time.Time) log.Fields {
	fields := log.Fields{
		"tie_count": len(auths),
	}
	if !effectiveWeeklyReset.IsZero() {
		fields["weekly_reset_at"] = effectiveWeeklyReset.Format(time.RFC3339)
		fields["weekly_reset_in_seconds"] = resetInSeconds(effectiveWeeklyReset, now)
	}
	ids := make([]string, 0, len(auths))
	fiveHourResets := make([]string, 0, len(auths))
	fiveHourUtilizations := make([]float64, 0, len(auths))
	for _, auth := range auths {
		if auth == nil {
			continue
		}
		ids = append(ids, auth.ID)
		if auth.Quota.FiveHourResetAt.IsZero() {
			fiveHourResets = append(fiveHourResets, "")
		} else {
			fiveHourResets = append(fiveHourResets, auth.Quota.FiveHourResetAt.Format(time.RFC3339))
		}
		fiveHourUtilizations = append(fiveHourUtilizations, auth.Quota.FiveHourUtilization)
	}
	fields["tied_auth_ids"] = ids
	fields["tied_five_hour_reset_at"] = fiveHourResets
	fields["tied_five_hour_utilization"] = fiveHourUtilizations
	return fields
}

func resetAwareCandidatesLogFields(auths []*Auth, now time.Time) log.Fields {
	fields := log.Fields{}
	ids := make([]string, 0, len(auths))
	weeklyResets := make([]string, 0, len(auths))
	weeklyUtilizations := make([]float64, 0, len(auths))
	fiveHourResets := make([]string, 0, len(auths))
	fiveHourUtilizations := make([]float64, 0, len(auths))
	currentlyLimited := make([]bool, 0, len(auths))
	for _, auth := range auths {
		if auth == nil {
			continue
		}
		ids = append(ids, auth.ID)
		if auth.Quota.WeeklyResetAt.IsZero() {
			weeklyResets = append(weeklyResets, "")
		} else {
			weeklyResets = append(weeklyResets, auth.Quota.WeeklyResetAt.Format(time.RFC3339))
		}
		weeklyUtilizations = append(weeklyUtilizations, auth.Quota.WeeklyUtilization)
		if auth.Quota.FiveHourResetAt.IsZero() {
			fiveHourResets = append(fiveHourResets, "")
		} else {
			fiveHourResets = append(fiveHourResets, auth.Quota.FiveHourResetAt.Format(time.RFC3339))
		}
		fiveHourUtilizations = append(fiveHourUtilizations, auth.Quota.FiveHourUtilization)
		currentlyLimited = append(currentlyLimited, !auth.Quota.LimitedUntil.IsZero() && auth.Quota.LimitedUntil.After(now))
	}
	fields["candidate_auth_ids"] = ids
	fields["candidate_weekly_reset_at"] = weeklyResets
	fields["candidate_weekly_utilization"] = weeklyUtilizations
	fields["candidate_five_hour_reset_at"] = fiveHourResets
	fields["candidate_five_hour_utilization"] = fiveHourUtilizations
	fields["candidate_currently_limited"] = currentlyLimited
	return fields
}

func quotaRemainingFraction(utilization float64) float64 {
	switch {
	case math.IsNaN(utilization) || utilization < 0:
		utilization = 0
	case utilization > 1:
		utilization = 1
	}
	return 1 - utilization
}

func resetInSeconds(reset, now time.Time) int64 {
	if reset.IsZero() {
		return 0
	}
	seconds := int64(math.Ceil(reset.Sub(now).Seconds()))
	if seconds < 0 {
		return 0
	}
	return seconds
}

func fallbackSelectorName(selector Selector) string {
	switch selector.(type) {
	case *FillFirstSelector:
		return "fill-first"
	case *RoundRobinSelector:
		return "round-robin"
	default:
		return "custom"
	}
}
