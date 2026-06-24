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

// TestResetAwareSelector_SoonestWeeklyResetWins verifies the selector prefers the account
// whose weekly window resets soonest, even when another account has more remaining quota.
func TestResetAwareSelector_SoonestWeeklyResetWins(t *testing.T) {
	t.Parallel()

	soon := weeklyAuth("soon", 24*time.Hour, 0.92)
	later := weeklyAuth("later", 72*time.Hour, 0.15)

	got := pickResetAware(t, &FillFirstSelector{}, []*Auth{soon, later})
	if got.ID != "soon" {
		t.Fatalf("Pick() = %q, want %q (soonest weekly reset)", got.ID, "soon")
	}
}

func TestManagerPickNextUsesResetAwareSelector(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	model := "claude-reset-aware-manager-test"
	manager := NewManager(nil, NewResetAwareSelector(&FillFirstSelector{}), nil)
	manager.RegisterExecutor(schedulerProviderTestExecutor{provider: "claude"})
	if manager.useSchedulerFastPath() {
		t.Fatalf("reset-aware selector should use legacy selector path, not scheduler fast path")
	}

	soonReset := weeklyAuth("manager-soon-reset", 24*time.Hour, 0.92)
	soonReset.Provider = "claude"
	laterReset := weeklyAuth("manager-later-reset", 72*time.Hour, 0.15)
	laterReset.Provider = "claude"
	registerSchedulerModels(t, "claude", model, soonReset.ID, laterReset.ID)

	if _, err := manager.Register(ctx, soonReset); err != nil {
		t.Fatalf("register soonReset auth: %v", err)
	}
	if _, err := manager.Register(ctx, laterReset); err != nil {
		t.Fatalf("register laterReset auth: %v", err)
	}

	got, _, err := manager.pickNext(ctx, "claude", model, cliproxyexecutor.Options{}, nil)
	if err != nil {
		t.Fatalf("pickNext() error = %v", err)
	}
	if got == nil {
		t.Fatalf("pickNext() auth = nil")
	}
	if got.ID != soonReset.ID {
		t.Fatalf("pickNext() = %q, want %q from reset-aware soonest weekly reset ranking", got.ID, soonReset.ID)
	}
}

func TestManagerPickNextResetAwareConsidersLowerPriorityQuota(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	model := "claude-reset-aware-priority-test"
	manager := NewManager(nil, NewResetAwareSelector(&FillFirstSelector{}), nil)
	manager.RegisterExecutor(schedulerProviderTestExecutor{provider: "claude"})

	exhaustedHighPriority := weeklyAuth("manager-exhausted-high-priority", 24*time.Hour, 1)
	exhaustedHighPriority.Provider = "claude"
	exhaustedHighPriority.Attributes = map[string]string{"priority": "10"}
	usableLowerPriority := weeklyAuth("manager-usable-lower-priority", 72*time.Hour, 0.18)
	usableLowerPriority.Provider = "claude"
	usableLowerPriority.Attributes = map[string]string{"priority": "0"}
	registerSchedulerModels(t, "claude", model, exhaustedHighPriority.ID, usableLowerPriority.ID)

	if _, err := manager.Register(ctx, exhaustedHighPriority); err != nil {
		t.Fatalf("register exhaustedHighPriority auth: %v", err)
	}
	if _, err := manager.Register(ctx, usableLowerPriority); err != nil {
		t.Fatalf("register usableLowerPriority auth: %v", err)
	}

	got, _, err := manager.pickNext(ctx, "claude", model, cliproxyexecutor.Options{}, nil)
	if err != nil {
		t.Fatalf("pickNext() error = %v", err)
	}
	if got == nil {
		t.Fatalf("pickNext() auth = nil")
	}
	if got.ID != usableLowerPriority.ID {
		t.Fatalf("pickNext() = %q, want %q from lower-priority account with weekly quota", got.ID, usableLowerPriority.ID)
	}
}

// TestResetAwareSelector_ABCScenario verifies the soonest-resetting account wins among
// accounts with remaining weekly quota.
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

