package tandem

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	k8e "github.com/xiaods/k8e/pkg/apis/k8e.sh/v1"
	"github.com/xiaods/k8e/pkg/cluster/managed"
	"github.com/xiaods/k8e/pkg/daemons/config"
)

// snapshotDriver builds a driver whose rqlite node answers the backup endpoint
// with a valid image, so the k8e snapshot protocol can be exercised without a
// real rqlited or a built Tandem binary.
func snapshotDriver(t *testing.T) (*Driver, string) {
	t.Helper()
	dir := t.TempDir()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/db/backup":
			if strings.Contains(r.URL.RawQuery, "compress") {
				_, _ = w.Write(gzipImage(t))
				return
			}
			_, _ = w.Write(sqliteImage())
		case "/db/load":
			_, _ = io.WriteString(w, "{}")
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	snapshots := filepath.Join(dir, "snapshots")
	if err := os.MkdirAll(snapshots, 0700); err != nil {
		t.Fatal(err)
	}
	d := NewDriver()
	d.SetControlConfig(&config.Control{
		DataDir:               dir,
		Runtime:               config.NewRuntime(nil),
		EtcdSnapshotName:      "on-demand",
		EtcdSnapshotDir:       snapshots,
		EtcdSnapshotCron:      "0 */12 * * *",
		EtcdSnapshotRetention: 3,
	})
	d.supervisor.cfg = withDefaults(Config{RqliteHTTP: strings.TrimPrefix(server.URL, "http://")})
	return d, snapshots
}

func postSnapshot(t *testing.T, d *Driver, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/db/snapshot", strings.NewReader(body))
	rec := httptest.NewRecorder()
	d.snapshotHandler().ServeHTTP(rec, req)
	return rec
}

func decodeSnapshotResult(t *testing.T, rec *httptest.ResponseRecorder) *managed.SnapshotResult {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("snapshot request failed: %d %s", rec.Code, rec.Body.String())
	}
	result := &managed.SnapshotResult{}
	if err := json.Unmarshal(rec.Body.Bytes(), result); err != nil {
		t.Fatalf("decode snapshot result: %v", err)
	}
	return result
}

func TestSnapshotSaveWritesABackupForTheRequest(t *testing.T) {
	d, dir := snapshotDriver(t)
	result := decodeSnapshotResult(t, postSnapshot(t, d, `{"operation":"save","name":["pre-upgrade"]}`))
	if len(result.Created) != 1 {
		t.Fatalf("expected one created snapshot, got %v", result.Created)
	}
	name := result.Created[0]
	if !strings.HasPrefix(name, "pre-upgrade-") || !strings.HasSuffix(name, ".db") {
		t.Fatalf("snapshot name %q does not follow <name>-<node>-<unix>.db", name)
	}
	image, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	if !bytes.HasPrefix(image, []byte(sqliteMagic)) {
		t.Fatal("snapshot is not an SQLite image")
	}
}

func TestSnapshotSaveHonoursTheCompressFlag(t *testing.T) {
	d, dir := snapshotDriver(t)
	result := decodeSnapshotResult(t, postSnapshot(t, d, `{"operation":"save","name":["zipped"],"compress":true}`))
	if len(result.Created) != 1 || !strings.HasSuffix(result.Created[0], ".gz") {
		t.Fatalf("compressed snapshot name = %v, want a .gz file", result.Created)
	}
	if _, err := os.Stat(filepath.Join(dir, result.Created[0])); err != nil {
		t.Fatalf("compressed snapshot missing: %v", err)
	}
}

// Driver.Snapshot is the on-demand entry point with no request overrides.
func TestDriverSnapshotUsesTheControlConfiguration(t *testing.T) {
	d, dir := snapshotDriver(t)
	result, err := d.Snapshot(t.Context())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(result.Created) != 1 {
		t.Fatalf("expected one created snapshot, got %v", result.Created)
	}
	if _, err := os.Stat(filepath.Join(dir, result.Created[0])); err != nil {
		t.Fatalf("snapshot not written to the configured directory: %v", err)
	}
}

