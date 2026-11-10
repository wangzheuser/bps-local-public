package store

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// validNull builds a non-null nullable integer column value using field
// assignment so the construction stays obvious in tests.
func validNull(v int64) sql.NullInt64 {
	var n sql.NullInt64
	n.Valid = true
	n.Int64 = v
	return n
}

func openTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func TestMigrateIsIdempotent(t *testing.T) {
	db := openTestDB(t)
	if err := db.Migrate(); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	var version int
	if err := db.QueryRow(`SELECT COALESCE(MAX(version),0) FROM schema_migrations`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version < 1 {
		t.Fatalf("version = %d, want >= 1", version)
	}
}

func TestAccountCRUDAndUniqueUpstream(t *testing.T) {
	db := openTestDB(t)
	a := &Account{ID: "acct_a", Name: "A", CredentialPath: "/tmp/a.json", Weight: 1}
	if err := db.CreateAccount(a); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := db.GetAccount("acct_a")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "A" || got.Enabled {
		t.Fatalf("unexpected account %+v", got)
	}

	dup := &Account{ID: "acct_b", Name: "B", CredentialPath: "/tmp/b.json", UpstreamAccountID: "acct-up-1", Weight: 1}
	if err := db.CreateAccount(dup); err != nil {
		t.Fatalf("create dup: %v", err)
	}
	if err := db.SetAccountCredential("acct_b", "/tmp/b2.json", "acct-up-1", "pro", 123); err != nil {
		t.Fatalf("set credential: %v", err)
	}
	c := &Account{ID: "acct_c", Name: "C", CredentialPath: "/tmp/c.json", UpstreamAccountID: "acct-up-1"}
	if err := db.CreateAccount(c); err == nil {
		t.Fatal("duplicate upstream id must be rejected")
	}

	if err := db.SetAccountEnabled("acct_a", true); err != nil {
		t.Fatalf("enable: %v", err)
	}
	got, _ = db.GetAccount("acct_a")
	if !got.Enabled {
		t.Fatal("enable did not persist")
	}
	if err := db.SetAccountError("acct_a", "basispoints_transport_error", 999); err != nil {
		t.Fatalf("set error: %v", err)
	}
	got, _ = db.GetAccount("acct_a")
	if got.LastErrorCode != "basispoints_transport_error" || got.CooldownUntil != 999 {
		t.Fatalf("error state not persisted: %+v", got)
	}
	_ = db.ClearAccountError("acct_a")
	got, _ = db.GetAccount("acct_a")
	if got.LastErrorCode != "" || got.CooldownUntil != 0 {
		t.Fatalf("clear error failed: %+v", got)
	}
	if _, err := db.FindAccountByUpstreamID("acct-up-1"); err != nil {
		t.Fatalf("find by upstream: %v", err)
	}
}

func TestAPIKeyCRUD(t *testing.T) {
	db := openTestDB(t)
	k := &APIKey{ID: "key_1", Name: "test", Prefix: "sk-bps-abc", KeyHash: "deadbeef", Enabled: true}
	if err := db.CreateAPIKey(k); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := db.GetAPIKeyByHash("deadbeef")
	if err != nil {
		t.Fatalf("get by hash: %v", err)
	}
	if got.ID != "key_1" {
		t.Fatalf("unexpected key %+v", got)
	}
	if err := db.SetAPIKeyEnabled("key_1", false); err != nil {
		t.Fatalf("disable: %v", err)
	}
	got, _ = db.GetAPIKey("key_1")
	if got.Enabled {
		t.Fatal("disable did not persist")
	}
	if err := db.RevokeAPIKey("key_1"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	got, _ = db.GetAPIKey("key_1")
	if got.RevokedAt == 0 || got.Enabled {
		t.Fatalf("revoke did not persist: %+v", got)
	}
}

func TestDeleteAccountNullsUsageAttribution(t *testing.T) {
	db := openTestDB(t)
	a := &Account{ID: "acct_del", Name: "D", CredentialPath: "/tmp/d.json", Weight: 1}
	if err := db.CreateAccount(a); err != nil {
		t.Fatal(err)
	}
	acctID := a.ID
	u := &UsageRequest{
		ID: "req_del", AccountID: &acctID, Status: "completed",
		HTTPStatus: 200, StartedAt: 1, FinishedAt: 2, DurationMS: 1,
	}
	if err := db.InsertUsageRequest(u); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteAccount(a.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := db.GetAccount(a.ID); err != ErrNotFound {
		t.Fatalf("record still present: %v", err)
	}
	rows, total, err := db.ListUsageRequests(UsageFilters{})
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || rows[0].AccountID != nil {
		t.Fatalf("usage not detached: total=%d acct=%v", total, rows[0].AccountID)
	}
	if err := db.DeleteAccount(a.ID); err != ErrNotFound {
		t.Fatalf("second delete = %v, want ErrNotFound", err)
	}
}

func TestDeleteAPIKeyNullsUsageAttribution(t *testing.T) {
	db := openTestDB(t)
	k := &APIKey{ID: "key_del", Name: "K", Prefix: "sk-bps-x", KeyHash: "hash-del", Enabled: true}
	if err := db.CreateAPIKey(k); err != nil {
		t.Fatal(err)
	}
	keyID := k.ID
	u := &UsageRequest{
		ID: "req_kdel", APIKeyID: &keyID, Status: "completed",
		HTTPStatus: 200, StartedAt: 1, FinishedAt: 2, DurationMS: 1,
	}
	if err := db.InsertUsageRequest(u); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteAPIKey(k.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := db.GetAPIKey(k.ID); err != ErrNotFound {
		t.Fatalf("key still present: %v", err)
	}
	rows, total, err := db.ListUsageRequests(UsageFilters{})
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || rows[0].APIKeyID != nil {
		t.Fatalf("usage not detached: total=%d key=%v", total, rows[0].APIKeyID)
	}
}

func TestUsageInsertQueryCleanup(t *testing.T) {
	db := openTestDB(t)
	k := &APIKey{ID: "key_1", Name: "test", Prefix: "sk-bps-abc", KeyHash: "deadbeef", Enabled: true}
	if err := db.CreateAPIKey(k); err != nil {
		t.Fatal(err)
	}
	keyID := "key_1"
	acctID := "acct_a"
	acct := &Account{ID: acctID, Name: "A", CredentialPath: "/tmp/a.json", Weight: 1}
	if err := db.CreateAccount(acct); err != nil {
		t.Fatal(err)
	}
	u := &UsageRequest{
		ID: "req_1", APIKeyID: &keyID, AccountID: &acctID,
		RequestedModel: "gpt-6-astra", ReportedModel: "gpt-6-astra",
		Status: "completed", HTTPStatus: 200, TerminalEvent: "response.completed",
		InputTokens:  validNull(10),
		OutputTokens: validNull(5),
		StartedAt:    1000, FinishedAt: 2000, DurationMS: 1000,
	}
	if err := db.InsertUsageRequest(u); err != nil {
		t.Fatalf("insert: %v", err)
	}
	rows, total, err := db.ListUsageRequests(UsageFilters{Limit: 10})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total != 1 || len(rows) != 1 || rows[0].ID != "req_1" {
		t.Fatalf("unexpected rows total=%d rows=%+v", total, rows)
	}
	summary, err := db.UsageSummary(UsageFilters{}, "day")
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if len(summary) != 1 || summary[0].Requests != 1 || summary[0].Completed != 1 {
		t.Fatalf("unexpected summary %+v", summary)
	}
	if _, err := db.UsageSummary(UsageFilters{}, "bogus"); err == nil {
		t.Fatal("bogus group_by must fail")
	}
	n, err := db.DeleteUsageBefore(99999)
	if err != nil || n != 1 {
		t.Fatalf("cleanup n=%d err=%v", n, err)
	}
}
