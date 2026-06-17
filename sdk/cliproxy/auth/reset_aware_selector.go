package auth

import (
	"context"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// weeklyWindow is the length of the provider weekly usage window (7 days).
const weeklyWindow = 7 * 24 * time.Hour

// ResetAwareSelector prefers the credential whose weekly quota is most at risk of being
// wasted before its window resets ("use it or lose it"). It ranks candidates by required
// weekly burn-rate (remaining fraction / time until reset) and delegates ties or candidates
// with no observed weekly window to the wrapped fallback strategy (round-robin/fill-first).
//
// It only changes behavior for credentials that carry an observed weekly window (currently
// Claude OAuth/subscription auths, populated from Anthropic unified rate-limit headers).
// Everything else — Codex, API-key Claude, other providers — has no weekly window and falls
// straight through to the fallback, preserving the existing selection policy.
type ResetAwareSelector struct {
	fallback Selector
}

// NewResetAwareSelector wraps the given fallback selector with reset-aware ranking.
func NewResetAwareSelector(fallback Selector) *ResetAwareSelector {
	if fallback == nil {
		fallback = &RoundRobinSelector{}
	}
	return &ResetAwareSelector{fallback: fallback}
}

// Pick selects the candidate with the highest required weekly burn-rate, skipping any
// credential the provider recently reported as rate-limited until its representative window
// resets. Ties and credentials with no known weekly window fall back to the wrapped selector.
func (s *ResetAwareSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	now := time.Now()
	available, err := getAvailableAuths(auths, provider, model, now)
	if err != nil {
		return nil, err
	}
	available = preferCodexWebsocketAuths(ctx, provider, available)
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

	// Rank usable credentials that carry a known weekly window by burn-rate.
	var best []*Auth
	var bestRate float64
	hasKnown := false
	for _, candidate := range usable {
		rate, ok := weeklyBurnRate(candidate, now)
		if !ok {
			continue
		}
		switch {
		case !hasKnown || rate > bestRate:
			hasKnown = true
			bestRate = rate
			best = append(best[:0], candidate)
		case rate == bestRate:
			best = append(best, candidate)
		}
	}

	if !hasKnown {
		entry.Debugf("reset-aware: no known weekly window, using fallback over %d usable candidates | provider=%s model=%s", len(usable), provider, model)
		return s.fallback.Pick(ctx, provider, model, opts, usable)
	}
	if len(best) == 1 {
		entry.Debugf("reset-aware: selected %s burn_rate=%.6f | provider=%s model=%s", best[0].ID, bestRate, provider, model)
		return best[0], nil
	}
	if len(best) == len(usable) {
		entry.Debugf("reset-aware: all %d usable candidates tied at burn_rate=%.6f, using fallback | provider=%s model=%s", len(usable), bestRate, provider, model)
		return s.fallback.Pick(ctx, provider, model, opts, usable)
	}
	entry.Debugf("reset-aware: %d credentials tied at burn_rate=%.6f, using fallback | provider=%s model=%s", len(best), bestRate, provider, model)
	return s.fallback.Pick(ctx, provider, model, opts, best)
}

// Stop releases resources held by the wrapped fallback selector, if any.
func (s *ResetAwareSelector) Stop() {
	if stoppable, ok := s.fallback.(StoppableSelector); ok {
		stoppable.Stop()
	}
}

// weeklyBurnRate returns the required weekly burn-rate for an auth, defined as the remaining
// weekly fraction divided by the seconds until the weekly window resets. A higher value means
// the credential's weekly quota is more likely to be wasted if not used soon. The reset is
// rolled forward in 7-day increments until it is in the future so a stale observation stays
// usable. It returns ok=false when the auth has no observed weekly reset.
func weeklyBurnRate(auth *Auth, now time.Time) (float64, bool) {
	if auth == nil {
		return 0, false
	}
	reset := auth.Quota.WeeklyResetAt
	if reset.IsZero() {
		return 0, false
	}
	for !reset.After(now) {
		reset = reset.Add(weeklyWindow)
	}
	remaining := 1 - auth.Quota.WeeklyUtilization
	if remaining < 0 {
		remaining = 0
	}
	seconds := reset.Sub(now).Seconds()
	if seconds < 1 {
		seconds = 1
	}
	return remaining / seconds, true
}
