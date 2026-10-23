package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"

	auth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type sharedSQLState struct {
	version            int64
	data               []byte
	commits, rollbacks int
	fail               bool
	query              string
}
type sharedSQLDriver struct{ state *sharedSQLState }
type sharedSQLConn struct{ state *sharedSQLState }
type sharedSQLTx struct{ state *sharedSQLState }
type sharedSQLRows struct {
	data []byte
	done bool
}

var sharedSQLDriverID atomic.Uint64

func (d *sharedSQLDriver) Open(string) (driver.Conn, error)  { return &sharedSQLConn{d.state}, nil }
func (c *sharedSQLConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("unused") }
func (c *sharedSQLConn) Close() error                        { return nil }
func (c *sharedSQLConn) Begin() (driver.Tx, error)           { return &sharedSQLTx{c.state}, nil }
func (t *sharedSQLTx) Commit() error                         { t.state.commits++; return nil }
func (t *sharedSQLTx) Rollback() error                       { t.state.rollbacks++; return nil }
func (c *sharedSQLConn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	c.state.query = query
	if c.state.fail {
		return nil, errors.New("synthetic database failure")
	}
	v := args[0].Value.(int64)
	if v < c.state.version {
		return driver.RowsAffected(0), nil
	}
	c.state.version = v
	c.state.data = append([]byte(nil), args[1].Value.([]byte)...)
	return driver.RowsAffected(1), nil
}
func (c *sharedSQLConn) QueryContext(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
	return &sharedSQLRows{data: c.state.data}, nil
}
func (r *sharedSQLRows) Columns() []string { return []string{"content"} }
func (r *sharedSQLRows) Close() error      { return nil }
func (r *sharedSQLRows) Next(dest []driver.Value) error {
	if r.done || len(r.data) == 0 {
		return io.EOF
	}
	r.done = true
	dest[0] = r.data
	return nil
}

func TestPostgresSharedQuotaVersionTransactionAndLegacySeparation(t *testing.T) {
	state := &sharedSQLState{}
	name := fmt.Sprintf("shared-quota-%d", sharedSQLDriverID.Add(1))
	sql.Register(name, &sharedSQLDriver{state})
	db, err := sql.Open(name, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store := &postgresCooldownStateStore{store: &PostgresStore{db: db, cfg: PostgresStoreConfig{Schema: "synthetic", CooldownTable: "cooldowns"}}}
	snapshot := auth.SharedQuotaSnapshot{SchemaVersion: 1, Version: 3, Pools: []auth.SharedQuotaState{}, Bindings: []auth.SharedQuotaBinding{}}
	if err := store.SaveSharedQuota(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(state.query, `"cooldowns_shared"`) || !strings.Contains(state.query, "target.version <= EXCLUDED.version") || strings.Contains(state.query, "auth_id") {
		t.Fatal("shared state was disguised as credential state or lacked version protection")
	}
	loaded, err := store.LoadSharedQuota(context.Background())
	if err != nil || loaded.Version != 3 {
		t.Fatalf("roundtrip: %+v, %v", loaded, err)
	}
	snapshot.Version = 2
	if err := store.SaveSharedQuota(context.Background(), snapshot); err == nil {
		t.Fatal("stale transaction accepted")
	}
	if state.commits != 1 || state.rollbacks != 1 {
		t.Fatalf("transaction completion: %+v", state)
	}
	invalid := snapshot
	invalid.SchemaVersion = 99
	if err := store.SaveSharedQuota(context.Background(), invalid); err == nil || state.commits != 1 || state.rollbacks != 1 {
		t.Fatal("invalid schema reached a write transaction")
	}
	state.fail = true
	snapshot.Version = 4
	if err := store.SaveSharedQuota(context.Background(), snapshot); err == nil || state.rollbacks != 2 {
		t.Fatal("failed save was reported as committed")
	}
	state.data = []byte(`{"schema_version":1,"version":0}`)
	if _, err := store.LoadSharedQuota(context.Background()); err == nil {
		t.Fatal("structurally invalid database snapshot was accepted")
	}
}
