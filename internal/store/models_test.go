package store

import (
	"errors"
	"path/filepath"
	"testing"
)

func openModelsDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "models.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestModelCatalogRoundTrip(t *testing.T) {
	db := openModelsDB(t)

	// Never synced reads as ErrNotFound so callers can prompt for a sync.
	if _, err := db.GetModelCatalog(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("empty catalog err = %v, want ErrNotFound", err)
	}

	payload := []byte(`{"object":"list","data":[{"id":"gpt-5.6-sol"}]}`)
	if err := db.SaveModelCatalog(payload, "acct-a"); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetModelCatalog()
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Payload) != string(payload) {
		t.Fatalf("payload = %s", got.Payload)
	}
	if got.SourceAccount != "acct-a" || got.SyncedAt == 0 {
		t.Fatalf("metadata = %+v", got)
	}

	// A second sync replaces the snapshot rather than appending.
	next := []byte(`{"object":"list","data":[{"id":"gpt-6-sol"}]}`)
	if err := db.SaveModelCatalog(next, "acct-b"); err != nil {
		t.Fatal(err)
	}
	got, err = db.GetModelCatalog()
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Payload) != string(next) || got.SourceAccount != "acct-b" {
		t.Fatalf("after resync = %+v", got)
	}
}

func TestModelCatalogRejectsEmptyPayload(t *testing.T) {
	db := openModelsDB(t)
	if err := db.SaveModelCatalog(nil, "acct"); err == nil {
		t.Fatal("empty payload must be rejected")
	}
}

func TestModelMappingsReplaceAndLookup(t *testing.T) {
	db := openModelsDB(t)

	if _, err := db.GetModelMapping("gpt-6-astra"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing mapping err = %v, want ErrNotFound", err)
	}

	pairs := []ModelMapping{
		{SourceModel: "gpt-6-astra", TargetModel: "gpt-5.6-luna"},
		{SourceModel: "gpt-6-sol", TargetModel: "gpt-5.6-sol"},
	}
	if err := db.ReplaceModelMappings(pairs); err != nil {
		t.Fatal(err)
	}
	rows, err := db.ListModelMappings()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d", len(rows))
	}
	// Ordered by source model.
	if rows[0].SourceModel != "gpt-6-astra" || rows[1].SourceModel != "gpt-6-sol" {
		t.Fatalf("order = %+v", rows)
	}
	if target, err := db.GetModelMapping("gpt-6-astra"); err != nil || target != "gpt-5.6-luna" {
		t.Fatalf("lookup = %q, %v", target, err)
	}

	// Replacing with a smaller set removes the dropped entry.
	if err := db.ReplaceModelMappings([]ModelMapping{
		{SourceModel: "gpt-6-astra", TargetModel: "gpt-5.6-luna"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetModelMapping("gpt-6-sol"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("dropped mapping still present: %v", err)
	}
	rows, _ = db.ListModelMappings()
	if len(rows) != 1 {
		t.Fatalf("rows after replace = %d", len(rows))
	}

	// Clearing everything works too.
	if err := db.ReplaceModelMappings(nil); err != nil {
		t.Fatal(err)
	}
	rows, _ = db.ListModelMappings()
	if len(rows) != 0 {
		t.Fatalf("rows after clear = %d", len(rows))
	}
}

func TestModelMappingsReplaceIsAtomic(t *testing.T) {
	db := openModelsDB(t)
	if err := db.ReplaceModelMappings([]ModelMapping{
		{SourceModel: "keep", TargetModel: "target"},
	}); err != nil {
		t.Fatal(err)
	}
	// A duplicate primary key inside one batch must roll the whole replace back,
	// leaving the previous set intact instead of a partial one.
	err := db.ReplaceModelMappings([]ModelMapping{
		{SourceModel: "dup", TargetModel: "a"},
		{SourceModel: "dup", TargetModel: "b"},
	})
	if err == nil {
		t.Fatal("duplicate source must fail")
	}
	rows, _ := db.ListModelMappings()
	if len(rows) != 1 || rows[0].SourceModel != "keep" {
		t.Fatalf("previous mappings must survive a failed replace: %+v", rows)
	}
}