// TestResetAwareSelector_TieDelegatesToFallback verifies equal weekly reset times defer to
// the wrapped strategy.
func TestResetAwareSelector_TieDelegatesToFallback(t *testing.T) {
	t.Parallel()

	// Use a shared timestamp so the two auths have exactly equal reset times.
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

func TestResetAwareSelector_AllEqualWeeklyResetUsesRoundRobinFallback(t *testing.T) {
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

// TestResetAwareSelector_KnownBeatsUnknown verifies a credential with usable weekly quota is
// preferred over one without a weekly window (which is deferred to the fallback only when no
// known eligible window exists).
func TestResetAwareSelector_KnownBeatsUnknown(t *testing.T) {
	t.Parallel()

	known := weeklyAuth("known", 24*time.Hour, 0.10)
	unknown := &Auth{ID: "aaa-unknown"} // sorts first by ID, but has no weekly window

	got := pickResetAware(t, &FillFirstSelector{}, []*Auth{unknown, known})
	if got.ID != "known" {
		t.Fatalf("Pick() = %q, want %q", got.ID, "known")
	}
}

func TestResetAwareSelector_SkipsExhaustedWeeklyQuota(t *testing.T) {
	t.Parallel()

	exhausted := weeklyAuth("exhausted", 24*time.Hour, 1.0)
	open := weeklyAuth("open", 72*time.Hour, 0.99)

	got := pickResetAware(t, &FillFirstSelector{}, []*Auth{exhausted, open})
	if got.ID != "open" {
		t.Fatalf("Pick() = %q, want %q (weekly quota remaining)", got.ID, "open")
	}
}

// TestResetAwareSelector_SkipsLimitedUntil verifies an account whose representative window
// is currently rate-limited is skipped in favour of the next-best usable account.
func TestResetAwareSelector_SkipsLimitedUntil(t *testing.T) {
	t.Parallel()

	// "capped" has the soonest weekly reset but is rate-limited until later.
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
	// Even all-limited, the soonest weekly reset among them should be chosen.
	low := mk("low", 144*time.Hour, 0.10)
	high := mk("high", 24*time.Hour, 0.10)

	got := pickResetAware(t, &FillFirstSelector{}, []*Auth{low, high})
	if got.ID != "high" {
		t.Fatalf("Pick() = %q, want %q", got.ID, "high")
	}
}

// TestResetAwareSelector_WeeklyQuotaBeatsPriority verifies reset-aware ranking can select
// a lower-priority account when the higher-priority account has no weekly quota remaining.
func TestResetAwareSelector_WeeklyQuotaBeatsPriority(t *testing.T) {
	t.Parallel()

	// High priority, but weekly quota is exhausted.
	hiPri := weeklyAuth("hi", 24*time.Hour, 1)
	hiPri.Attributes = map[string]string{"priority": "10"}
	// Low priority, but it still has weekly quota.
	loPri := weeklyAuth("lo", 144*time.Hour, 0.10)
	loPri.Attributes = map[string]string{"priority": "0"}

	got := pickResetAware(t, &FillFirstSelector{}, []*Auth{loPri, hiPri})
	if got.ID != "lo" {
		t.Fatalf("Pick() = %q, want %q (weekly quota beats priority)", got.ID, "lo")
	}
}

// TestResetAwareSelector_ExcludesCooledAuth verifies reactively cooled auths are excluded.
func TestResetAwareSelector_ExcludesCooledAuth(t *testing.T) {
	t.Parallel()

	cooled := weeklyAuth("cooled", 24*time.Hour, 0.10) // soonest reset, but cooled
	cooled.Unavailable = true
	cooled.NextRetryAfter = time.Now().Add(time.Hour)
	cooled.Quota.Exceeded = true

	open := weeklyAuth("open", 72*time.Hour, 0.20)

	got := pickResetAware(t, &FillFirstSelector{}, []*Auth{cooled, open})
	if got.ID != "open" {
		t.Fatalf("Pick() = %q, want %q (cooled auth excluded)", got.ID, "open")
	}
}

func TestEffectiveWeeklyResetWithQuota_RollForwardStaleReset(t *testing.T) {
	t.Parallel()

	now := time.Now()
	// Reset 3 days in the past should roll forward to ~4 days in the future (7d window).
	auth := &Auth{Quota: QuotaState{WeeklyResetAt: now.Add(-3 * 24 * time.Hour), WeeklyUtilization: 0.5}}
	reset, ok := effectiveWeeklyResetWithQuota(auth, now)
	if !ok {
		t.Fatalf("effectiveWeeklyResetWithQuota() ok = false, want true (stale reset should roll forward)")
	}
	if !reset.After(now) {
		t.Fatalf("effectiveWeeklyResetWithQuota() reset = %v, want after %v", reset, now)
	}
}

func TestEffectiveWeeklyResetWithQuota_Unknown(t *testing.T) {
	t.Parallel()

	if _, ok := effectiveWeeklyResetWithQuota(&Auth{}, time.Now()); ok {
		t.Fatalf("effectiveWeeklyResetWithQuota() ok = true for zero reset, want false")
	}
	if _, ok := effectiveWeeklyResetWithQuota(nil, time.Now()); ok {
		t.Fatalf("effectiveWeeklyResetWithQuota(nil) ok = true, want false")
	}
}
