package rqlitecompat

import (
	"context"
	"errors"
	"time"

	"go.etcd.io/etcd/api/v3/v3rpc/rpctypes"
)

// Lease is the layer's view of one lease.
type Lease struct {
	ID       int64
	TTL      int64
	Expiry   int64
	Revision int64
}

// LeaseGrant creates a lease. The id is random and positive; a collision is
// reported as ErrLeaseExist rather than silently overwriting (etcd semantics).
func (s *Store) LeaseGrant(ctx context.Context, id, ttl int64) (*Lease, error) {
	if id == 0 {
		generated, err := NewLeaseID()
		if err != nil {
			return nil, err
		}
		id = generated
	}
	rev, err := s.Revision(ctx)
	if err != nil {
		return nil, err
	}
	expiry := leaseExpiry(s.now(), ttl)
	resp, err := s.client.Write(ctx, Statement{
		SQL: `INSERT INTO leases(id, ttl, expiry_unix, owner) VALUES(:id, :ttl, :expiry, :owner) ` +
			`ON CONFLICT(id) DO NOTHING`,
		Params: map[string]any{"id": id, "ttl": ttl, "expiry": expiry, "owner": s.owner},
	})
	if err != nil {
		return nil, err
	}
	if resp.Results[0].RowsAffected == 0 {
		return nil, rpctypes.ErrGRPCLeaseExist
	}
	return &Lease{ID: id, TTL: ttl, Expiry: expiry, Revision: rev}, nil
}

// LeaseRevoke revokes a lease and deletes its keys in one transaction.
func (s *Store) LeaseRevoke(ctx context.Context, id int64) (int64, error) {
	resp, err := s.client.Write(ctx, s.expireLeaseStatements(id, 0)...)
	if err != nil {
		return 0, err
	}
	present, err := resp.Results[0].row(0)
	if err != nil {
		return 0, err
	}
	if present.int("present") == 0 {
		return 0, rpctypes.ErrGRPCLeaseNotFound
	}
	return readRevisionResult(resp.Results[len(resp.Results)-1])
}

// LeaseKeepAlive resets a lease's expiry and returns the TTL.
func (s *Store) LeaseKeepAlive(ctx context.Context, id int64) (int64, error) {
	expiry := leaseExpiry(s.now(), 0)
	ttl, err := s.leaseTTL(ctx, id)
	if err != nil {
		return 0, err
	}
	expiry = leaseExpiry(s.now(), ttl)
	resp, err := s.client.Write(ctx, Statement{
		SQL:    `UPDATE leases SET expiry_unix = :expiry WHERE id = :id`,
		Params: map[string]any{"expiry": expiry, "id": id},
	})
	if err != nil {
		return 0, err
	}
	if resp.Results[0].RowsAffected == 0 {
		return 0, rpctypes.ErrGRPCLeaseNotFound
	}
	return ttl, nil
}

// LeaseTimeToLive returns the lease's TTL, remaining seconds and, when keys is
// set, the keys attached to it.
func (s *Store) LeaseTimeToLive(ctx context.Context, id int64, keys bool) (*Lease, []KV, error) {
	stmts := []Statement{{
		SQL:    `SELECT id, ttl, expiry_unix FROM leases WHERE id = :id`,
		Params: map[string]any{"id": id},
	}}
	if keys {
		stmts = append(stmts, Statement{
			SQL:    `SELECT key, value, create_revision, mod_revision, version, lease FROM kv WHERE lease = :id ORDER BY key`,
			Params: map[string]any{"id": id},
		})
	}
	resp, err := s.client.Read(ctx, stmts...)
	if err != nil {
		return nil, nil, err
	}
	if len(resp.Results[0].Values) == 0 {
		return nil, nil, rpctypes.ErrGRPCLeaseNotFound
	}
	r, err := resp.Results[0].row(0)
	if err != nil {
		return nil, nil, err
	}
	remaining := r.int("expiry_unix") - s.nowUnix()
	if remaining < 0 {
		remaining = 0
	}
	lease := &Lease{ID: r.int("id"), TTL: r.int("ttl"), Expiry: remaining}
	var kvs []KV
	if keys {
		kvs, err = kvsFromResult(resp.Results[1])
		if err != nil {
			return nil, nil, err
		}
	}
	return lease, kvs, nil
}

