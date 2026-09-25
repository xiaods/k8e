//go:build tandem_integration

package tandem

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// Exercise the production gRPC -> SQL path against a fresh real rqlite.
func TestTandemPersistentTxn(t *testing.T) {
	binary := os.Getenv("TANDEM_TEST_BINARY")
	if binary == "" {
		t.Fatal("set TANDEM_TEST_BINARY to a Linux Tandem executable")
	}
	endpoint, tls := startTestTandem(t, binary, true)
	ctx, cli, cancel := newTestClient(t, endpoint, tls, 30*time.Second)
	t.Cleanup(cancel)
	// Blocking Dial isn't enough to prove the SQL schema is ready.
	waitForReady(t, ctx, cli, 300*time.Millisecond, 30*time.Second, "schema readiness")
	testTxnCRUD(t, ctx, cli)
	testTxnCAS(t, ctx, cli)
	testTxnRangeLimit(t, ctx, cli)
}

// A Range inside a Txn must honour the op's own limit: etcd truncates the
// page, reports the full count of matching keys and sets more. Only the unary
// Range path did this before, so a paginating controller reading through a
// Txn would have looped forever on a page that never ended.
func testTxnRangeLimit(t *testing.T, ctx context.Context, cli *clientv3.Client) {
	t.Helper()
	seedTxnLimitKeys(t, ctx, cli)
	defer cleanupTxnLimitKeys(t, ctx, cli)

	// A limit below the match count truncates the page, keeps the full count
	// and reports that more pages remain.
	page := txnRangeAtLimit(t, ctx, cli, 2)
	if len(page.Kvs) != 2 {
		t.Fatalf("txn range returned %d keys, want 2 (limit not applied)", len(page.Kvs))
	}
	if page.Count != 3 {
		t.Fatalf("txn range count = %d, want 3 (full match count)", page.Count)
	}
	if !page.More {
		t.Fatal("txn range more = false, want true when the limit truncates the page")
	}
	if string(page.Kvs[0].Key) != "txnlimit/a" || string(page.Kvs[1].Key) != "txnlimit/b" {
		t.Fatalf("txn range page out of order: %q, %q", page.Kvs[0].Key, page.Kvs[1].Key)
	}

	// A limit at or above the match count must not claim more pages.
	untruncated := txnRangeAtLimit(t, ctx, cli, 3)
	if len(untruncated.Kvs) != 3 || untruncated.Count != 3 || untruncated.More {
		t.Fatalf("untruncated txn range: %d keys, count %d, more %v; want 3/3/false", len(untruncated.Kvs), untruncated.Count, untruncated.More)
	}
}

func seedTxnLimitKeys(t *testing.T, ctx context.Context, cli *clientv3.Client) {
	t.Helper()
	for _, key := range []string{"txnlimit/a", "txnlimit/b", "txnlimit/c"} {
		if _, err := cli.Put(ctx, key, "v"); err != nil {
			t.Fatalf("seed %s: %v", key, err)
		}
	}
}

func cleanupTxnLimitKeys(t *testing.T, ctx context.Context, cli *clientv3.Client) {
	t.Helper()
	if _, err := cli.Delete(ctx, "txnlimit/", clientv3.WithPrefix()); err != nil {
		t.Errorf("cleanup: %v", err)
	}
}

// txnRangeAtLimit runs a read-only Txn whose single op is a prefix Range with
// the given limit, and returns what that op answered.
func txnRangeAtLimit(t *testing.T, ctx context.Context, cli *clientv3.Client, limit int64) *pb.RangeResponse {
	t.Helper()
	response, err := cli.Txn(ctx).
		Then(clientv3.OpGet("txnlimit/", clientv3.WithPrefix(), clientv3.WithLimit(limit))).
		Commit()
	if err != nil {
		t.Fatalf("txn range at limit %d: %v", limit, err)
	}
	if !response.Succeeded || len(response.Responses) != 1 {
		t.Fatalf("txn range at limit %d: responses: %+v", limit, response)
	}
	return response.Responses[0].GetResponseRange()
}

func testTxnCRUD(t *testing.T, ctx context.Context, cli *clientv3.Client) {
	t.Helper()
	key, value := "txn/'\x00", "binary\x00\xff;value"
	created := txnCreate(t, ctx, cli, key, value)
	txnReadBack(t, ctx, cli, key, value)
	updated := txnUpdate(t, ctx, cli, key, value, created.Header.Revision)
	txnReadOnlyBranch(t, ctx, cli, key, updated.Header.Revision)
	txnDelete(t, ctx, cli, key)
}