func TestSnapshotListDescribesLocalFiles(t *testing.T) {
	d, dir := snapshotDriver(t)
	for _, name := range []string{"on-demand-node-1.db", "on-demand-node-2.db.gz"} {
		if err := os.WriteFile(filepath.Join(dir, name), sqliteImage(), 0600); err != nil {
			t.Fatal(err)
		}
	}
	rec := postSnapshot(t, d, `{"operation":"list"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("list failed: %d %s", rec.Code, rec.Body.String())
	}
	list := &k8e.ETCDSnapshotFileList{}
	if err := json.Unmarshal(rec.Body.Bytes(), list); err != nil {
		t.Fatalf("decode snapshot list: %v", err)
	}
	if len(list.Items) != 2 {
		t.Fatalf("expected two snapshots, got %d", len(list.Items))
	}
	locations := map[string]bool{}
	for _, item := range list.Items {
		if !strings.HasPrefix(item.Spec.Location, "file://") {
			t.Errorf("snapshot %s has location %q, want a file:// path", item.Spec.SnapshotName, item.Spec.Location)
		}
		if item.Status.Size == nil || item.Status.Size.Value() != int64(len(sqliteImage())) {
			t.Errorf("snapshot %s has no usable size", item.Spec.SnapshotName)
		}
		if item.Status.CreationTime == nil || item.Status.ReadyToUse == nil || !*item.Status.ReadyToUse {
			t.Errorf("snapshot %s is not reported as ready with a creation time", item.Spec.SnapshotName)
		}
		locations[item.Spec.Location] = true
	}
	if len(locations) != 2 {
		t.Fatalf("snapshot locations are not distinct: %v", locations)
	}
}

func TestSnapshotDeleteRemovesOnlyTheNamedFile(t *testing.T) {
	d, dir := snapshotDriver(t)
	keep := filepath.Join(dir, "on-demand-node-1.db")
	remove := filepath.Join(dir, "on-demand-node-2.db")
	for _, path := range []string{keep, remove} {
		if err := os.WriteFile(path, sqliteImage(), 0600); err != nil {
			t.Fatal(err)
		}
	}
	result := decodeSnapshotResult(t, postSnapshot(t, d, `{"operation":"delete","name":["on-demand-node-2.db"]}`))
	if len(result.Deleted) != 1 || result.Deleted[0] != "on-demand-node-2.db" {
		t.Fatalf("deleted = %v, want the named snapshot", result.Deleted)
	}
	if _, err := os.Stat(remove); !os.IsNotExist(err) {
		t.Fatal("the named snapshot still exists")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("an unrelated snapshot was removed: %v", err)
	}
}

// A name is CLI input, so it must never escape the snapshot directory.
func TestSnapshotDeleteCannotEscapeTheSnapshotDirectory(t *testing.T) {
	d, dir := snapshotDriver(t)
	outside := filepath.Join(filepath.Dir(dir), "secret.db")
	if err := os.WriteFile(outside, sqliteImage(), 0600); err != nil {
		t.Fatal(err)
	}
	postSnapshot(t, d, `{"operation":"delete","name":["../secret.db"]}`)
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("a snapshot name escaped its directory: %v", err)
	}
}

func TestSnapshotPruneKeepsTheNewest(t *testing.T) {
	d, dir := snapshotDriver(t)
	names := []string{"on-demand-a-1.db", "on-demand-a-2.db", "on-demand-a-3.db", "on-demand-a-4.db"}
	for i, name := range names {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, sqliteImage(), 0600); err != nil {
			t.Fatal(err)
		}
		// Distinct modification times decide which snapshots are the oldest.
		stamp := time.Now().Add(time.Duration(i) * time.Minute)
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	result := decodeSnapshotResult(t, postSnapshot(t, d, `{"operation":"prune","name":["on-demand-a"],"retention":2}`))
	if len(result.Deleted) != 2 {
		t.Fatalf("pruned %v, want the two oldest", result.Deleted)
	}
	for _, name := range names[:2] {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("%s should have been pruned", name)
		}
	}
	for _, name := range names[2:] {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s should have been kept: %v", name, err)
		}
	}
}

// An explicit save passes retention 0 so it cannot delete the backup it just
// took, and prune with retention 0 must therefore keep everything.
func TestSnapshotPruneWithZeroRetentionKeepsEverything(t *testing.T) {
	d, dir := snapshotDriver(t)
	for _, name := range []string{"on-demand-a-1.db", "on-demand-a-2.db"} {
		if err := os.WriteFile(filepath.Join(dir, name), sqliteImage(), 0600); err != nil {
			t.Fatal(err)
		}
	}
	result := decodeSnapshotResult(t, postSnapshot(t, d, `{"operation":"prune","name":["on-demand-a"],"retention":0}`))
	if len(result.Deleted) != 0 {
		t.Fatalf("retention 0 deleted %v", result.Deleted)
	}
}

// S3 is optional in KIP-29 and not implemented for this backend; answering
// with a local file would look like the upload the operator asked for.
func TestSnapshotRefusesS3(t *testing.T) {
	d, dir := snapshotDriver(t)
	rec := postSnapshot(t, d, `{"operation":"save","name":["backup"],"s3":{"bucket":"b"}}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("S3 request returned %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "S3") {
		t.Fatalf("error does not name S3: %s", rec.Body.String())
	}
	entries, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("an S3 request wrote a local snapshot: %v", entries)
	}
}

