// Package accounts manages BPS account records and their device authorization
// lifecycle. Credential tokens live only in OAuth files; this package moves
// them from the authorization flow to the credential path and nothing further.
package accounts

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/sunboyzoey/GPT-BPS/internal/auth"
	"github.com/sunboyzoey/GPT-BPS/internal/store"
)

// ErrAlreadyAuthorized means the account already holds valid credentials, so a
// first-time device authorization would only leave the UI waiting needlessly.
var ErrAlreadyAuthorized = errors.New("account is already authorized; use reauthorization to replace its credentials")

// Authorizer performs one device authorization round. Production wraps
// auth.DeviceClient; tests inject a fake.
type Authorizer interface {
	Authorize(ctx context.Context, show func(url, userCode string) error) (*auth.DeviceCredentials, error)
}

// DeviceAuthorizer adapts auth.DeviceClient to Authorizer.
type DeviceAuthorizer struct {
	Client *auth.DeviceClient
}

// Authorize runs the device login flow.
func (d DeviceAuthorizer) Authorize(ctx context.Context, show func(url, userCode string) error) (*auth.DeviceCredentials, error) {
	return d.Client.Login(ctx, func(p auth.DevicePrompt) error {
		return show(p.URL, p.UserCode)
	})
}

// RuntimeManager receives validated credentials after authorization succeeds.
// The managed server implements it by building a proxy runtime and adding it
// to the scheduler.
type RuntimeManager interface {
	// EnsureRuntime builds (or rebuilds) the runtime for the account.
	EnsureRuntime(a store.Account, st *auth.Store) error
	// HasRuntime reports whether a runtime is currently loaded.
	HasRuntime(id string) bool
	// RemoveRuntime drops the account from scheduling and drains its transport.
	RemoveRuntime(id string)
}

// AuthTaskView is the admin-visible snapshot of one authorization task.
type AuthTaskView struct {
	AccountID string `json:"account_id"`
	State     string `json:"state"` // running | succeeded | failed | cancelled
	URL       string `json:"url,omitempty"`
	UserCode  string `json:"user_code,omitempty"`
	StartedAt int64  `json:"started_at"`
	ExpiresAt int64  `json:"expires_at"`
	Error     string `json:"error,omitempty"`
}

type authTask struct {
	mu        sync.Mutex
	accountID string
	state     string
	url       string
	userCode  string
	startedAt time.Time
	expiresAt time.Time
	err       error
	cancel    context.CancelFunc
	done      chan struct{}
	shown     chan struct{} // closed once the device prompt has been captured
}

func (t *authTask) view() AuthTaskView {
	t.mu.Lock()
	defer t.mu.Unlock()
	v := AuthTaskView{
		AccountID: t.accountID,
		State:     t.state,
		URL:       t.url,
		UserCode:  t.userCode,
		StartedAt: t.startedAt.UnixMilli(),
		ExpiresAt: t.expiresAt.UnixMilli(),
	}
	if t.err != nil {
		v.Error = t.err.Error()
	}
	return v
}

// Service coordinates account records, authorization tasks and runtime handoff.
type Service struct {
	db        *store.DB
	dataDir   string
	proxyURL  func() string
	manager   RuntimeManager
	newAuth   func(proxyAddress string) (Authorizer, error)
	now       func() time.Time
	idGen     func(prefix string) (string, error)

	mu    sync.Mutex
	tasks map[string]*authTask
	lastAuthErr map[string]string
	// completeMu serializes credential completion so the duplicate-upstream
	// check and the identity-unique write never interleave between tasks of
	// this process. The database unique index stays as the final guard.
	completeMu sync.Mutex
}

