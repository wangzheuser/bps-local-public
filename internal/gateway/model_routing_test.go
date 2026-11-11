package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newCapScheduler builds a scheduler with the given account capabilities.
func newCapScheduler(t *testing.T, caps map[string]map[string]bool) *Scheduler {
	t.Helper()
	// Every runtime needs a credential store because eligibility reads expiry.
	st := fakeStore(t)
	s := NewScheduler()
	for id, serves := range caps {
		rt := &Runtime{ID: id, Name: id, Store: st}
		rt.Enabled.Store(true)
		rt.weight = 1
		if serves != nil {
			rt.SetServes(serves)
		}
		s.Upsert(rt)
	}
	return s
}

// With capability data, only accounts measured as serving the model are used.
func TestPickForModelRestrictsToVerified(t *testing.T) {
	s := newCapScheduler(t, map[string]map[string]bool{
		"good": {"gpt-5.6-sol": true, "gpt-5.6-luna": true},
		"bad":  {"gpt-5.6-sol": false, "gpt-5.6-luna": true},
	})
	for i := 0; i < 20; i++ {
		rt, err := s.PickForModel("", "gpt-5.6-sol")
		if err != nil {
			t.Fatalf("pick %d: %v", i, err)
		}
		if rt.ID != "good" {
			t.Fatalf("routed gpt-5.6-sol to %s, want good", rt.ID)
		}
		s.Release(rt)
	}
}

// A model nobody was measured to serve fails explicitly instead of degrading.
func TestPickForModelUnsupported(t *testing.T) {
	s := newCapScheduler(t, map[string]map[string]bool{
		"a": {"gpt-6-astra": false},
		"b": {"gpt-6-astra": false},
	})
	if _, err := s.PickForModel("", "gpt-6-astra"); err != ErrModelUnsupported {
		t.Fatalf("err = %v, want ErrModelUnsupported", err)
	}
}

// Without capability data the model is unconstrained, so a fresh install works.
func TestPickForModelUnconstrainedWithoutData(t *testing.T) {
	s := newCapScheduler(t, map[string]map[string]bool{
		"a": nil,
		"b": nil,
	})
	seen := map[string]bool{}
	for i := 0; i < 10; i++ {
		rt, err := s.PickForModel("", "gpt-5.6-sol")
		if err != nil {
			t.Fatalf("pick: %v", err)
		}
		seen[rt.ID] = true
		s.Release(rt)
	}
	if len(seen) != 2 {
		t.Fatalf("expected both accounts eligible, saw %v", seen)
	}
}

// An account with no row for the model is skipped once another account proved
// it can serve it.
func TestPickForModelSkipsAccountWithoutData(t *testing.T) {
	s := newCapScheduler(t, map[string]map[string]bool{
		"known":   {"gpt-5.6-sol": true},
		"unknown": {"gpt-5.6-luna": true}, // has data, but not for sol
	})
	for i := 0; i < 10; i++ {
		rt, err := s.PickForModel("", "gpt-5.6-sol")
		if err != nil {
			t.Fatalf("pick: %v", err)
		}
		if rt.ID != "known" {
			t.Fatalf("routed to %s, want known", rt.ID)
		}
		s.Release(rt)
	}
}

// An empty model keeps the old behaviour: any eligible account.
func TestPickForModelEmptyModelIsUnconstrained(t *testing.T) {
	s := newCapScheduler(t, map[string]map[string]bool{
		"a": {"gpt-5.6-sol": false},
	})
	rt, err := s.PickForModel("", "")
	if err != nil {
		t.Fatalf("pick: %v", err)
	}
	if rt.ID != "a" {
		t.Fatalf("got %s", rt.ID)
	}
	s.Release(rt)
}

// The HTTP layer must answer 409 when a model has no supporting account.
func TestGatewayModelNotServedReturns409(t *testing.T) {
	up := fakeSSEUpstream(t, sseCompleted, "", 0)
	defer up.Close()
	gw, sched, key := buildTestGateway(t, up.URL, "")
	rt := sched.Get("acct_A")
	rt.SetServes(map[string]bool{"gpt-5.6-luna": true})

	body := `{"model":"gpt-6-astra","input":"hi","stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	gw.APIHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "model_not_served") {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

// A served model still reaches the upstream.
func TestGatewayServedModelPasses(t *testing.T) {
	up := fakeSSEUpstream(t, sseCompleted, "", 0)
	defer up.Close()
	gw, sched, key := buildTestGateway(t, up.URL, "")
	sched.Get("acct_A").SetServes(map[string]bool{"gpt-5.6-luna": true})

	body := `{"model":"gpt-5.6-luna","input":"hi","stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	gw.APIHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
}

