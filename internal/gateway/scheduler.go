package gateway

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sync"
	"time"
)

// ErrNoAccount is returned when no account is eligible for scheduling.
var ErrNoAccount = errors.New("no_account_available")

const (
	stickyTTL    = 15 * time.Minute
	stickyMax    = 10000
	maxBackoff   = 5 * time.Minute
	baseBackoff  = 5 * time.Second
)

// Scheduler picks accounts with weighted least-active selection, optional
// session stickiness and failure cooldowns.
type Scheduler struct {
	mu       sync.Mutex
	runtimes map[string]*Runtime
	order    []string
	rr       uint64
	sticky   map[string]stickyEntry
	hmacKey  []byte
	now      func() time.Time
}

type stickyEntry struct {
	accountID string
	expires   time.Time
}

// NewScheduler builds an empty scheduler.
func NewScheduler() *Scheduler {
	key := make([]byte, 32)
	// The sticky key only pseudonymizes session ids in memory; randomness
	// quality is not security-critical here, but crypto/rand is cheap.
	_, _ = readRandom(key)
	return &Scheduler{
		runtimes: make(map[string]*Runtime),
		sticky:   make(map[string]stickyEntry),
		hmacKey:  key,
		now:      time.Now,
	}
}

// Upsert adds or replaces a runtime. A replaced runtime keeps serving in-flight
// requests because the caller keeps its own reference until drained.
func (s *Scheduler) Upsert(r *Runtime) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.runtimes[r.ID]; !exists {
		s.order = append(s.order, r.ID)
	}
	s.runtimes[r.ID] = r
}

// Remove drops a runtime from scheduling.
func (s *Scheduler) Remove(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.runtimes, id)
	for i, id2 := range s.order {
		if id2 == id {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
}

// Get returns the runtime with the given id, or nil.
func (s *Scheduler) Get(id string) *Runtime {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.runtimes[id]
}

// HasRuntime reports whether a runtime is loaded.
func (s *Scheduler) HasRuntime(id string) bool { return s.Get(id) != nil }

// SetEnabled toggles the enabled flag.
func (s *Scheduler) SetEnabled(id string, enabled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := s.runtimes[id]; r != nil {
		r.Enabled.Store(enabled)
		if enabled {
			r.consecutiveFailures = 0
		}
	}
}

// SetWeight updates the weight with bounds.
func (s *Scheduler) SetWeight(id string, weight int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := s.runtimes[id]; r != nil {
		r.weight = weight
	}
}

func (s *Scheduler) eligible(r *Runtime, now time.Time) bool {
	if !r.Enabled.Load() || r.AuthRequired.Load() {
		return false
	}
	if r.CooldownUntil.Load() > now.UnixMilli() {
		return false
	}
	if exp := r.Store.Expiry(); !exp.IsZero() && !exp.After(now) {
		return false
	}
	return true
}

// ErrModelUnsupported reports that capability data exists for the model but no
// eligible account was measured as really serving it. Returning this beats
// silently routing to an account that would downgrade the request.
var ErrModelUnsupported = errors.New("model_not_served_by_any_account")

// Pick selects the next account and increments its in-flight counter. The
// caller must Release when the request finishes.
//
// When model is non-empty and some account carries capability data for it, only
// accounts measured as serving that model are considered. Without capability
// data the model is unconstrained, so a fresh install keeps working.
func (s *Scheduler) Pick(sessionID string) (*Runtime, error) {
	return s.PickForModel(sessionID, "")
}

// PickForModel is Pick with a model constraint. An empty model means "any".
func (s *Scheduler) PickForModel(sessionID, model string) (*Runtime, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()

	// Decide first whether the model is constrained at all.
	constrained := false
	if model != "" {
		for _, r := range s.runtimes {
			if _, hasData := r.Serves(model); hasData {
				constrained = true
				break
			}
		}
	}
	allowed := func(r *Runtime) bool {
		if !constrained {
			return true
		}
		serves, hasData := r.Serves(model)
		// An account with no data for this model is not a safe target once
		// another account proved it can serve it.
		return hasData && serves
	}

	if sessionID != "" {
		key := s.stickyKey(sessionID)
		if e, ok := s.sticky[key]; ok && e.expires.After(now) {
			if r := s.runtimes[e.accountID]; r != nil && s.eligible(r, now) && allowed(r) {
				r.Active.Add(1)
				return r, nil
			}
		}
	}

	var best *Runtime
	bestScore := 0.0
	var bestOrder int
	for i, id := range s.order {
		r := s.runtimes[id]
		if r == nil || !s.eligible(r, now) || !allowed(r) {
			continue
		}
		score := float64(r.Active.Load()) / float64(maxWeight(r))
		if best == nil || score < bestScore {
			best, bestScore, bestOrder = r, score, i
		}
	}
	if best == nil {
		if constrained {
			return nil, ErrModelUnsupported
		}
		return nil, ErrNoAccount
	}
	// Tie-break deterministically across equal scores using a rotating start
	// offset so equal-weight idle accounts still alternate.
	if s.hasEqualScoreFiltered(s.order, bestScore, now, allowed) {
		s.rr++
		offset := int(s.rr % uint64(len(s.order)))
		for i := 0; i < len(s.order); i++ {
			r := s.runtimes[s.order[(i+offset)%len(s.order)]]
			if r != nil && s.eligible(r, now) && allowed(r) &&
				float64(r.Active.Load())/float64(maxWeight(r)) == bestScore {
				best = r
				break
			}
		}
	}
	_ = bestOrder
	best.Active.Add(1)
	if sessionID != "" {
		s.pruneStickyLocked(now)
		if len(s.sticky) < stickyMax {
			s.sticky[s.stickyKey(sessionID)] = stickyEntry{accountID: best.ID, expires: now.Add(stickyTTL)}
		}
	}
	return best, nil
}

func maxWeight(r *Runtime) int {
	if r.weight < 1 {
		return 1
	}
	return r.weight
}

func (s *Scheduler) hasEqualScore(order []string, score float64, now time.Time) bool {
	return s.hasEqualScoreFiltered(order, score, now, func(*Runtime) bool { return true })
}

// hasEqualScoreFiltered is hasEqualScore restricted to accounts the model
// constraint allows, so tie-breaking cannot drift onto an ineligible account.
func (s *Scheduler) hasEqualScoreFiltered(order []string, score float64, now time.Time, allowed func(*Runtime) bool) bool {
	count := 0
	for _, id := range order {
		r := s.runtimes[id]
		if r == nil || !s.eligible(r, now) || !allowed(r) {
			continue
		}
		if float64(r.Active.Load())/float64(maxWeight(r)) == score {
			count++
			if count > 1 {
				return true
			}
		}
	}
	return false
}

// Release decrements the in-flight counter.
func (s *Scheduler) Release(r *Runtime) { r.Active.Add(-1) }

// MarkSuccess resets failure state. It reports whether persistent error state
// was cleared so the caller can sync the database.
func (s *Scheduler) MarkSuccess(r *Runtime) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := r.lastErrorCode != "" || r.CooldownUntil.Load() != 0
	r.consecutiveFailures = 0
	r.lastErrorCode = ""
	r.CooldownUntil.Store(0)
	return changed
}

