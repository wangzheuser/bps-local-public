package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrNotFound is returned when a requested row does not exist.
var ErrNotFound = errors.New("not found")

// nz converts a nullable integer column into a plain int64.
func nz(v sql.NullInt64) int64 {
	if v.Valid {
		return v.Int64
	}
	return 0
}

// Account is the persistent metadata for one BPS credential. It never contains
// tokens; the OAuth file path points at the credential on disk.
type Account struct {
	ID                string
	Name              string
	CredentialPath    string
	UpstreamAccountID string
	PlanType          string
	Enabled           bool
	Weight            int
	TokenExpiresAt    int64 // Unix ms, 0 = unknown
	LastErrorCode     string
	CooldownUntil     int64 // Unix ms, 0 = none
	CreatedAt         int64
	UpdatedAt         int64
}

const accountColumns = `id, name, credential_path, upstream_account_id, plan_type,
	enabled, weight, token_expires_at, last_error_code, cooldown_until, created_at, updated_at`

func scanAccount(row interface{ Scan(...any) error }) (*Account, error) {
	var a Account
	var expires, cooldown sql.NullInt64
	var enabled int
	if err := row.Scan(&a.ID, &a.Name, &a.CredentialPath, &a.UpstreamAccountID, &a.PlanType,
		&enabled, &a.Weight, &expires, &a.LastErrorCode, &cooldown, &a.CreatedAt, &a.UpdatedAt); err != nil {
		return nil, err
	}
	a.Enabled = enabled == 1
	a.TokenExpiresAt = nz(expires)
	a.CooldownUntil = nz(cooldown)
	return &a, nil
}

// CreateAccount inserts a new pending account row.
func (d *DB) CreateAccount(a *Account) error {
	now := time.Now().UnixMilli()
	a.CreatedAt, a.UpdatedAt = now, now
	_, err := d.Exec(`INSERT INTO accounts (`+accountColumns+`) VALUES
		(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.ID, a.Name, a.CredentialPath, a.UpstreamAccountID, a.PlanType,
		boolInt(a.Enabled), a.Weight, nullInt(a.TokenExpiresAt), a.LastErrorCode,
		nullInt(a.CooldownUntil), a.CreatedAt, a.UpdatedAt)
	if err != nil {
		return fmt.Errorf("insert account: %w", err)
	}
	return nil
}

// GetAccount returns one account or ErrNotFound.
func (d *DB) GetAccount(id string) (*Account, error) {
	row := d.QueryRow(`SELECT `+accountColumns+` FROM accounts WHERE id = ?`, id)
	a, err := scanAccount(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return a, err
}

// ListAccounts returns all accounts ordered by creation time.
func (d *DB) ListAccounts() ([]*Account, error) {
	rows, err := d.Query(`SELECT ` + accountColumns + ` FROM accounts ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Account
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// FindAccountByUpstreamID locates an account by its ChatGPT account id.
func (d *DB) FindAccountByUpstreamID(upstreamID string) (*Account, error) {
	row := d.QueryRow(`SELECT `+accountColumns+` FROM accounts
		WHERE upstream_account_id = ? AND upstream_account_id <> ''`, upstreamID)
	a, err := scanAccount(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return a, err
}

// SetAccountEnabled updates the enabled flag.
func (d *DB) SetAccountEnabled(id string, enabled bool) error {
	return d.updateAccount(id, `enabled = ?`, boolInt(enabled))
}

// SetAccountWeight updates the scheduling weight.
func (d *DB) SetAccountWeight(id string, weight int) error {
	return d.updateAccount(id, `weight = ?`, weight)
}

// SetAccountCredential records a freshly authorized credential version and its
// decoded identity, clearing any previous error state.
func (d *DB) SetAccountCredential(id, credentialPath, upstreamID, planType string, tokenExpiresAt int64) error {
	return d.updateAccount(id,
		`credential_path = ?, upstream_account_id = ?, plan_type = ?, token_expires_at = ?, last_error_code = '', cooldown_until = NULL`,
		credentialPath, upstreamID, planType, nullInt(tokenExpiresAt))
}

// SetAccountError records the last scheduling error code and cooldown.
func (d *DB) SetAccountError(id, errorCode string, cooldownUntil int64) error {
	return d.updateAccount(id, `last_error_code = ?, cooldown_until = ?`, errorCode, nullInt(cooldownUntil))
}

// ClearAccountError removes the stored error state after a success or manual
// re-enable.
func (d *DB) ClearAccountError(id string) error {
	return d.updateAccount(id, `last_error_code = '', cooldown_until = NULL`)
}

// DeleteAccount removes an account record. Historical usage rows are kept but
// lose their account attribution so aggregate statistics survive the delete;
// the foreign key would otherwise block the removal.
func (d *DB) DeleteAccount(id string) error {
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("begin delete account: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`UPDATE usage_requests SET account_id = NULL WHERE account_id = ?`, id); err != nil {
		return fmt.Errorf("detach usage for account %s: %w", id, err)
	}
	res, err := tx.Exec(`DELETE FROM accounts WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete account %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit delete account %s: %w", id, err)
	}
	return nil
}

func (d *DB) updateAccount(id, assignments string, args ...any) error {
	res, err := d.Exec(`UPDATE accounts SET `+assignments+`, updated_at = ? WHERE id = ?`,
		append(args, time.Now().UnixMilli(), id)...)
	if err != nil {
		return fmt.Errorf("update account %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func nullInt(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}
