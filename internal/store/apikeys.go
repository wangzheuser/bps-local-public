package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// APIKey is the persistent record for one client key. The SHA-256 hash
// authenticates requests; the optional sealed copy lets the console reveal the
// key again without keeping plaintext in the database.
type APIKey struct {
	ID         string
	Name       string
	Prefix     string
	KeyHash    string
	KeySealed  string
	Enabled    bool
	ExpiresAt  int64 // Unix ms, 0 = never
	RevokedAt  int64 // Unix ms, 0 = active
	LastUsedAt int64
	CreatedAt  int64
	UpdatedAt  int64
}

const apiKeyColumns = `id, name, prefix, key_hash, key_sealed, enabled, expires_at, revoked_at, last_used_at, created_at, updated_at`

func scanAPIKey(row interface{ Scan(...any) error }) (*APIKey, error) {
	var k APIKey
	var enabled int
	var expires, revoked, lastUsed sql.NullInt64
	if err := row.Scan(&k.ID, &k.Name, &k.Prefix, &k.KeyHash, &k.KeySealed, &enabled, &expires, &revoked, &lastUsed, &k.CreatedAt, &k.UpdatedAt); err != nil {
		return nil, err
	}
	k.Enabled = enabled == 1
	k.ExpiresAt = nz(expires)
	k.RevokedAt = nz(revoked)
	k.LastUsedAt = nz(lastUsed)
	return &k, nil
}

// CreateAPIKey inserts a new key record.
func (d *DB) CreateAPIKey(k *APIKey) error {
	now := time.Now().UnixMilli()
	k.CreatedAt, k.UpdatedAt = now, now
	_, err := d.Exec(`INSERT INTO api_keys (`+apiKeyColumns+`) VALUES
		(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		k.ID, k.Name, k.Prefix, k.KeyHash, k.KeySealed, boolInt(k.Enabled),
		nullInt(k.ExpiresAt), nullInt(k.RevokedAt), nullInt(k.LastUsedAt), k.CreatedAt, k.UpdatedAt)
	if err != nil {
		return fmt.Errorf("insert api key: %w", err)
	}
	return nil
}

// GetAPIKeyByHash looks a key up by its SHA-256 hash.
func (d *DB) GetAPIKeyByHash(hash string) (*APIKey, error) {
	row := d.QueryRow(`SELECT `+apiKeyColumns+` FROM api_keys WHERE key_hash = ?`, hash)
	k, err := scanAPIKey(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return k, err
}

// GetAPIKey returns one key by id.
func (d *DB) GetAPIKey(id string) (*APIKey, error) {
	row := d.QueryRow(`SELECT `+apiKeyColumns+` FROM api_keys WHERE id = ?`, id)
	k, err := scanAPIKey(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return k, err
}

// ListAPIKeys returns every key ordered by creation time.
func (d *DB) ListAPIKeys() ([]*APIKey, error) {
	rows, err := d.Query(`SELECT ` + apiKeyColumns + ` FROM api_keys ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*APIKey
	for rows.Next() {
		k, err := scanAPIKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// SetAPIKeyEnabled flips the enabled flag without touching revocation.
func (d *DB) SetAPIKeyEnabled(id string, enabled bool) error {
	return d.updateAPIKey(id, `enabled = ?`, boolInt(enabled))
}

// RevokeAPIKey marks a key permanently revoked.
func (d *DB) RevokeAPIKey(id string) error {
	return d.updateAPIKey(id, `enabled = 0, revoked_at = ?`, time.Now().UnixMilli())
}

// TouchAPIKey records the latest use time.
func (d *DB) TouchAPIKey(id string, at int64) error {
	res, err := d.Exec(`UPDATE api_keys SET last_used_at = ?, updated_at = ? WHERE id = ?`, at, at, id)
	if err != nil {
		return fmt.Errorf("touch api key %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteAPIKey removes a key record permanently. Historical usage rows keep
// their aggregates but lose the key attribution so the foreign key does not
// block the delete.
func (d *DB) DeleteAPIKey(id string) error {
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("begin delete api key: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`UPDATE usage_requests SET api_key_id = NULL WHERE api_key_id = ?`, id); err != nil {
		return fmt.Errorf("detach usage for api key %s: %w", id, err)
	}
	res, err := tx.Exec(`DELETE FROM api_keys WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete api key %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit delete api key %s: %w", id, err)
	}
	return nil
}

func (d *DB) updateAPIKey(id, assignments string, args ...any) error {
	res, err := d.Exec(`UPDATE api_keys SET `+assignments+`, updated_at = ? WHERE id = ?`,
		append(args, time.Now().UnixMilli(), id)...)
	if err != nil {
		return fmt.Errorf("update api key %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
