package auth

import (
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestApplyRateLimitSnapshot_PersistsWindows(t *testing.T) {
	t.Parallel()

	now := time.Now()
	auth := &Auth{ID: "claude-a"}
	snap := &cliproxyexecutor.RateLimitSnapshot{
		Status:              "allowed",
		WeeklyResetAt:       now.Add(48 * time.Hour),
		WeeklyUtilization:   0.31,
		FiveHourResetAt:     now.Add(2 * time.Hour),
		FiveHourUtilization: 0.93,
	}

	applyRateLimitSnapshot(auth, snap, now)

	if !auth.Quota.WeeklyResetAt.Equal(snap.WeeklyResetAt) {
		t.Fatalf("WeeklyResetAt = %v, want %v", auth.Quota.WeeklyResetAt, snap.WeeklyResetAt)
	}
	if auth.Quota.WeeklyUtilization != 0.31 {
		t.Fatalf("WeeklyUtilization = %v, want 0.31", auth.Quota.WeeklyUtilization)
	}
	if auth.Quota.UnifiedStatus != "allowed" {
		t.Fatalf("UnifiedStatus = %q, want allowed", auth.Quota.UnifiedStatus)
	}
	if !auth.Quota.LimitedUntil.IsZero() {
		t.Fatalf("LimitedUntil = %v, want zero when status=allowed", auth.Quota.LimitedUntil)
	}
}

func TestApplyRateLimitSnapshot_RateLimitedSetsLimitedUntil(t *testing.T) {
	t.Parallel()

	now := time.Now()
	fiveHourReset := now.Add(2 * time.Hour)
	weeklyReset := now.Add(72 * time.Hour)

	t.Run("five_hour representative", func(t *testing.T) {
		auth := &Auth{ID: "a"}
		snap := &cliproxyexecutor.RateLimitSnapshot{
			Status:               "rate_limited",
			RepresentativeWindow: "five_hour",
			FiveHourResetAt:      fiveHourReset,
			WeeklyResetAt:        weeklyReset,
		}
		applyRateLimitSnapshot(auth, snap, now)
		if !auth.Quota.LimitedUntil.Equal(fiveHourReset) {
			t.Fatalf("LimitedUntil = %v, want 5h reset %v", auth.Quota.LimitedUntil, fiveHourReset)
		}
	})

	t.Run("seven_day representative", func(t *testing.T) {
		auth := &Auth{ID: "a"}
		snap := &cliproxyexecutor.RateLimitSnapshot{
			Status:               "rate_limited",
			RepresentativeWindow: "seven_day",
			FiveHourResetAt:      fiveHourReset,
			WeeklyResetAt:        weeklyReset,
		}
		applyRateLimitSnapshot(auth, snap, now)
		if !auth.Quota.LimitedUntil.Equal(weeklyReset) {
			t.Fatalf("LimitedUntil = %v, want weekly reset %v", auth.Quota.LimitedUntil, weeklyReset)
		}
	})
}

func TestApplyRateLimitSnapshot_ClearsLimitedUntilWhenAllowed(t *testing.T) {
	t.Parallel()

	now := time.Now()
	auth := &Auth{ID: "a", Quota: QuotaState{LimitedUntil: now.Add(time.Hour)}}
	applyRateLimitSnapshot(auth, &cliproxyexecutor.RateLimitSnapshot{Status: "allowed", WeeklyResetAt: now.Add(time.Hour)}, now)
	if !auth.Quota.LimitedUntil.IsZero() {
		t.Fatalf("LimitedUntil = %v, want cleared on allowed status", auth.Quota.LimitedUntil)
	}
}

func TestApplyRateLimitSnapshot_NilIsNoop(t *testing.T) {
	t.Parallel()

	auth := &Auth{ID: "a"}
	applyRateLimitSnapshot(auth, nil, time.Now())
	if !auth.Quota.WeeklyResetAt.IsZero() {
		t.Fatalf("WeeklyResetAt mutated on nil snapshot")
	}
}