// NewService builds the account service. newAuth is injectable for tests.
// proxyURL is read on every authorization so a console proxy change applies to
// the next device login without a restart.
func NewService(db *store.DB, dataDir string, proxyURL func() string, manager RuntimeManager,
	newAuth func(proxyAddress string) (Authorizer, error)) (*Service, error) {
	if newAuth == nil {
		newAuth = func(proxy string) (Authorizer, error) {
			client, err := auth.NewDeviceClient(proxy)
			if err != nil {
				return nil, err
			}
			return DeviceAuthorizer{Client: client}, nil
		}
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("create accounts directory: %w", err)
	}
	return &Service{
		db:          db,
		dataDir:     dataDir,
		proxyURL:    proxyURL,
		manager:     manager,
		newAuth:     newAuth,
		now:         time.Now,
		idGen:       newID,
		tasks:       make(map[string]*authTask),
		lastAuthErr: make(map[string]string),
	}, nil
}

func newID(prefix string) (string, error) {
	raw := make([]byte, 12)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(raw), nil
}

// Create records a new pending account.
func (s *Service) Create(name string) (*store.Account, error) {
	id, err := s.idGen("acct_")
	if err != nil {
		return nil, err
	}
	a := &store.Account{
		ID:   id,
		Name: name,
		// The credential path is reserved now; the file appears only after a
		// successful authorization. The version suffix keeps reauthorizations
		// from ever overwriting a live file.
		CredentialPath: filepath.Join(s.dataDir, id+"-v1.json"),
		Enabled:        false,
		Weight:         1,
	}
	if err := s.db.CreateAccount(a); err != nil {
		return nil, err
	}
	return a, nil
}

// StartAuthorization launches (or returns) the device authorization task for an
// account that has no credentials yet.
func (s *Service) StartAuthorization(id string) (*AuthTaskView, error) {
	return s.startAuthorization(id, false)
}

// Reauthorize replaces an existing account's credentials with a new device
// authorization. It is the only path that may start a task on an account that
// already holds credentials.
func (s *Service) Reauthorize(id string) (*AuthTaskView, error) {
	return s.startAuthorization(id, true)
}

