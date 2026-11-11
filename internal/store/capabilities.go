package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// AccountModel records what one account actually serves for one model, as
// measured by fingerprint attribution rather than by the model name it echoes.
type AccountModel struct {
	AccountID string
	Model     string
	TopModel  string  // model the fingerprint pointed at
	SelfProb  float64 // probability of the requested model
	Verified  bool    // TopModel == Model
	CheckedAt int64
	LastError string
}

// UpsertAccountModels writes probe results, replacing rows for the same
// (account, model) pair. A single probe run covers many pairs.
func (d *DB) UpsertAccountModels(rows []AccountModel) error {
	if len(rows) == 0 {
		return nil
	}
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("begin capability upsert: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().UnixMilli()
	for _, r := range rows {
		checked := r.CheckedAt
		if checked == 0 {
			checked = now
		}
		if _, err := tx.Exec(`INSERT INTO account_models
			(account_id, model, top_model, self_prob, verified, checked_at, last_error)
			VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(account_id, model) DO UPDATE SET
				top_model = excluded.top_model,
				self_prob = excluded.self_prob,
				verified = excluded.verified,
				checked_at = excluded.checked_at,
				last_error = excluded.last_error`,
			r.AccountID, r.Model, r.TopModel, r.SelfProb, boolInt(r.Verified), checked, r.LastError); err != nil {
			return fmt.Errorf("upsert capability %s/%s: %w", r.AccountID, r.Model, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit capability upsert: %w", err)
	}
	return nil
}

// ListAccountModels returns every stored capability row.
func (d *DB) ListAccountModels() ([]AccountModel, error) {
	rows, err := d.Query(`SELECT account_id, model, top_model, self_prob, verified, checked_at, last_error
		FROM account_models ORDER BY account_id, model`)
	if err != nil {
		return nil, fmt.Errorf("list capabilities: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []AccountModel
	for rows.Next() {
		var r AccountModel
		var verified int
		if err := rows.Scan(&r.AccountID, &r.Model, &r.TopModel, &r.SelfProb,
			&verified, &r.CheckedAt, &r.LastError); err != nil {
			return nil, fmt.Errorf("scan capability: %w", err)
		}
		r.Verified = verified == 1
		out = append(out, r)
	}
	return out, rows.Err()
}

// VerifiedAccountsForModel returns account IDs measured as really serving the
// model. The second result reports whether any row exists for the model at all,
// which lets callers distinguish "no data yet" from "nobody supports it".
func (d *DB) VerifiedAccountsForModel(model string) (accounts []string, hasData bool, err error) {
	rows, err := d.Query(`SELECT account_id, verified FROM account_models WHERE model = ?`, model)
	if err != nil {
		return nil, false, fmt.Errorf("query capability for %s: %w", model, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id string
		var verified int
		if err := rows.Scan(&id, &verified); err != nil {
			return nil, false, fmt.Errorf("scan capability: %w", err)
		}
		hasData = true
		if verified == 1 {
			accounts = append(accounts, id)
		}
	}
	return accounts, hasData, rows.Err()
}

// DeleteAccountModels drops probe rows for one account, used when it is removed.
func (d *DB) DeleteAccountModels(accountID string) error {
	if _, err := d.Exec(`DELETE FROM account_models WHERE account_id = ?`, accountID); err != nil {
		return fmt.Errorf("delete capabilities for %s: %w", accountID, err)
	}
	return nil
}

// GetProbeSchedule reads the interval in minutes, the last run time and how
// many challenges each model gets. Zero interval means scheduled probing is
// off; a zero challenge count falls back to the default of 3.
func (d *DB) GetProbeSchedule() (intervalMinutes int, lastRun int64, challenges int, err error) {
	var iv, last, ch string
	read := func(key string, dst *string) error {
		e := d.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(dst)
		if errors.Is(e, sql.ErrNoRows) {
			*dst = "0"
			return nil
		}
		return e
	}
	if err := read("probe_interval_minutes", &iv); err != nil {
		return 0, 0, 0, fmt.Errorf("read probe interval: %w", err)
	}
	if err := read("probe_last_run", &last); err != nil {
		return 0, 0, 0, fmt.Errorf("read probe last run: %w", err)
	}
	if err := read("probe_challenges", &ch); err != nil {
		return 0, 0, 0, fmt.Errorf("read probe challenges: %w", err)
	}
	_, _ = fmt.Sscanf(iv, "%d", &intervalMinutes)
	_, _ = fmt.Sscanf(last, "%d", &lastRun)
	_, _ = fmt.Sscanf(ch, "%d", &challenges)
	if challenges < 1 {
		challenges = DefaultProbeChallenges
	}
	return intervalMinutes, lastRun, challenges, nil
}

// DefaultProbeChallenges matches the bank's recommended query count.
const DefaultProbeChallenges = 3

// SetProbeInterval stores the scheduled probing interval in minutes.
func (d *DB) SetProbeInterval(minutes int) error {
	return d.SetSetting("probe_interval_minutes", fmt.Sprintf("%d", minutes))
}

// SetProbeLastRun records when a full probe run finished.
func (d *DB) SetProbeLastRun(unixMilli int64) error {
	return d.SetSetting("probe_last_run", fmt.Sprintf("%d", unixMilli))
}

// SetProbeChallenges stores how many challenges each model gets per run.
func (d *DB) SetProbeChallenges(n int) error {
	return d.SetSetting("probe_challenges", fmt.Sprintf("%d", n))
}
