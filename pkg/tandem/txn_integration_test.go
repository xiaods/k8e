//go:build tandem_integration

package tandem

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

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