func TestSnapshotRejectsUnknownOperationsAndMethods(t *testing.T) {
	d, _ := snapshotDriver(t)
	if rec := postSnapshot(t, d, `{"operation":"teleport"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown operation returned %d, want 400", rec.Code)
	}
	req := httptest.NewRequest(http.MethodGet, "/db/snapshot", nil)
	rec := httptest.NewRecorder()
	d.snapshotHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("GET /db/snapshot returned %d, want 400", rec.Code)
	}
}

// Without a running rqlite there is nothing to back up, and saying so is the
// only honest answer.
func TestSnapshotFailsWithoutARunningStore(t *testing.T) {
	d := NewDriver()
	d.SetControlConfig(&config.Control{DataDir: t.TempDir(), Runtime: config.NewRuntime(nil)})
	rec := postSnapshot(t, d, `{"operation":"save"}`)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("save without a store returned %d, want 500", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "rqlite") {
		t.Fatalf("error does not explain the missing store: %s", rec.Body.String())
	}
}

// The CLI reaches the endpoint through the k8e server's mux, so the route has
// to be mounted there rather than only existing as a method.
func TestRegisterMountsTheSnapshotRoute(t *testing.T) {
	d, _ := snapshotDriver(t)
	handler, err := d.Register(http.NotFoundHandler())
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/db/snapshot", strings.NewReader("{}"))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code == http.StatusNotFound {
		t.Fatal("/db/snapshot is not mounted")
	}
}

// A reset has no rqlite equivalent of etcd's force-new-cluster: without a
// backup it would silently destroy the store, so it is refused first.
func TestResetRefusesWithoutABackup(t *testing.T) {
	dir := t.TempDir()
	store := filepath.Join(dir, "tandem", "rqlite")
	if err := os.MkdirAll(store, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store, "raft.db"), []byte("state"), 0600); err != nil {
		t.Fatal(err)
	}
	d := NewDriver()
	d.SetControlConfig(&config.Control{DataDir: dir, Runtime: config.NewRuntime(nil)})
	err := d.Reset(t.Context(), func() error { return nil })
	if err == nil {
		t.Fatal("a reset without a backup must be refused")
	}
	if !strings.Contains(err.Error(), "cluster-reset-restore-path") {
		t.Fatalf("error does not name the required flag: %v", err)
	}
	if _, err := os.Stat(filepath.Join(store, "raft.db")); err != nil {
		t.Fatalf("the refusal must not touch the store: %v", err)
	}
}

func TestRestoreRequiresABackupPath(t *testing.T) {
	d := NewDriver()
	d.SetControlConfig(&config.Control{DataDir: t.TempDir(), Runtime: config.NewRuntime(nil)})
	if err := d.Restore(t.Context()); err == nil {
		t.Fatal("a restore without a path must be refused")
	}
}
