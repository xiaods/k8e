package rqlitecompat

import (
	"context"
	"strconv"
)

// SchemaVersion is the version of the versioned schema this layer writes. It
// is recorded in k8e_compat_meta so a future migration can detect an old
// layout; a recorded version newer than this binary refuses to start
// (KIP-29 §5).
const SchemaVersion = 1

// schemaStatements is the KIP-29 §5 schema. All keys and values are BLOB:
// SQLite compares BLOBs with memcmp, which is exactly etcd's bytewise key
// order, and arbitrary (non-UTF-8) bytes round-trip. All tables are STRICT so
// a type mistake fails loudly. Every statement is idempotent.
var schemaStatements = []string{
	`CREATE TABLE IF NOT EXISTS k8e_compat_meta (
		name  TEXT NOT NULL PRIMARY KEY,
		value INTEGER NOT NULL
	) STRICT`,

	`CREATE TABLE IF NOT EXISTS kv (
		key             BLOB NOT NULL PRIMARY KEY,
		value           BLOB NOT NULL,
		create_revision INTEGER NOT NULL,
		mod_revision    INTEGER NOT NULL,
		version         INTEGER NOT NULL,
		lease           INTEGER NOT NULL DEFAULT 0
	) STRICT`,
	`CREATE INDEX IF NOT EXISTS kv_lease ON kv(lease)`,
	`CREATE INDEX IF NOT EXISTS kv_mod_rev ON kv(mod_revision)`,

	`CREATE TABLE IF NOT EXISTS kv_history (
		key          BLOB NOT NULL,
		mod_revision INTEGER NOT NULL,
		version      INTEGER NOT NULL,
		deleted      INTEGER NOT NULL,
		value        BLOB,
		lease        INTEGER NOT NULL DEFAULT 0,
		create_revision INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (key, mod_revision)
	) STRICT`,
	`CREATE INDEX IF NOT EXISTS kv_history_rev ON kv_history(mod_revision)`,

	`CREATE TABLE IF NOT EXISTS applied_requests (
		request_id TEXT NOT NULL PRIMARY KEY,
		kind       TEXT NOT NULL,
		outcome    TEXT NOT NULL,
		revision   INTEGER NOT NULL
	) STRICT`,

	`CREATE TABLE IF NOT EXISTS events (
		revision  INTEGER NOT NULL,
		seq       INTEGER NOT NULL,
		kind      TEXT NOT NULL,
		key       BLOB NOT NULL,
		value     BLOB,
		create_revision INTEGER NOT NULL,
		mod_revision    INTEGER NOT NULL,
		version         INTEGER NOT NULL,
		lease           INTEGER NOT NULL,
		prev_value           BLOB,
		prev_create_revision INTEGER NOT NULL,
		prev_mod_revision    INTEGER NOT NULL,
		prev_version         INTEGER NOT NULL,
		prev_lease           INTEGER NOT NULL,
		PRIMARY KEY (revision, seq)
	) STRICT`,

	// op_prev and txn_ctx are per-transaction scratch tables. A request is a
	// single SQLite transaction, and rqlite applies statements serially, so a
	// scratch row can never be observed by another request.
	`CREATE TABLE IF NOT EXISTS op_prev (
		key             BLOB NOT NULL PRIMARY KEY,
		value           BLOB,
		create_revision INTEGER NOT NULL,
		mod_revision    INTEGER NOT NULL,
		version         INTEGER NOT NULL,
		lease           INTEGER NOT NULL
	) STRICT`,
	`CREATE TABLE IF NOT EXISTS txn_ctx (
		id     INTEGER NOT NULL PRIMARY KEY,
		passed INTEGER NOT NULL,
		rev    INTEGER NOT NULL
	) STRICT`,

	`CREATE TABLE IF NOT EXISTS leases (
		id          INTEGER NOT NULL PRIMARY KEY,
		ttl         INTEGER NOT NULL,
		expiry_unix INTEGER NOT NULL,
		owner       TEXT NOT NULL
	) STRICT`,
	`CREATE INDEX IF NOT EXISTS leases_expiry ON leases(expiry_unix)`,

	`INSERT OR IGNORE INTO k8e_compat_meta(name, value) VALUES('revision', 0)`,
	`INSERT OR IGNORE INTO k8e_compat_meta(name, value) VALUES('compact_revision', 0)`,
	`INSERT OR IGNORE INTO k8e_compat_meta(name, value) VALUES('cluster_id', 0)`,
	`INSERT OR IGNORE INTO k8e_compat_meta(name, value) VALUES('schema_version', ` +
		strconv.Itoa(SchemaVersion) + `)`,
}

// Bootstrap creates the schema. It is idempotent: CREATE TABLE IF NOT EXISTS
// plus INSERT OR IGNORE, so a second call neither resets the revision counter
// nor duplicates the schema version row. A recorded schema_version newer than
// this binary is refused (no implicit downgrade).
func Bootstrap(ctx context.Context, c *Client) error {
	resp, err := c.Read(ctx, Statement{SQL: `SELECT value FROM k8e_compat_meta WHERE name = 'schema_version'`})
	if err == nil && len(resp.Results) > 0 && len(resp.Results[0].Values) > 0 {
		row, rowErr := resp.Results[0].row(0)
		if rowErr != nil {
			return rowErr
		}
		if got := row.int("value"); got > SchemaVersion {
			return &Err{Message: "rqlitecompat: schema version " + strconv.FormatInt(got, 10) +
				" is newer than this binary (" + strconv.Itoa(SchemaVersion) + ")"}
		}
	}

	stmts := make([]Statement, 0, len(schemaStatements))
	for _, sql := range schemaStatements {
		stmts = append(stmts, Statement{SQL: sql})
	}
	_, err = c.Write(ctx, stmts...)
	return err
}