func txnCreate(t *testing.T, ctx context.Context, cli *clientv3.Client, key, value string) *clientv3.TxnResponse {
	t.Helper()
	created, err := cli.Txn(ctx).If(clientv3.Compare(clientv3.Version(key), "=", 0)).
		Then(clientv3.OpPut(key, value)).Else(clientv3.OpPut("never", "bad")).Commit()
	if err != nil || !created.Succeeded || len(created.Responses) != 1 {
		t.Fatalf("create missing key: %+v, %v", created, err)
	}
	return created
}

func txnReadBack(t *testing.T, ctx context.Context, cli *clientv3.Client, key, value string) {
	t.Helper()
	got, err := cli.Get(ctx, key)
	if err != nil || len(got.Kvs) != 1 || string(got.Kvs[0].Value) != value {
		t.Fatalf("binary transaction: %+v, %v", got, err)
	}
}

func txnUpdate(t *testing.T, ctx context.Context, cli *clientv3.Client, key, value string, createRev int64) *clientv3.TxnResponse {
	t.Helper()
	updated, err := cli.Txn(ctx).If(clientv3.Compare(clientv3.Value(key), "=", value)).
		Then(clientv3.OpPut(key, "new", clientv3.WithPrevKV()), clientv3.OpPut("second", "value")).Commit()
	if err != nil || !updated.Succeeded || len(updated.Responses) != 2 {
		t.Fatalf("update: %+v, %v", updated, err)
	}
	prev := updated.Responses[0].GetResponsePut().PrevKv
	if prev == nil || string(prev.Value) != value || prev.ModRevision != createRev {
		t.Fatalf("previous value: %+v", prev)
	}
	for _, response := range updated.Responses {
		if response.GetResponsePut().Header.Revision != updated.Header.Revision {
			t.Fatalf("split revision: %+v", updated)
		}
	}
	return updated
}

func txnReadOnlyBranch(t *testing.T, ctx context.Context, cli *clientv3.Client, key string, updateRev int64) {
	t.Helper()
	readOnly, err := cli.Txn(ctx).If(clientv3.Compare(clientv3.Version(key), "=", 0)).
		Then(clientv3.OpPut("never", "bad")).Else(clientv3.OpGet(key)).Commit()
	if err != nil || readOnly.Succeeded || readOnly.Header.Revision != updateRev {
		t.Fatalf("read-only failure branch advanced revision: %+v, %v", readOnly, err)
	}
}

func txnDelete(t *testing.T, ctx context.Context, cli *clientv3.Client, key string) {
	t.Helper()
	deleted, err := cli.Txn(ctx).Then(clientv3.OpDelete(key)).Commit()
	if err != nil || len(deleted.Responses) != 1 || deleted.Responses[0].GetResponseDeleteRange().Deleted != 1 {
		t.Fatalf("exact key delete: %+v, %v", deleted, err)
	}
	empty, err := cli.Txn(ctx).Then(clientv3.OpDelete(key)).Commit()
	if err != nil || empty.Header.Revision != deleted.Header.Revision || empty.Responses[0].GetResponseDeleteRange().Deleted != 0 {
		t.Fatalf("empty delete: %+v, %v", empty, err)
	}
}

func testTxnCAS(t *testing.T, ctx context.Context, cli *clientv3.Client) {
	t.Helper()
	// Concurrent create-if-absent calls must have exactly one winner and retain
	// their own response; scratch results may not be read by a later HTTP call.
	const contenders = 8
	results := make(chan bool, contenders)
	errors := make(chan error, contenders)
	var wg sync.WaitGroup
	for i := range contenders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := cli.Txn(ctx).If(clientv3.Compare(clientv3.Version("cas"), "=", 0)).
				Then(clientv3.OpPut("cas", fmt.Sprint(i))).Commit()
			if err != nil {
				errors <- err
				return
			}
			results <- result.Succeeded
		}()
	}
	wg.Wait()
	close(results)
	close(errors)
	for err := range errors {
		t.Error(err)
	}
	winners := 0
	for succeeded := range results {
		if succeeded {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("CAS winners = %d, want 1", winners)
	}
}