// LeaseLeases lists all lease ids.
func (s *Store) LeaseLeases(ctx context.Context) ([]int64, error) {
	resp, err := s.client.Read(ctx, Statement{SQL: `SELECT id FROM leases ORDER BY id`})
	if err != nil {
		return nil, err
	}
	rows, err := resp.Results[0].rows()
	if err != nil {
		return nil, err
	}
	ids := make([]int64, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.int("id"))
	}
	return ids, nil
}

func (s *Store) leaseTTL(ctx context.Context, id int64) (int64, error) {
	resp, err := s.client.Read(ctx, Statement{
		SQL:    `SELECT ttl FROM leases WHERE id = :id`,
		Params: map[string]any{"id": id},
	})
	if err != nil {
		return 0, err
	}
	if len(resp.Results[0].Values) == 0 {
		return 0, rpctypes.ErrGRPCLeaseNotFound
	}
	r, err := resp.Results[0].row(0)
	if err != nil {
		return 0, err
	}
	return r.int("ttl"), nil
}

// expireLeaseStatements atomically deletes a lease's keys, records the delete
// events at the next revision and removes the lease. whenExpired is 0 for an
// explicit revoke and the expiry deadline for the reaper, which guards against
// a lease that was renewed between the scan and the write.
func (s *Store) expireLeaseStatements(id, whenExpired int64) []Statement {
	params := map[string]any{"id": id}
	leaseGuard := `EXISTS(SELECT 1 FROM leases WHERE id = :id`
	if whenExpired > 0 {
		params["now"] = whenExpired
		leaseGuard += ` AND expiry_unix <= :now`
	}
	leaseGuard += `)`
	rev := `(SELECT value FROM k8e_compat_meta WHERE name = 'revision')`

	history := `INSERT INTO kv_history(key, mod_revision, version, deleted, value, lease, create_revision) ` +
		`SELECT key, ` + rev + ` + 1, version, 1, NULL, lease, create_revision FROM kv WHERE lease = :id AND ` + leaseGuard

	event := `INSERT INTO events(revision, seq, kind, key, value, create_revision, mod_revision, version, lease, ` +
		`prev_value, prev_create_revision, prev_mod_revision, prev_version, prev_lease) ` +
		`SELECT ` + rev + ` + 1, ` + seqExpr(rev+" + 1", "ROW_NUMBER() OVER (ORDER BY key) - 1 + ") + `, 'DELETE', key, NULL, 0, 0, 0, 0, ` +
		`value, create_revision, mod_revision, version, lease ` +
		`FROM kv WHERE lease = :id AND ` + leaseGuard

	return []Statement{
		{SQL: `SELECT EXISTS(SELECT 1 FROM leases WHERE id = :id) AS present`, Params: params},
		{SQL: `DELETE FROM op_prev`},
		{SQL: history, Params: params},
		{SQL: event, Params: params},
		{SQL: `DELETE FROM kv WHERE lease = :id AND ` + leaseGuard, Params: params},
		{SQL: bumpSQL},
		{SQL: `DELETE FROM leases WHERE id = :id AND ` + leaseGuard, Params: params},
		{SQL: readRevisionSQL},
	}
}

// ExpireDue expires every lease whose deadline has passed and returns how many
// leases were reaped. Each lease expires in its own transaction, so a crash
// mid-sweep leaves only already-expired leases for the next run.
func (s *Store) ExpireDue(ctx context.Context) (int, error) {
	resp, err := s.client.Read(ctx, Statement{
		SQL:    `SELECT id FROM leases WHERE expiry_unix <= :now ORDER BY id`,
		Params: map[string]any{"now": s.nowUnix()},
	})
	if err != nil {
		return 0, err
	}
	rows, err := resp.Results[0].rows()
	if err != nil {
		return 0, err
	}
	reaped := 0
	now := s.nowUnix()
	for _, r := range rows {
		id := r.int("id")
		if _, err := s.client.Write(ctx, s.expireLeaseStatements(id, now)...); err != nil {
			return reaped, err
		}
		// A keep-alive that landed between the scan and the write wins: the
		// lease still exists, so it was not reaped.
		if _, err := s.leaseTTL(ctx, id); err != nil {
			if errors.Is(err, rpctypes.ErrGRPCLeaseNotFound) {
				reaped++
				continue
			}
			return reaped, err
		}
	}
	return reaped, nil
}

// StartExpiry runs the lease reaper until ctx is cancelled. It is the single
// owner of expiry for this process; M2 may move ownership to a leader lease.
func (s *Store) StartExpiry(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = time.Second
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_, _ = s.ExpireDue(ctx)
			}
		}
	}()
}
