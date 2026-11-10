package accounts

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sunboyzoey/GPT-BPS/internal/auth"
	"github.com/sunboyzoey/GPT-BPS/internal/store"
)

// fakeAuthorizer returns fixed credentials without touching the network.
type fakeAuthorizer struct {
	creds *auth.DeviceCredentials
	err   error
}

func (f fakeAuthorizer) Authorize(ctx context.Context, show func(url, userCode string) error) (*auth.DeviceCredentials, error) {
	if f.err != nil {
		return nil, f.err
	}
	if err := show("https://auth.example.test/device", "TEST-CODE"); err != nil {
		return nil, err
	}
	return f.creds, nil
}

// blockingAuthorizer waits for its context before returning.
type blockingAuthorizer struct{ cancelled atomic.Bool }

func (b *blockingAuthorizer) Authorize(ctx context.Context, show func(url, userCode string) error) (*auth.DeviceCredentials, error) {
	if err := show("https://auth.example.test/device", "BLOCK-CODE"); err != nil {
		return nil, err
	}
	<-ctx.Done()
	b.cancelled.Store(true)
	return nil, ctx.Err()
}

type countingManager struct {
	ensured []string
	removed []string
	has     bool
}

func (m *countingManager) EnsureRuntime(a store.Account, st *auth.Store) error {
	m.ensured = append(m.ensured, a.ID)
	m.has = true
	return nil
}

func (m *countingManager) HasRuntime(id string) bool { return m.has }

func (m *countingManager) RemoveRuntime(id string) {
	m.removed = append(m.removed, id)
	m.has = false
}

// fakeCreds builds device credentials whose access token parses as a JWT with
// a future expiry and an account claim, matching the real device-flow shape.
func fakeCreds(accountID string) *auth.DeviceCredentials {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	claims := `{"exp":` + strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10) +
		`,"https://api.openai.com/auth":{"chatgpt_account_id":"` + accountID + `","chatgpt_plan_type":"pro"}}`
	token := header + "." + base64RawURL([]byte(claims)) + ".sig"
	return &auth.DeviceCredentials{
		AccessToken: token, TokenType: "Bearer", ExpiresIn: 3600,
		Scope: "test", ClientID: "test-client",
	}
}

func base64RawURL(b []byte) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	var out []byte
	for i := 0; i < len(b); i += 3 {
		var chunk [3]byte
		n := copy(chunk[:], b[i:])
		out = append(out, alphabet[chunk[0]>>2])
		out = append(out, alphabet[(chunk[0]&0x03)<<4|chunk[1]>>4])
		if n > 1 {
			out = append(out, alphabet[(chunk[1]&0x0F)<<2|chunk[2]>>6])
		}
		if n > 2 {
			out = append(out, alphabet[(chunk[2]&0x3F)])
		}
	}
	return string(out)
}

func newTestService(t *testing.T, authz Authorizer) (*Service, *store.DB, *countingManager) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "acct.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatal(err)
	}
	mgr := &countingManager{}
	svc, err := NewService(db, filepath.Join(t.TempDir(), "accounts"), func() string { return "" }, mgr,
		func(string) (Authorizer, error) { return authz, nil })
	if err != nil {
		t.Fatal(err)
	}
	return svc, db, mgr
}

