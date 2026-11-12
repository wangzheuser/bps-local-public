// Package probe measures which model each account really serves, using the
// ModelTrace fingerprint challenges.
//
// Upstream plans silently route several model names onto one backend, so the
// model field in a response proves nothing. Probing each account/model pair and
// recording the attribution lets the scheduler avoid accounts that downgrade.
package probe

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sunboyzoey/GPT-BPS/internal/gateway"
	"github.com/sunboyzoey/GPT-BPS/internal/modeltrace"
	"github.com/sunboyzoey/GPT-BPS/internal/store"
)

// Status is the observable state of one probe run.
type Status struct {
	Running   bool   `json:"running"`
	Total     int    `json:"total"`
	Done      int    `json:"done"`
	Failed    int    `json:"failed"`
	StartedAt int64  `json:"started_at"`
	FinishedAt int64 `json:"finished_at"`
	LastError string `json:"last_error"`
}

// Service runs attribution probes on demand and on a schedule.
type Service struct {
	db      *store.DB
	sched   *gateway.Scheduler
	bankPath string
	now     func() time.Time

	mu     sync.Mutex
	status Status
	cancel context.CancelFunc
	// singles tracks account/model pairs owned by a single-cell probe so a
	// concurrent full run skips them instead of measuring twice.
	singles map[singleKey]bool
}

// NewService builds the probe service. An empty bankPath or a missing bank
// leaves probing disabled; the rest of the server keeps working.
func NewService(db *store.DB, sched *gateway.Scheduler, bankPath string) *Service {
	if bankPath != "" {
		modeltrace.SetBankPath(bankPath)
	}
	return &Service{db: db, sched: sched, bankPath: bankPath, now: time.Now}
}

// Available reports whether a usable fingerprint bank is loaded.
func (s *Service) Available() (bool, error) {
	_, err := modeltrace.Load()
	return err == nil, err
}

// Status returns a snapshot of the current or last run.
func (s *Service) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status
}

// Start launches a full probe run in the background. It returns immediately;
// callers poll Status. A second call while running is a no-op returning the
// current status.
func (s *Service) Start() (Status, error) {
	if _, err := modeltrace.Load(); err != nil {
		return s.Status(), err
	}
	accounts, err := s.db.ListAccounts()
	if err != nil {
		return s.Status(), err
	}
	models, err := s.targetModels()
	if err != nil {
		return s.Status(), err
	}
	if len(accounts) == 0 || len(models) == 0 {
		return s.Status(), fmt.Errorf("probe: no accounts or models to test")
	}

	s.mu.Lock()
	if s.status.Running {
		st := s.status
		s.mu.Unlock()
		return st, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.status = Status{
		Running:   true,
		Total:     len(accounts) * len(models),
		StartedAt: s.now().UnixMilli(),
	}
	s.mu.Unlock()

	go s.run(ctx, accounts, models)
	return s.Status(), nil
}

// Stop cancels a running probe.
func (s *Service) Stop() {
	s.mu.Lock()
	cancel := s.cancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// targetModels prefers the synced catalog; without it probing has nothing to
// compare against.
func (s *Service) targetModels() ([]string, error) {
	cat, err := s.db.GetModelCatalog()
	if err != nil {
		return nil, fmt.Errorf("probe: model catalog unavailable, sync it first: %w", err)
	}
	var doc struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(cat.Payload, &doc); err != nil {
		return nil, fmt.Errorf("probe: catalog is corrupt: %w", err)
	}
	out := make([]string, 0, len(doc.Data))
	for _, m := range doc.Data {
		if id := strings.TrimSpace(m.ID); id != "" {
			out = append(out, id)
		}
	}
	return out, nil
}

// run probes accounts with limited concurrency. Upstream rejects a burst of
// long generations, so the number of accounts probed at once is capped and each
// account stays serial internally.
func (s *Service) run(ctx context.Context, accounts []*store.Account, models []string) {
	bank, err := modeltrace.Load()
	if err != nil {
		s.finish(err)
		return
	}

	limit := s.concurrency()
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	for _, a := range accounts {
		wg.Add(1)
		go func(a *store.Account) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			s.probeAccount(ctx, bank, a, models)
		}(a)
	}
	wg.Wait()

	if err := s.refreshCapabilities(); err != nil {
		s.finish(err)
		return
	}
	_ = s.db.SetProbeLastRun(s.now().UnixMilli())
	s.finish(nil)
}

// concurrency caps how many accounts are probed in parallel. Probing sends long
// generations, and the upstream throttles bursts, so the default is one.
func (s *Service) concurrency() int {
	if v := os.Getenv("BPS_PROBE_CONCURRENCY"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 && n <= 8 {
			return n
		}
	}
	return 1
}

func (s *Service) finish(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status.Running = false
	s.status.FinishedAt = s.now().UnixMilli()
	if err != nil {
		s.status.LastError = err.Error()
	}
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
}

// ProbeOne measures a single account/model pair synchronously and returns the
// stored row. It exists so the console can re-check one cell without waiting for
// a full run, which can take many minutes.
//
// A full run holds the same per-account serialisation guarantee; a single probe
// takes the run mutex only long enough to reserve the pair, so a concurrent full
// run cannot measure the same account twice at once.
func (s *Service) ProbeOne(ctx context.Context, accountID, model string) (store.AccountModel, error) {
	accountID = strings.TrimSpace(accountID)
	model = strings.TrimSpace(model)
	if accountID == "" || model == "" {
		return store.AccountModel{}, fmt.Errorf("probe: account and model are required")
	}
	bank, err := modeltrace.Load()
	if err != nil {
		return store.AccountModel{}, err
	}
	acc, err := s.db.GetAccount(accountID)
	if err != nil {
		return store.AccountModel{}, fmt.Errorf("probe: account not found: %w", err)
	}
	rt := s.sched.Get(accountID)
	if rt == nil {
		return store.AccountModel{}, fmt.Errorf("probe: account runtime is not loaded")
	}

	if err := s.beginSingle(accountID, model); err != nil {
		return store.AccountModel{}, err
	}
	defer s.endSingle(accountID, model)

	row := s.probePair(ctx, bank, acc, rt, model)
	return row, nil
}

// singleKey identifies one reserved account/model pair.
type singleKey struct{ account, model string }

// beginSingle reserves a pair so a scheduled run skips it, and rejects a second
// concurrent probe of the same pair.
func (s *Service) beginSingle(accountID, model string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.singles == nil {
		s.singles = make(map[singleKey]bool)
	}
	k := singleKey{accountID, model}
	if s.singles[k] {
		return fmt.Errorf("probe: this account/model is already being probed")
	}
	s.singles[k] = true
	return nil
}

func (s *Service) endSingle(accountID, model string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.singles, singleKey{accountID, model})
}

