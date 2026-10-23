package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	auth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

var _ auth.SharedQuotaStateStore = (*postgresCooldownStateStore)(nil)

func (s *postgresCooldownStateStore) LoadSharedQuota(ctx context.Context) (auth.SharedQuotaSnapshot, error) {
	var snapshot auth.SharedQuotaSnapshot
	if s == nil || s.store == nil || s.store.db == nil {
		return snapshot, fmt.Errorf("postgres shared quota: not initialized")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	query := fmt.Sprintf("SELECT content FROM %s WHERE id = 'gemini-enterprise'", s.store.fullTableName(s.store.cfg.CooldownTable+"_shared"))
	var data []byte
	if err := s.store.db.QueryRowContext(ctx, query).Scan(&data); err == sql.ErrNoRows {
		return snapshot, nil
	} else if err != nil {
		return snapshot, fmt.Errorf("postgres shared quota: load: %w", err)
	}
	if json.Unmarshal(data, &snapshot) != nil {
		return snapshot, fmt.Errorf("postgres shared quota: invalid snapshot")
	}
	if err := snapshot.Validate(); err != nil {
		return auth.SharedQuotaSnapshot{}, fmt.Errorf("postgres shared quota: %w", err)
	}
	return snapshot, nil
}
func (s *postgresCooldownStateStore) SaveSharedQuota(ctx context.Context, snapshot auth.SharedQuotaSnapshot) error {
	if s == nil || s.store == nil || s.store.db == nil {
		return fmt.Errorf("postgres shared quota: not initialized")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := snapshot.Validate(); err != nil {
		return fmt.Errorf("postgres shared quota: %w", err)
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		return fmt.Errorf("postgres shared quota: encode: %w", err)
	}
	tx, errBegin := s.store.db.BeginTx(ctx, nil)
	if errBegin != nil {
		return fmt.Errorf("postgres shared quota: begin: %w", errBegin)
	}
	query := fmt.Sprintf(`INSERT INTO %s AS target (id, version, content) VALUES ('gemini-enterprise', $1, $2)
	ON CONFLICT (id) DO UPDATE SET version = EXCLUDED.version, content = EXCLUDED.content
	WHERE target.version <= EXCLUDED.version`, s.store.fullTableName(s.store.cfg.CooldownTable+"_shared"))
	result, errExec := tx.ExecContext(ctx, query, snapshot.Version, data)
	if errExec != nil {
		return rollbackPostgresCooldownTransaction(tx, fmt.Errorf("postgres shared quota: save: %w", errExec))
	}
	if rows, errRows := result.RowsAffected(); errRows != nil || rows != 1 {
		return rollbackPostgresCooldownTransaction(tx, fmt.Errorf("postgres shared quota: stale snapshot or unavailable write result"))
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("postgres shared quota: commit: %w", err)
	}
	return nil
}