func waitAuth(t *testing.T, svc *Service, id string) {
	t.Helper()
	for i := 0; i < 100; i++ {
		v := svc.AuthorizationStatus(id)
		if v != nil && v.State != "running" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("authorization did not finish for %s", id)
}

func TestCreateAndAuthorizeAccount(t *testing.T) {
	svc, db, mgr := newTestService(t, fakeAuthorizer{creds: fakeCreds("up-1")})
	a, err := svc.Create("测试账号")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if a.Enabled || a.Weight != 1 {
		t.Fatalf("unexpected defaults: %+v", a)
	}

	task, err := svc.StartAuthorization(a.ID)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if task.UserCode != "TEST-CODE" {
		t.Fatalf("user code = %q", task.UserCode)
	}
	waitAuth(t, svc, a.ID)

	v := svc.AuthorizationStatus(a.ID)
	if v == nil || v.State != "succeeded" {
		t.Fatalf("task state = %+v", v)
	}

	got, err := db.GetAccount(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.UpstreamAccountID != "up-1" || got.PlanType != "pro" {
		t.Fatalf("credential identity not recorded: %+v", got)
	}
	if _, err := os.Stat(got.CredentialPath); err != nil {
		t.Fatalf("credential file missing: %v", err)
	}
	if !strings.HasSuffix(got.CredentialPath, ".json") {
		t.Fatalf("unexpected path %s", got.CredentialPath)
	}
	if len(mgr.ensured) != 1 || mgr.ensured[0] != a.ID {
		t.Fatalf("runtime not ensured: %+v", mgr.ensured)
	}
}

func TestDuplicateUpstreamRejected(t *testing.T) {
	svc, _, _ := newTestService(t, fakeAuthorizer{creds: fakeCreds("up-dup")})
	a1, _ := svc.Create("A")
	a2, _ := svc.Create("B")
	if _, err := svc.StartAuthorization(a1.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.StartAuthorization(a2.ID); err != nil {
		t.Fatal(err)
	}
	waitAuth(t, svc, a1.ID)
	waitAuth(t, svc, a2.ID)
	v := svc.AuthorizationStatus(a2.ID)
	if v == nil || v.State != "failed" || !strings.Contains(v.Error, "already managed") {
		t.Fatalf("duplicate must fail with clear error: %+v", v)
	}
	if svc.LastAuthError(a2.ID) == "" {
		t.Fatal("last auth error must be recorded")
	}
}

func TestCancelAuthorization(t *testing.T) {
	blocking := &blockingAuthorizer{}
	svc, _, _ := newTestService(t, blocking)
	a, _ := svc.Create("C")
	if _, err := svc.StartAuthorization(a.ID); err != nil {
		t.Fatal(err)
	}
	task, err := svc.CancelAuthorization(a.ID)
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if task.State != "cancelled" {
		t.Fatalf("state = %s", task.State)
	}
	if !blocking.cancelled.Load() {
		t.Fatal("authorizer context was not cancelled")
	}
}

func TestSetEnabledRequiresAuthorization(t *testing.T) {
	svc, _, _ := newTestService(t, fakeAuthorizer{creds: fakeCreds("up-x")})
	a, _ := svc.Create("D")
	if err := svc.SetEnabled(a.ID, true); err == nil {
		t.Fatal("enabling an unauthorized account must fail")
	}
}

func TestSetWeightBounds(t *testing.T) {
	svc, _, _ := newTestService(t, fakeAuthorizer{creds: fakeCreds("up-w")})
	a, _ := svc.Create("E")
	if err := svc.SetWeight(a.ID, 0); err == nil {
		t.Fatal("weight 0 must be rejected")
	}
	if err := svc.SetWeight(a.ID, 101); err == nil {
		t.Fatal("weight 101 must be rejected")
	}
	if err := svc.SetWeight(a.ID, 5); err != nil {
		t.Fatalf("valid weight rejected: %v", err)
	}
}

func TestShutdownCancelsRunningTasks(t *testing.T) {
	blocking := &blockingAuthorizer{}
	svc, _, _ := newTestService(t, blocking)
	a, _ := svc.Create("F")
	if _, err := svc.StartAuthorization(a.ID); err != nil {
		t.Fatal(err)
	}
	svc.Shutdown()
	if !blocking.cancelled.Load() {
		t.Fatal("shutdown must cancel running tasks")
	}
	if !errors.Is(context.Canceled, context.Canceled) {
		t.Fatal("sanity")
	}
}

func TestStartAuthorizationRejectsAuthorizedAccount(t *testing.T) {
	svc, db, _ := newTestService(t, fakeAuthorizer{creds: fakeCreds("up-guard")})
	a, _ := svc.Create("已授权")
	if _, err := svc.StartAuthorization(a.ID); err != nil {
		t.Fatal(err)
	}
	waitAuth(t, svc, a.ID)

	saved, err := db.GetAccount(a.ID)
	if err != nil || saved.UpstreamAccountID == "" {
		t.Fatalf("account should be authorized: %+v %v", saved, err)
	}

	// A plain authorization call must not start a new task on an authorized
	// account; that is what made the panel wait forever.
	if _, err := svc.StartAuthorization(a.ID); !errors.Is(err, ErrAlreadyAuthorized) {
		t.Fatalf("second StartAuthorization = %v, want ErrAlreadyAuthorized", err)
	}

	// Explicit reauthorization is allowed and replaces credentials.
	task, err := svc.Reauthorize(a.ID)
	if err != nil {
		t.Fatalf("reauthorize: %v", err)
	}
	if task.State != "running" && task.State != "succeeded" {
		t.Fatalf("unexpected task state %q", task.State)
	}
	waitAuth(t, svc, a.ID)
}

func TestDeleteAccountRemovesRecordFilesAndRuntime(t *testing.T) {
	svc, db, mgr := newTestService(t, fakeAuthorizer{creds: fakeCreds("up-del")})
	a, _ := svc.Create("待删除")
	if _, err := svc.StartAuthorization(a.ID); err != nil {
		t.Fatal(err)
	}
	waitAuth(t, svc, a.ID)

	saved, err := db.GetAccount(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(saved.CredentialPath); err != nil {
		t.Fatalf("credential file should exist before delete: %v", err)
	}

	if err := svc.Delete(a.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}

	if _, err := db.GetAccount(a.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("record still present: %v", err)
	}
	if _, err := os.Stat(saved.CredentialPath); !os.IsNotExist(err) {
		t.Fatalf("credential file still present: %v", err)
	}
	if len(mgr.removed) != 1 || mgr.removed[0] != a.ID {
		t.Fatalf("runtime not removed: %+v", mgr.removed)
	}
	if svc.AuthorizationStatus(a.ID) != nil {
		t.Fatal("authorization task not cleared")
	}

	// Deleting again is a clean not-found error.
	if err := svc.Delete(a.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("second delete = %v, want ErrNotFound", err)
	}
}

func TestDeleteAccountKeepsUsageHistory(t *testing.T) {
	svc, db, _ := newTestService(t, fakeAuthorizer{creds: fakeCreds("up-usage")})
	a, _ := svc.Create("有用量")
	if _, err := svc.StartAuthorization(a.ID); err != nil {
		t.Fatal(err)
	}
	waitAuth(t, svc, a.ID)

	acctID := a.ID
	u := &store.UsageRequest{
		ID: "req_keep", AccountID: &acctID, Status: "completed",
		HTTPStatus: 200, StartedAt: 1, FinishedAt: 2, DurationMS: 1,
	}
	if err := db.InsertUsageRequest(u); err != nil {
		t.Fatal(err)
	}

	if err := svc.Delete(a.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}

	rows, total, err := db.ListUsageRequests(store.UsageFilters{})
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || len(rows) != 1 {
		t.Fatalf("usage history lost: total=%d", total)
	}
	if rows[0].AccountID != nil {
		t.Fatalf("usage still attributed: %v", *rows[0].AccountID)
	}
}
