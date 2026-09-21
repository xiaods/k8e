package rqlitecompat

import (
	"context"
	"strconv"
)

// SchemaVersion is the version of the versioned schema this prototype writes.
// It is recorded in k8e_compat_meta so a future migration can detect an old
// layout; the full draft (events, leases, identities, compaction watermark)
// lives in KIP-29.
const SchemaVersion = 1

// The prototype schema is the storage-level subset of the KIP-29 draft that
// the M0 gate must exercise:
//
//   - kv            current key state (create_revision/mod_revision/version)
//   - kv_history    retained revisions including tombstones (deleted=1)
//   - k8e_compat_meta  global revision counter and schema version
//   - applied_requests request identity + committed outcome, used to make an
//     uncertain request replay exactly once
//
// All tables are STRICT and all keys/values are BLOBs so that byte ordering
// and byte-for-byte round-tripping match etcd. SQLite compares BLOBs with
// memcmp, which is the ordering etcd's range scans rely on.
var schemaStatements = []string{
	`CREATE TABLE IF NOT EXISTS k8e_compat_meta (
		name  TEXT NOT NULL PRIMARY KEY,
		value INTEGER NOT NULL
	) STRICT`,
	`CREATE TABLE IF NOT EXISTS kv (
		key             BLOB NOT NULL PRIMARY KEY,
		create_revision INTEGER NOT NULL,
		mod_revision    INTEGER NOT NULL,
		version         INTEGER NOT NULL,
		lease           INTEGER NOT NULL DEFAULT 0,
		value           BLOB NOT NULL
	) STRICT`,
	`CREATE TABLE IF NOT EXISTS kv_history (
		key          BLOB NOT NULL,
		mod_revision INTEGER NOT NULL,
		version      INTEGER NOT NULL,
		deleted      INTEGER NOT NULL,
		value        BLOB,
		PRIMARY KEY (key, mod_revision)
	) STRICT`,
	`CREATE TABLE IF NOT EXISTS applied_requests (
		request_id TEXT NOT NULL PRIMARY KEY,
		kind       TEXT NOT NULL,
		outcome    TEXT NOT NULL,
		revision   INTEGER NOT NULL
	) STRICT`,
	`INSERT OR IGNORE INTO k8e_compat_meta(name, value) VALUES('revision', 0)`,
	`INSERT OR IGNORE INTO k8e_compat_meta(name, value) VALUES('schema_version', ` +
		strconv.Itoa(SchemaVersion) + `)`,
}

// Bootstrap creates the schema. It is idempotent: CREATE TABLE IF NOT EXISTS
// plus INSERT OR IGNORE, so a second call neither resets the revision counter
// nor duplicates the schema version row.
func Bootstrap(ctx context.Context, c *Client) error {
	stmts := make([]Statement, 0, len(schemaStatements))
	for _, sql := range schemaStatements {
		stmts = append(stmts, Statement{SQL: sql})
	}
	_, err := c.Write(ctx, stmts...)
	return err
}
