package auth

import (
	"context"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func weeklyAuth(id string, resetIn time.Duration, utilization float64) *Auth {
	return &Auth{
		ID: id,
		Quota: QuotaState{
			WeeklyResetAt:     time.Now().Add(resetIn),
			WeeklyUtilization: utilization,
		},
	}
}

func pickResetAware(t *testing.T, fallback Selector, auths []*Auth) *Auth {
	t.Helper()
	selector := NewResetAwareSelector(fallback)
	got, err := selector.Pick(context.Background(), "claude", "", cliproxyexecutor.Options{}, auths)
	if err != nil {
		t.Fatalf("Pick() error = %v", err)
	}
	if got == nil {
		t.Fatalf("Pick() auth = nil")
	}
	return got
}

// TestResetAwareSelector_HighestBurnRateWins verifies the burn-rate metric beats a
// timing-only heuristic: an account with a large remaining budget resetting slightly later
// outranks one with little left that resets sooner.
func TestResetAwareSelector_HighestBurnRateWins(t *testing.T) {
	t.Parallel()

	// P: 8% remaining (0.92 used), resets in 1 day  -> burn-rate ~0.08/day
	// Q: 85% remaining (0.15 used), resets in 3 days -> burn-rate ~0.28/day
	p := weeklyAuth("p", 24*time.Hour, 0.92)
	q := weeklyAuth("q", 72*time.Hour, 0.15)

	got := pickResetAware(t, &FillFirstSelector{}, []*Auth{p, q})
	if got.ID != "q" {
		t.Fatalf("Pick() = %q, want %q (higher burn-rate)", got.ID, "q")
	}
}

// TestResetAwareSelector_ABCScenario reproduces the grilled A/B/C scenario where the
// soonest-resetting account also has the highest burn-rate.
func TestResetAwareSelector_ABCScenario(t *testing.T) {
	t.Parallel()

	a := weeklyAuth("a", 96*time.Hour, 0.20)  // 80% remaining, 4 days
	b := weeklyAuth("b", 48*time.Hour, 0.31)  // 69% remaining, 2 days
	c := weeklyAuth("c", 144*time.Hour, 0.11) // 89% remaining, 6 days

	got := pickResetAware(t, &FillFirstSelector{}, []*Auth{a, b, c})
	if got.ID != "b" {
		t.Fatalf("Pick() = %q, want %q", got.ID, "b")
	}
}

// TestResetAwareSelector_TieDelegatesToFallback verifies equal burn-rates defer to the
// wrapped strategy.
func TestResetAwareSelector_TieDelegatesToFallback(t *testing.T) {
	t.Parallel()

	// Identical reset + utilization yields an exact burn-rate tie. Use a shared timestamp
	// so the two auths are truly equal (separate time.Now() calls would differ slightly).
	reset := time.Now().Add(48 * time.Hour)
	a := &Auth{ID: "b", Quota: QuotaState{WeeklyResetAt: reset, WeeklyUtilization: 0.30}}
	b := &Auth{ID: "a", Quota: QuotaState{WeeklyResetAt: reset, WeeklyUtilization: 0.30}}

	// FillFirst fallback sorts available by ID, so the tie resolves to "a".
	got := pickResetAware(t, &FillFirstSelector{}, []*Auth{a, b})
	if got.ID != "a" {
		t.Fatalf("Pick() = %q, want %q (fallback breaks tie by ID)", got.ID, "a")
	}
}

// TestResetAwareSelector_AllUnknownFallback verifies credentials with no weekly window
// fall straight through to the base strategy (Codex / API-key behaviour is preserved).
func TestResetAwareSelector_AllUnknownFallback(t *testing.T) {
	t.Parallel()

	auths := []*Auth{{ID: "b"}, {ID: "a"}, {ID: "c"}}
	got := pickResetAware(t, &FillFirstSelector{}, auths)
	if got.ID != "a" {
		t.Fatalf("Pick() = %q, want %q (fallback over unknowns)", got.ID, "a")
	}
}

func TestResetAwareSelector_AllUnknownUsesRoundRobinFallback(t *testing.T) {
	t.Parallel()

	selector := NewResetAwareSelector(&RoundRobinSelector{})
	auths := []*Auth{{ID: "b"}, {ID: "a"}}

	first, err := selector.Pick(context.Background(), "claude", "", cliproxyexecutor.Options{}, auths)
	if err != nil {
		t.Fatalf("Pick() first error = %v", err)
	}
	second, err := selector.Pick(context.Background(), "claude", "", cliproxyexecutor.Options{}, auths)
	if err != nil {
		t.Fatalf("Pick() second error = %v", err)
	}
	if first == nil || second == nil {
		t.Fatalf("Pick() returned nil auths: first=%v second=%v", first, second)
	}
	if first.ID != "a" || second.ID != "b" {
		t.Fatalf("Pick() sequence = %q,%q; want a,b from round-robin fallback", first.ID, second.ID)
	}
}

func TestResetAwareSelector_AllEqualBurnRateUsesRoundRobinFallback(t *testing.T) {
	t.Parallel()

	reset := time.Now().Add(48 * time.Hour)
	auths := []*Auth{
		{ID: "b", Quota: QuotaState{WeeklyResetAt: reset, WeeklyUtilization: 0.30}},
		{ID: "a", Quota: QuotaState{WeeklyResetAt: reset, WeeklyUtilization: 0.30}},
	}
	selector := NewResetAwareSelector(&RoundRobinSelector{})

	first, err := selector.Pick(context.Background(), "claude", "", cliproxyexecutor.Options{}, auths)
	if err != nil {
		t.Fatalf("Pick() first error = %v", err)
	}
	second, err := selector.Pick(context.Background(), "claude", "", cliproxyexecutor.Options{}, auths)
	if err != nil {
		t.Fatalf("Pick() second error = %v", err)
	}
	if first == nil || second == nil {
		t.Fatalf("Pick() returned nil auths: first=%v second=%v", first, second)
	}
	if first.ID != "a" || second.ID != "b" {
		t.Fatalf("Pick() sequence = %q,%q; want a,b from round-robin fallback", first.ID, second.ID)
	}
}

// TestResetAwareSelector_KnownBeatsUnknown verifies a credential with a weekly window is
// preferred over one without (which is deferred to the fallback only when no known exists).
func TestResetAwareSelector_KnownBeatsUnknown(t *testing.T) {
	t.Parallel()

	known := weeklyAuth("known", 24*time.Hour, 0.10)
	unknown := &Auth{ID: "aaa-unknown"} // sorts first by ID, but has no weekly window

	got := pickResetAware(t, &FillFirstSelector{}, []*Auth{unknown, known})
	if got.ID != "known" {
		t.Fatalf("Pick() = %q, want %q", got.ID, "known")
	}
}

// TestResetAwareSelector_SkipsLimitedUntil verifies an account whose representative window
// is currently rate-limited is skipped in favour of the next-best usable account.
func TestResetAwareSelector_SkipsLimitedUntil(t *testing.T) {
	t.Parallel()

	// "capped" has the highest burn-rate but is rate-limited until later.
	capped := weeklyAuth("capped", 24*time.Hour, 0.10)
	capped.Quota.UnifiedStatus = "rate_limited"
	capped.Quota.LimitedUntil = time.Now().Add(2 * time.Hour)

	open := weeklyAuth("open", 72*time.Hour, 0.20)

	got := pickResetAware(t, &FillFirstSelector{}, []*Auth{capped, open})
	if got.ID != "open" {
		t.Fatalf("Pick() = %q, want %q (capped account skipped)", got.ID, "open")
	}
}

// TestResetAwareSelector_AllLimitedFallsThrough verifies that when every candidate is
// rate-limited we still return one (snapshots may be stale) rather than erroring.
func TestResetAwareSelector_AllLimitedFallsThrough(t *testing.T) {
	t.Parallel()

	mk := func(id string, resetIn time.Duration, util float64) *Auth {
		a := weeklyAuth(id, resetIn, util)
		a.Quota.UnifiedStatus = "rate_limited"
		a.Quota.LimitedUntil = time.Now().Add(time.Hour)
		return a
	}
	// Even all-limited, the highest burn-rate among them should be chosen.
	low := mk("low", 144*time.Hour, 0.10)
	high := mk("high", 24*time.Hour, 0.10)

	got := pickResetAware(t, &FillFirstSelector{}, []*Auth{low, high})
	if got.ID != "high" {
		t.Fatalf("Pick() = %q, want %q", got.ID, "high")
	}
}

// TestResetAwareSelector_PriorityBucket verifies reset-aware ranking only operates within
// the highest available priority bucket.
func TestResetAwareSelector_PriorityBucket(t *testing.T) {
	t.Parallel()

	// High priority but low burn-rate.
	hiPri := weeklyAuth("hi", 144*time.Hour, 0.10)
	hiPri.Attributes = map[string]string{"priority": "10"}
	// Low priority but very high burn-rate (should still lose: filtered out by priority).
	loPri := weeklyAuth("lo", 1*time.Hour, 0.10)
	loPri.Attributes = map[string]string{"priority": "0"}

	got := pickResetAware(t, &FillFirstSelector{}, []*Auth{loPri, hiPri})
	if got.ID != "hi" {
		t.Fatalf("Pick() = %q, want %q (priority bucket wins before burn-rate)", got.ID, "hi")
	}
}

// TestResetAwareSelector_ExcludesCooledAuth verifies reactively cooled auths are excluded.
func TestResetAwareSelector_ExcludesCooledAuth(t *testing.T) {
	t.Parallel()

	cooled := weeklyAuth("cooled", 24*time.Hour, 0.10) // best burn-rate, but cooled
	cooled.Unavailable = true
	cooled.NextRetryAfter = time.Now().Add(time.Hour)
	cooled.Quota.Exceeded = true

	open := weeklyAuth("open", 72*time.Hour, 0.20)

	got := pickResetAware(t, &FillFirstSelector{}, []*Auth{cooled, open})
	if got.ID != "open" {
		t.Fatalf("Pick() = %q, want %q (cooled auth excluded)", got.ID, "open")
	}
}

func TestWeeklyBurnRate_RollForwardStaleReset(t *testing.T) {
	t.Parallel()

	now := time.Now()
	// Reset 3 days in the past should roll forward to ~4 days in the future (7d window).
	auth := &Auth{Quota: QuotaState{WeeklyResetAt: now.Add(-3 * 24 * time.Hour), WeeklyUtilization: 0.5}}
	rate, ok := weeklyBurnRate(auth, now)
	if !ok {
		t.Fatalf("weeklyBurnRate() ok = false, want true (stale reset should roll forward)")
	}
	if rate <= 0 {
		t.Fatalf("weeklyBurnRate() = %v, want > 0", rate)
	}
}

func TestWeeklyBurnRate_Unknown(t *testing.T) {
	t.Parallel()

	if _, ok := weeklyBurnRate(&Auth{}, time.Now()); ok {
		t.Fatalf("weeklyBurnRate() ok = true for zero reset, want false")
	}
	if _, ok := weeklyBurnRate(nil, time.Now()); ok {
		t.Fatalf("weeklyBurnRate(nil) ok = true, want false")
	}
}