// probePair runs every challenge for one account/model pair, attributes the
// answers and persists the row. The caller owns any run accounting.
func (s *Service) probePair(ctx context.Context, bank *modeltrace.Bank, a *store.Account, rt *gateway.Runtime, model string) store.AccountModel {
	challenges := modeltrace.Challenges(s.ChallengeCount())
	answers := make([]modeltrace.Answer, 0, len(challenges))
	var genErr string
	for _, ch := range challenges {
		if ctx.Err() != nil {
			genErr = ctx.Err().Error()
			break
		}
		text, err := runTestWithRetry(ctx, rt, model, ch.Prompt, probeAttempts)
		if err != nil {
			genErr = err.Error()
			break
		}
		answers = append(answers, modeltrace.Answer{Expected: ch.Expected, Text: text})
	}
	row := store.AccountModel{AccountID: a.ID, Model: model, CheckedAt: s.now().UnixMilli()}
	if genErr != "" {
		row.LastError = genErr
	} else if res, err := bank.Attribute(model, answers); err != nil {
		row.LastError = err.Error()
	} else {
		row.TopModel = res.TopModel
		row.SelfProb = res.SelfProb
		row.Verified = res.Match
	}
	if err := s.db.UpsertAccountModels([]store.AccountModel{row}); err != nil {
		row.LastError = err.Error()
	}
	// Publish immediately so routing reflects the measurement.
	s.publishAccount(a.ID)
	return row
}

// probeAccount runs every challenge for every model of one account.
func (s *Service) probeAccount(ctx context.Context, bank *modeltrace.Bank, a *store.Account, models []string) {
	rt := s.sched.Get(a.ID)
	if rt == nil {
		s.recordBatch(a.ID, models, "runtime not loaded")
		return
	}
	for _, model := range models {
		if ctx.Err() != nil {
			return
		}
		if s.reserved(a.ID, model) {
			continue
		}
		row := s.probePair(ctx, bank, a, rt, model)
		s.bump(row.LastError != "")
	}
}

// reserved reports whether a single-cell probe currently owns this pair.
func (s *Service) reserved(accountID, model string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.singles[singleKey{accountID, model}]
}

