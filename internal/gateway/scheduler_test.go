package gateway

import (
	"sync"
	"testing"
	"time"

	"github.com/sunboyzoey/GPT-BPS/internal/auth"
)

// fakeStore builds an auth.Store whose token never expires (zero expiry).
func fakeStore(t *testing.T) *auth.Store {
	t.Helper()
	st, err := auth.Load(fakeAuthFile(t, "sk-test-token", "acct-local"))
	if err != nil {
		t.Fatalf("fake store: %v", err)
	}
	return st
}

func newTestRuntime(t *testing.T, id string, weight int) *Runtime {
	t.Helper()
	rt := &Runtime{ID: id, Name: "n-" + id, Store: fakeStore(t), weight: weight}
	rt.Enabled.Store(true)
	return rt
}

func TestPickWeightedLeastActive(t *testing.T) {
	s := NewScheduler()
	a := newTestRuntime(t, "acct_a", 1)
	b := newTestRuntime(t, "acct_b", 3)
	s.Upsert(a)
	s.Upsert(b)

	first, _ := s.Pick("")
	if first.ID != "acct_a" && first.ID != "acct_b" {
		t.Fatalf("unexpected pick %s", first.ID)
	}
	// Fill a to active=1: score a=1, b=0 -> next must be b.
	second, _ := s.Pick("")
	if second.ID == first.ID {
		t.Fatalf("least-active violated: both picks %s", first.ID)
	}
	// Weight 3 account b should absorb more traffic. Drive 6 picks, count.
	counts := map[string]int{"acct_a": 0, "acct_b": 0}
	for i := 0; i < 6; i++ {
		rt, err := s.Pick("")
		if err != nil {
			t.Fatalf("pick: %v", err)
		}
		counts[rt.ID]++
		defer s.Release(rt)
	}
	if counts["acct_b"] < counts["acct_a"] {
		t.Fatalf("weight-3 account got fewer picks: %+v", counts)
	}
}

func TestPickExcludesDisabledCooldownAuthRequired(t *testing.T) {
	s := NewScheduler()
	a := newTestRuntime(t, "acct_a", 1)
	b := newTestRuntime(t, "acct_b", 1)
	s.Upsert(a)
	s.Upsert(b)

	a.Enabled.Store(false)
	rt, err := s.Pick("")
	if err != nil || rt.ID != "acct_b" {
		t.Fatalf("disabled still picked: %v %v", rt, err)
	}
	s.Release(rt)

	b.CooldownUntil.Store(time.Now().Add(time.Hour).UnixMilli())
	if _, err := s.Pick(""); err == nil {
		t.Fatal("all accounts unavailable must error")
	}
	s.MarkSuccess(b) // clear? MarkSuccess clears cooldown only if lastErrorCode set; force:
	b.CooldownUntil.Store(0)
	rt, err = s.Pick("")
	if err != nil || rt.ID != "acct_b" {
		t.Fatalf("cooldown not respected: %v %v", rt, err)
	}
	s.Release(rt)

	b.AuthRequired.Store(true)
	a.Enabled.Store(true)
	rt, err = s.Pick("")
	if err != nil || rt.ID != "acct_a" {
		t.Fatalf("auth-required still picked: %v %v", rt, err)
	}
	s.Release(rt)
}

func TestPickStickySession(t *testing.T) {
	s := NewScheduler()
	a := newTestRuntime(t, "acct_a", 1)
	b := newTestRuntime(t, "acct_b", 1)
	s.Upsert(a)
	s.Upsert(b)
	first, _ := s.Pick("session-1")
	for i := 0; i < 5; i++ {
		next, err := s.Pick("session-1")
		if err != nil {
			t.Fatalf("pick: %v", err)
		}
		if next.ID != first.ID {
			t.Fatalf("sticky broken: %s vs %s", first.ID, next.ID)
		}
	}
	// Sticky account disabled -> falls back to another.
	a.Enabled.Store(false)
	next, _ := s.Pick("session-1")
	if next.ID != "acct_b" {
		t.Fatalf("sticky fallback failed: %s", next.ID)
	}
}

func TestNetworkFailureBackoff(t *testing.T) {
	s := NewScheduler()
	a := newTestRuntime(t, "acct_a", 1)
	s.Upsert(a)
	if !s.MarkNetworkFailure(a, "basispoints_transport_error") {
		t.Fatal("mark should report change")
	}
	if a.CooldownUntil.Load() <= time.Now().UnixMilli() {
		t.Fatal("cooldown not applied")
	}
	if _, err := s.Pick(""); err == nil {
		t.Fatal("cooling account must not be picked")
	}
	a.CooldownUntil.Store(0)
	if !s.MarkSuccess(a) {
		t.Fatal("success after clearing cooldown must sync error state")
	}
	if a.lastErrorCode != "" || a.CooldownUntil.Load() != 0 {
		t.Fatal("success must clear error state")
	}

	// A runtime that never failed reports no change on success.
	b := newTestRuntime(t, "acct_clean", 1)
	s.Upsert(b)
	if s.MarkSuccess(b) {
		t.Fatal("success on clean account must not report change")
	}
}

func TestAuthFailureStopsScheduling(t *testing.T) {
	s := NewScheduler()
	a := newTestRuntime(t, "acct_a", 1)
	s.Upsert(a)
	s.MarkAuthFailure(a, "basispoints_auth_error")
	if _, err := s.Pick(""); err == nil {
		t.Fatal("auth-required account must not be picked")
	}
	s.ResetAuth(a.ID)
	rt, err := s.Pick("")
	if err != nil {
		t.Fatalf("reset failed: %v", err)
	}
	s.Release(rt)
}

func TestReleaseDecrements(t *testing.T) {
	s := NewScheduler()
	a := newTestRuntime(t, "acct_a", 1)
	s.Upsert(a)
	rt, _ := s.Pick("")
	if rt.Active.Load() != 1 {
		t.Fatalf("active = %d", rt.Active.Load())
	}
	s.Release(rt)
	if rt.Active.Load() != 0 {
		t.Fatalf("active after release = %d", rt.Active.Load())
	}
}

func TestSchedulerConcurrentPicks(t *testing.T) {
	s := NewScheduler()
	a := newTestRuntime(t, "acct_a", 1)
	b := newTestRuntime(t, "acct_b", 1)
	s.Upsert(a)
	s.Upsert(b)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				rt, err := s.Pick("")
				if err != nil {
					t.Errorf("pick: %v", err)
					return
				}
				s.Release(rt)
			}
		}()
	}
	wg.Wait()
	if a.Active.Load() != 0 || b.Active.Load() != 0 {
		t.Fatalf("leaked active counts: %d %d", a.Active.Load(), b.Active.Load())
	}
}
