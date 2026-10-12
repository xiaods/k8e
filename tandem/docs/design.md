# Tandem — etcd v3 Protocol Compatibility Layer

## Overview

A Zig implementation of the etcd v3 gRPC API backed by rqlite (Raft + SQLite).
Provides MVCC semantics, atomic transactions, watches, leases, and compaction
while rqlite handles consensus and persistence.

```
k8e / kube-apiserver
        │ etcd v3 gRPC + mTLS
        ▼
   Tandem (this)
   ┌─────────────────────────────┐
   │ gRPC Server (HTTP/2)        │
   │  KV / Watch / Lease         │
   │  Cluster / Maintenance      │
   ├─────────────────────────────┤
   │ MVCC Engine                 │
   │  Revision / History / Txn   │
   │  Watch Events / Lease TTL   │
   ├─────────────────────────────┤
   │ rqlite HTTP Client          │
   │  linearizable reads         │
   │  atomic transactions        │
   └─────────────┬───────────────┘
                 │ HTTP
                 ▼
            rqlite :4001
       Raft / SQLite / Persistent
```

## Key Design Decisions

### 1. MVCC Schema (SQLite via rqlite)

```sql
-- Current key-value state (latest version per key)
CREATE TABLE IF NOT EXISTS kv (
    key            BLOB PRIMARY KEY,
    value          BLOB NOT NULL,
    create_rev     INTEGER NOT NULL,
    mod_rev        INTEGER NOT NULL,
    version        INTEGER NOT NULL DEFAULT 1,
    lease_id       INTEGER NOT NULL DEFAULT 0,
    is_deleted     INTEGER NOT NULL DEFAULT 0,
    created_at     TEXT NOT NULL DEFAULT (datetime('now'))
);

-- History of all revisions (for Watch and historical reads)
CREATE TABLE IF NOT EXISTS kv_history (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    key            BLOB NOT NULL,
    value          BLOB,
    create_rev     INTEGER NOT NULL,
    mod_rev        INTEGER NOT NULL,
    version        INTEGER NOT NULL,
    lease_id       INTEGER NOT NULL DEFAULT 0,
    is_deleted     INTEGER NOT NULL DEFAULT 0,
    operation      TEXT NOT NULL,  -- 'put' or 'delete'
    created_at     TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE INDEX IF NOT EXISTS idx_kv_history_mod_rev ON kv_history(mod_rev);
CREATE INDEX IF NOT EXISTS idx_kv_history_key ON kv_history(key, mod_rev);

-- Global revision counter
CREATE TABLE IF NOT EXISTS revision (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    current_rev INTEGER NOT NULL DEFAULT 0
);
INSERT OR IGNORE INTO revision (id, current_rev) VALUES (1, 0);

-- Leases
CREATE TABLE IF NOT EXISTS leases (
    lease_id    INTEGER PRIMARY KEY,
    ttl         INTEGER NOT NULL,
    expire_at   TEXT NOT NULL,
    created_at  TEXT NOT NULL DEFAULT (datetime('now'))
);

-- Lease-to-key associations
CREATE TABLE IF NOT EXISTS lease_keys (
    lease_id    INTEGER NOT NULL,
    key         BLOB NOT NULL,
    PRIMARY KEY (lease_id, key)
);

-- Compaction watermark
CREATE TABLE IF NOT EXISTS compaction (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    compact_rev INTEGER NOT NULL DEFAULT 0,
    compacted_at TEXT
);
INSERT OR IGNORE INTO compaction (id, compact_rev) VALUES (1, 0);
```

### 2. Atomic Transaction Semantics

All etcd Txn operations execute in a **single rqlite transaction request**:

```sql
-- Example: CAS put (mod_revision check + conditional update)
-- Phase 1: Read current state
SELECT mod_rev, version, create_rev FROM kv WHERE key = ?;
-- Phase 2: If condition passes, perform all mutations in one transaction
BEGIN;
  UPDATE revision SET current_rev = current_rev + 1 WHERE id = 1;
  SELECT current_rev FROM revision WHERE id = 1;
  UPDATE kv SET value = ?, mod_rev = ?, version = version + 1, lease_id = ? WHERE key = ?;
  INSERT INTO kv_history (key, value, create_rev, mod_rev, version, lease_id, operation)
    VALUES (?, ?, ?, ?, ?, ?, 'put');
  -- Delete old lease association if changed
  DELETE FROM lease_keys WHERE key = ? AND lease_id != ?;
  INSERT OR IGNORE INTO lease_keys (lease_id, key) VALUES (?, ?);
COMMIT;
```

### 3. Watch Implementation

- Events are written to `kv_history` within the same transaction as data changes
- Watchers poll `kv_history` by `mod_rev > last_seen_rev` at a configurable interval
- Events are filtered by key prefix/range and dispatched to matching watchers
- Each watcher has a unique `watch_id` and tracks its progress

### 4. Lease Expiry

- A background process periodically scans `leases` for expired entries
- Expired leases trigger atomic deletion of associated keys
- Only the rqlite leader executes lease expiry (enforced by conditional UPDATE)

### 5. Compaction

- Removes `kv_history` rows where `mod_rev < compact_rev`
- Refuses to compact below the minimum revision visible to active watchers
- Updates `compaction` table with the new watermark

### 6. gRPC Server

- Minimal HTTP/2 server (Zig raw TCP + TLS)
- Protobuf message framing (5-byte gRPC frame header + protobuf payload)
- Services: KV, Watch, Lease, Cluster, Maintenance, Auth

## Implementation Phases

### Phase 1: Foundation (this PR)
- [x] Project structure and build integration
- [ ] Protobuf wire format encoder/decoder
- [ ] etcd message types (KV, RangeRequest/Response, etc.)
- [ ] Minimal gRPC server (HTTP/2 listener)
- [ ] rqlite HTTP client

### Phase 2: KV/MVCC
- [ ] Schema initialization
- [ ] Range (with prefix, revision, limit, count)
- [ ] Put (with lease, prev_kv)
- [ ] DeleteRange (with prefix, prev_kv)
- [ ] Revision management

### Phase 3: Transactions
- [ ] Compare operators (EQUAL, NOT_EQUAL, GREATER, LESS, EXISTS)
- [ ] Txn (atomic condition + success/failure branches)
- [ ] Compact

### Phase 4: Watch
- [ ] Event persistence in kv_history
- [ ] Watch create/cancel/progress
- [ ] Event dispatch and fan-out
- [ ] Slow consumer handling

### Phase 5: Lease
- [ ] LeaseGrant/Revoke
- [ ] LeaseKeepAlive (streaming)
- [ ] LeaseTimeToLive
- [ ] Background expiry and key deletion

### Phase 6: Cluster & Maintenance
- [ ] Member management (delegated to rqlite /nodes API)
- [ ] Status (delegated to rqlite /status API)
- [ ] Alarm, Defragment
- [ ] Snapshot (delegated to rqlite /snapshot API)

### Phase 7: Testing
- [ ] Differential tests against real etcd
- [ ] Concurrent CAS verification
- [ ] Crash recovery
- [ ] Protocol limits (binary KV, size limits)