// publishAccount reloads this account's capability set into its live runtime.
func (s *Service) publishAccount(accountID string) {
	rows, err := s.db.ListAccountModels()
	if err != nil {
		return
	}
	set := make(map[string]bool)
	for _, r := range rows {
		if r.AccountID == accountID {
			set[r.Model] = r.Verified
		}
	}
	rt := s.sched.Get(accountID)
	if rt == nil || len(set) == 0 {
		return
	}
	rt.SetServes(set)
}

// recordBatch marks every model of an account as failed for one reason.
func (s *Service) recordBatch(accountID string, models []string, reason string) {
	now := s.now().UnixMilli()
	rows := make([]store.AccountModel, 0, len(models))
	for _, m := range models {
		rows = append(rows, store.AccountModel{
			AccountID: accountID, Model: m, CheckedAt: now, LastError: reason,
		})
	}
	_ = s.db.UpsertAccountModels(rows)
	for range models {
		s.bump(true)
	}
}

func (s *Service) bump(failed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status.Done++
	if failed {
		s.status.Failed++
	}
}

// LoadStoredCapabilities pushes the persisted capability set into every live
// runtime. Called at startup so routing survives a restart without re-probing.
func (s *Service) LoadStoredCapabilities() error {
	return s.refreshCapabilities()
}

// refreshCapabilities pushes the stored verified set into each runtime so
// routing picks it up without a restart.
func (s *Service) refreshCapabilities() error {
	rows, err := s.db.ListAccountModels()
	if err != nil {
		return err
	}
	byAccount := make(map[string]map[string]bool)
	for _, r := range rows {
		set := byAccount[r.AccountID]
		if set == nil {
			set = make(map[string]bool)
			byAccount[r.AccountID] = set
		}
		// Only a measured hit counts; a downgrade must not mark the account as
		// serving the model.
		set[r.Model] = r.Verified
	}
	// Iterate the live runtimes, not the info snapshot: SetServes mutates the
	// runtime object itself.
	for _, info := range s.sched.Snapshot() {
		rt := s.sched.Get(info.ID)
		if rt == nil {
			continue
		}
		if set, ok := byAccount[info.ID]; ok {
			rt.SetServes(set)
			continue
		}
		rt.SetServes(nil)
	}
	return nil
}

// runTest drives one generation through the account runtime, reusing the same
// path real traffic takes.
func runTest(ctx context.Context, rt *gateway.Runtime, model, prompt string) (string, error) {
	res, err := rt.Generate(ctx, model, prompt, "")
	if err != nil {
		return "", err
	}
	return res.Text, nil
}

// probeAttempts is how many times one challenge is retried. Upstream overload
// surfaces as a transient error, so a short retry recovers most of them.
const probeAttempts = 3

// runTestWithRetry retries a failed challenge with a growing pause. It gives up
// on cancellation so stopping a run does not wait for the retries.
func runTestWithRetry(ctx context.Context, rt *gateway.Runtime, model, prompt string, attempts int) (string, error) {
	if attempts < 1 {
		attempts = 1
	}
	var lastErr error
	for i := 0; i < attempts; i++ {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		text, err := runTest(ctx, rt, model, prompt)
		if err == nil {
			return text, nil
		}
		lastErr = err
		if i == attempts-1 {
			break
		}
		// Back off before the next attempt: 5s, then 10s.
		wait := time.Duration(5*(i+1)) * time.Second
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(wait):
		}
	}
	return "", lastErr
}


// ChallengeCount reports how many probes each model gets, defaulting to the
// bank's recommended 3 when unset.
func (s *Service) ChallengeCount() int {
	_, _, n, err := s.db.GetProbeSchedule()
	if err != nil || n < 1 {
		return store.DefaultProbeChallenges
	}
	if n > modeltrace.MaxChallenges {
		return modeltrace.MaxChallenges
	}
	return n
}

// RunScheduler checks the configured interval and starts a probe when one is
// due. It blocks until ctx is done and is meant to run in its own goroutine.
func (s *Service) RunScheduler(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.maybeStartScheduled()
		}
	}
}

// maybeStartScheduled starts a run when the interval elapsed and nothing is in
// progress. Failures land on the status instead of being retried every minute.
func (s *Service) maybeStartScheduled() {
	interval, lastRun, _, err := s.db.GetProbeSchedule()
	if err != nil || interval <= 0 {
		return
	}
	if s.Status().Running {
		return
	}
	due := s.now().Add(-time.Duration(interval) * time.Minute)
	if lastRun > 0 && time.UnixMilli(lastRun).After(due) {
		return
	}
	if _, err := s.Start(); err != nil {
		s.mu.Lock()
		s.status.LastError = err.Error()
		s.mu.Unlock()
	}
}