func (s *Service) startAuthorization(id string, allowAuthorized bool) (*AuthTaskView, error) {
	a, err := s.db.GetAccount(id)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if t, ok := s.tasks[id]; ok {
		t.mu.Lock()
		running := t.state == "running"
		t.mu.Unlock()
		if running {
			s.mu.Unlock()
			v := t.view()
			return &v, nil
		}
	}
	// An account that already holds credentials is authorized. Starting another
	// device task would leave the panel waiting for an authorization the user
	// does not need to repeat, which looks like a stuck dialog. Re-authorization
	// is an explicit action and goes through Reauthorize.
	if a.UpstreamAccountID != "" && !allowAuthorized {
		s.mu.Unlock()
		return nil, ErrAlreadyAuthorized
	}
	authz, err := s.newAuth(s.proxyURL())
	if err != nil {
		s.mu.Unlock()
		return nil, fmt.Errorf("build authorizer: %w", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	started := s.now()
	t := &authTask{
		accountID: id,
		state:     "running",
		startedAt: started,
		expiresAt: started.Add(15 * time.Minute),
		cancel:    cancel,
		done:      make(chan struct{}),
		shown:     make(chan struct{}),
	}
	s.tasks[id] = t
	delete(s.lastAuthErr, id)
	s.mu.Unlock()

	go func() {
		defer close(t.done)
		creds, err := authz.Authorize(ctx, func(url, code string) error {
			t.mu.Lock()
			t.url, t.userCode = url, code
			t.mu.Unlock()
			close(t.shown)
			return nil
		})
		t.mu.Lock()
		t.err = err
		t.mu.Unlock()
		if err != nil {
			if ctx.Err() != nil {
				t.mu.Lock()
				t.state = "cancelled"
				t.mu.Unlock()
			} else {
				t.mu.Lock()
				t.state = "failed"
				t.mu.Unlock()
				s.mu.Lock()
				s.lastAuthErr[id] = err.Error()
				s.mu.Unlock()
				_ = s.db.SetAccountError(id, "authorization_failed", 0)
			}
			return
		}
		if err := s.complete(a, creds); err != nil {
			t.mu.Lock()
			t.err = err
			t.state = "failed"
			t.mu.Unlock()
			s.mu.Lock()
			s.lastAuthErr[id] = err.Error()
			s.mu.Unlock()
			_ = s.db.SetAccountError(id, "authorization_failed", 0)
			return
		}
		t.mu.Lock()
		t.state = "succeeded"
		t.mu.Unlock()
	}()
	// Give the authorizer a moment to surface the device prompt so the admin
	// UI receives URL and user code in the same response. Authorization
	// services usually answer fast; if not, the UI polls the task anyway.
	select {
	case <-t.shown:
	case <-t.done:
	case <-time.After(3 * time.Second):
	}
	v := t.view()
	return &v, nil
}

// complete saves the credentials, validates identity and hands off to the
// runtime manager. It never overwrites an existing credential file.
func (s *Service) complete(a *store.Account, creds *auth.DeviceCredentials) error {
	s.completeMu.Lock()
	defer s.completeMu.Unlock()
	path := a.CredentialPath
	if _, err := os.Lstat(path); err == nil {
		// Reserved path already used (reauthorization or retry): pick a new
		// versioned file so a live credential is never replaced mid-flight.
		suffix, err := s.idGen("v")
		if err != nil {
			return err
		}
		ext := filepath.Ext(path)
		path = path[:len(path)-len(ext)] + "-" + suffix + ext
	}
	if err := auth.SaveDeviceCredentials(path, creds); err != nil {
		return fmt.Errorf("save credential: %w", err)
	}
	st, err := auth.Load(path)
	if err != nil {
		return fmt.Errorf("load credential: %w", err)
	}
	token, upstreamID, err := st.Credentials()
	if err != nil {
		return fmt.Errorf("read credential identity: %w", err)
	}
	if upstreamID == "" {
		upstreamID = auth.AccountIDFromToken(token)
	}
	if upstreamID == "" {
		return errors.New("credential does not carry a ChatGPT account id")
	}
	if existing, err := s.db.FindAccountByUpstreamID(upstreamID); err == nil && existing.ID != a.ID {
		return fmt.Errorf("upstream account %s… is already managed as %s", upstreamID[:min(8, len(upstreamID))], existing.ID)
	} else if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	expires := st.Expiry()
	var expiresMS int64
	if !expires.IsZero() {
		expiresMS = expires.UnixMilli()
	}
	if err := s.db.SetAccountCredential(a.ID, path, upstreamID, auth.PlanType(token), expiresMS); err != nil {
		if isUniqueUpstreamConflict(err) {
			return fmt.Errorf("upstream account %s… is already managed by another account record", upstreamID[:min(8, len(upstreamID))])
		}
		return err
	}
	updated, err := s.db.GetAccount(a.ID)
	if err != nil {
		return err
	}
	if s.manager != nil {
		if err := s.manager.EnsureRuntime(*updated, st); err != nil {
			_ = s.db.SetAccountError(a.ID, "runtime_build_failed", 0)
			return fmt.Errorf("build runtime: %w", err)
		}
	}
	return nil
}

// AuthorizationStatus returns the current task view, or nil when none exists.
func (s *Service) AuthorizationStatus(id string) *AuthTaskView {
	s.mu.Lock()
	t, ok := s.tasks[id]
	s.mu.Unlock()
	if !ok {
		return nil
	}
	v := t.view()
	return &v
}

// CancelAuthorization stops a running task. Finished tasks are untouched.
func (s *Service) CancelAuthorization(id string) (*AuthTaskView, error) {
	s.mu.Lock()
	t, ok := s.tasks[id]
	s.mu.Unlock()
	if !ok {
		return nil, store.ErrNotFound
	}
	t.mu.Lock()
	running := t.state == "running"
	t.mu.Unlock()
	if running {
		t.cancel()
		select {
		case <-t.done:
		case <-time.After(5 * time.Second):
		}
	}
	v := t.view()
	return &v, nil
}

// LastAuthError reports the stored failure text for the admin UI.
func (s *Service) LastAuthError(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastAuthErr[id]
}

// SetEnabled toggles scheduling. Enabling reloads credentials and rebuilds the
// runtime when needed; failures surface to the caller and the DB.
func (s *Service) SetEnabled(id string, enabled bool) error {
	a, err := s.db.GetAccount(id)
	if err != nil {
		return err
	}
	if !enabled {
		if err := s.db.SetAccountEnabled(id, false); err != nil {
			return err
		}
		return nil
	}
	if a.UpstreamAccountID == "" {
		return errors.New("account is not authorized yet")
	}
	if s.manager != nil && !s.manager.HasRuntime(id) {
		st, err := auth.Load(a.CredentialPath)
		if err != nil {
			_ = s.db.SetAccountError(id, "credential_load_failed", 0)
			return fmt.Errorf("load credential: %w", err)
		}
		if err := s.manager.EnsureRuntime(*a, st); err != nil {
			_ = s.db.SetAccountError(id, "runtime_build_failed", 0)
			return fmt.Errorf("build runtime: %w", err)
		}
	}
	if err := s.db.SetAccountEnabled(id, true); err != nil {
		return err
	}
	return s.db.ClearAccountError(id)
}

// SetWeight updates the scheduling weight with bounds validation.
func (s *Service) SetWeight(id string, weight int) error {
	if weight < 1 || weight > 100 {
		return errors.New("weight must be between 1 and 100")
	}
	return s.db.SetAccountWeight(id, weight)
}

// Delete removes an account permanently: its record, every credential file
// version on disk, its authorization task and its scheduling runtime. Usage
// history is retained without the account attribution.
//
// The database row is removed before files so a failure never leaves a record
// pointing at missing credentials; orphaned credential files are harmless.
func (s *Service) Delete(id string) error {
	a, err := s.db.GetAccount(id)
	if err != nil {
		return err
	}

	// Stop any in-flight device authorization for this account.
	s.mu.Lock()
	if t, ok := s.tasks[id]; ok {
		t.mu.Lock()
		running := t.state == "running"
		t.mu.Unlock()
		if running {
			t.cancel()
		}
		delete(s.tasks, id)
	}
	delete(s.lastAuthErr, id)
	s.mu.Unlock()

	if err := s.db.DeleteAccount(id); err != nil {
		return err
	}
	if s.manager != nil {
		s.manager.RemoveRuntime(id)
	}

	// Remove every credential version: <id>-v1.json, <id>-v1-<suffix>.json.
	matches, err := filepath.Glob(filepath.Join(s.dataDir, a.ID+"-*.json"))
	if err != nil {
		return fmt.Errorf("scan credential files: %w", err)
	}
	for _, path := range matches {
		if rmErr := os.Remove(path); rmErr != nil && !os.IsNotExist(rmErr) {
			return fmt.Errorf("remove credential file %s: %w", filepath.Base(path), rmErr)
		}
	}
	return nil
}

// Shutdown cancels all running authorization tasks and waits for them in
// parallel so total wait stays bounded regardless of task count.
func (s *Service) Shutdown() {
	s.mu.Lock()
	tasks := make([]*authTask, 0, len(s.tasks))
	for _, t := range s.tasks {
		tasks = append(tasks, t)
	}
	s.mu.Unlock()
	var wg sync.WaitGroup
	for _, t := range tasks {
		t.mu.Lock()
		running := t.state == "running"
		t.mu.Unlock()
		if !running {
			continue
		}
		wg.Add(1)
		go func(t *authTask) {
			defer wg.Done()
			t.cancel()
			select {
			case <-t.done:
			case <-time.After(2 * time.Second):
			}
		}(t)
	}
	wg.Wait()
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// isUniqueUpstreamConflict reports whether the driver surfaced the partial
// unique index on accounts.upstream_account_id. modernc sqlite exposes this
// as a constraint failure with the index/columns in its message.
func isUniqueUpstreamConflict(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed") &&
		strings.Contains(err.Error(), "upstream_account_id")
}