// MarkNetworkFailure applies exponential cooldown. It always reports changed.
func (s *Scheduler) MarkNetworkFailure(r *Runtime, code string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	r.consecutiveFailures++
	shift := r.consecutiveFailures
	if shift > 10 {
		shift = 10
	}
	backoff := baseBackoff << uint(shift-1)
	if backoff > maxBackoff || backoff <= 0 {
		backoff = maxBackoff
	}
	until := s.now().Add(backoff).UnixMilli()
	if prev := r.CooldownUntil.Load(); until < prev {
		until = prev
	}
	r.CooldownUntil.Store(until)
	r.lastErrorCode = code
	return true
}

// MarkAuthFailure removes the account from scheduling until reauthorization.
func (s *Scheduler) MarkAuthFailure(r *Runtime, code string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	r.AuthRequired.Store(true)
	r.lastErrorCode = code
	return true
}

// MarkCooldown applies a bounded server-provided wait.
func (s *Scheduler) MarkCooldown(r *Runtime, wait time.Duration, code string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if wait <= 0 {
		wait = baseBackoff
	}
	if wait > maxBackoff {
		wait = maxBackoff
	}
	until := s.now().Add(wait).UnixMilli()
	if prev := r.CooldownUntil.Load(); until < prev {
		until = prev
	}
	r.CooldownUntil.Store(until)
	r.lastErrorCode = code
	return true
}

// ResetAuth clears the auth-required flag after reauthorization or a manual
// enable with fresh credentials.
func (s *Scheduler) ResetAuth(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := s.runtimes[id]; r != nil {
		r.AuthRequired.Store(false)
		r.consecutiveFailures = 0
		r.CooldownUntil.Store(0)
		r.lastErrorCode = ""
	}
}

// Snapshot returns the current admin view of every runtime.
func (s *Scheduler) Snapshot() []RuntimeInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]RuntimeInfo, 0, len(s.order))
	for _, id := range s.order {
		r := s.runtimes[id]
		if r == nil {
			continue
		}
		out = append(out, RuntimeInfo{
			ID:             r.ID,
			Name:           r.Name,
			Enabled:        r.Enabled.Load(),
			Weight:         r.weight,
			ActiveRequests: r.Active.Load(),
			AuthRequired:   r.AuthRequired.Load(),
			CooldownUntil:  r.CooldownUntil.Load(),
			LastErrorCode:  r.lastErrorCode,
		})
	}
	return out
}

// EligibleCount reports how many accounts could serve a request right now.
func (s *Scheduler) EligibleCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	n := 0
	for _, id := range s.order {
		if r := s.runtimes[id]; r != nil && s.eligible(r, now) {
			n++
		}
	}
	return n
}

func (s *Scheduler) stickyKey(sessionID string) string {
	mac := hmac.New(sha256.New, s.hmacKey)
	mac.Write([]byte(sessionID))
	return hex.EncodeToString(mac.Sum(nil))
}

func (s *Scheduler) pruneStickyLocked(now time.Time) {
	for k, e := range s.sticky {
		if !e.expires.After(now) {
			delete(s.sticky, k)
		}
	}
}
