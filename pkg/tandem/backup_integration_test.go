//go:build tandem_integration

package tandem

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xiaods/k8e/pkg/rqlitecompat"
)

// TestTandemBackupRestoresAFreshStore is the M2 stage-3 contract of epic #592
// ("备份确实可恢复 ... 从空数据目录恢复"): a backup taken from a live store must
// load into an empty data directory with its data, its history and its
// revision intact.
//
// Both halves of the contract are exercised against a real rqlited: Backup
// streams the live store and RestoreFrom loads the file into a node that has
// never held data. The store is written through pkg/rqlitecompat — the Go
// reference implementation of the same versioned schema the compatibility
// layer uses — so what is restored is etcd-level data (KV, history, revision)
// rather than rows this test invented.
func TestTandemBackupRestoresAFreshStore(t *testing.T) {
	for _, compress := range []bool{false, true} {
		name := "plain"
		if compress {
			name = "compressed"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			source, _ := startTestRqlite(t, filepath.Join(t.TempDir(), "rqlite"), "", "")
			store := bootstrapTestStore(t, ctx, source)

			key := []byte("/registry/pods/default/web-0")
			first, err := store.Put(ctx, rqlitecompat.PutOptions{Key: key, Value: []byte("v1")})
			if err != nil {
				t.Fatalf("put v1: %v", err)
			}
			second, err := store.Put(ctx, rqlitecompat.PutOptions{Key: key, Value: []byte("v2")})
			if err != nil {
				t.Fatalf("put v2: %v", err)
			}
			if second.Revision != first.Revision+1 {
				t.Fatalf("revisions are not consecutive: %d then %d", first.Revision, second.Revision)
			}

			dest := filepath.Join(t.TempDir(), "on-demand.db")
			if compress {
				dest += ".gz"
			}
			backup := &Supervisor{cfg: withDefaults(Config{RqliteHTTP: strings.TrimPrefix(source, "http://")})}
			if _, err := backup.Backup(ctx, dest, BackupOptions{Compress: compress}); err != nil {
				t.Fatalf("backup: %v", err)
			}

			// A node with an empty data directory: this is the recovery path an
			// operator follows after losing the store.
			fresh, _ := startTestRqlite(t, filepath.Join(t.TempDir(), "rqlite"), "", "")
			restore := &Supervisor{cfg: withDefaults(Config{RqliteHTTP: strings.TrimPrefix(fresh, "http://")})}
			if err := restore.RestoreFrom(ctx, dest); err != nil {
				t.Fatalf("restore into a fresh data directory: %v", err)
			}

			// The restored node is read through a plain store, not a
			// re-bootstrapped one: if the schema had not travelled with the
			// backup, these reads would fail rather than silently start over.
			restored := rqlitecompat.NewStore(rqlitecompat.NewClient(fresh), "restored")
			revision, err := restored.Revision(ctx)
			if err != nil {
				t.Fatalf("read the restored revision: %v", err)
			}
			if revision != second.Revision {
				t.Fatalf("restored revision is %d, want %d", revision, second.Revision)
			}

			head, err := restored.Range(ctx, rqlitecompat.RangeOptions{Key: key})
			if err != nil {
				t.Fatalf("range the restored key: %v", err)
			}
			if len(head.KVs) != 1 {
				t.Fatalf("restored range returned %d keys, want 1", len(head.KVs))
			}
			if got := string(head.KVs[0].Value); got != "v2" {
				t.Fatalf("restored value is %q, want %q", got, "v2")
			}
			if head.KVs[0].ModRevision != second.Revision || head.KVs[0].CreateRevision != first.Revision {
				t.Fatalf("restored revisions are create=%d mod=%d, want create=%d mod=%d",
					head.KVs[0].CreateRevision, head.KVs[0].ModRevision, first.Revision, second.Revision)
			}

			// History has to survive as well: a watch cache that resumes from
			// the restored revision must still see the earlier write.
			past, err := restored.Range(ctx, rqlitecompat.RangeOptions{Key: key, Revision: first.Revision})
			if err != nil {
				t.Fatalf("read history at revision %d: %v", first.Revision, err)
			}
			if len(past.KVs) != 1 || string(past.KVs[0].Value) != "v1" {
				t.Fatalf("history at revision %d returned %v, want the value %q", first.Revision, past.KVs, "v1")
			}
		})
	}
}

// bootstrapTestStore creates the compatibility schema on a real rqlite node and
// returns a store attached to it, the way the M1 layer is brought up.
func bootstrapTestStore(t *testing.T, ctx context.Context, url string) *rqlitecompat.Store {
	t.Helper()
	client := rqlitecompat.NewClient(url)
	if err := client.Ready(ctx, url); err != nil {
		t.Fatalf("rqlite at %s is not ready: %v", url, err)
	}
	if err := rqlitecompat.Bootstrap(ctx, client); err != nil {
		t.Fatalf("bootstrap compatibility schema: %v", err)
	}
	return rqlitecompat.NewStore(client, "tandem-backup-integration")
}
