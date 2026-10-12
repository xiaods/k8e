package tandem

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gorilla/mux"
	k8e "github.com/xiaods/k8e/pkg/apis/k8e.sh/v1"
	"github.com/xiaods/k8e/pkg/cluster/managed"
	"github.com/xiaods/k8e/pkg/server/auth"
	"github.com/xiaods/k8e/pkg/version"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The snapshot operations the k8e etcd-snapshot CLI sends over /db/snapshot.
const (
	snapshotOperationSave   = "save"
	snapshotOperationList   = "list"
	snapshotOperationPrune  = "prune"
	snapshotOperationDelete = "delete"
)

// defaultSnapshotBase is used when neither the CLI nor the control config names
// a snapshot.
const defaultSnapshotBase = "on-demand"

// maxSnapshotRequest bounds the little JSON body the CLI sends.
const maxSnapshotRequest = 64 << 10

// snapshotRequest mirrors the body the k8e etcd-snapshot CLI posts to
// /db/snapshot (pkg/etcd.SnapshotRequest). The CLI is shared with the
// embedded-etcd driver, so the tandem endpoint accepts the same protocol; it is
// declared here rather than imported so the driver that replaces embedded etcd
// does not depend on it.
type snapshotRequest struct {
	Operation string          `json:"operation"`
	Name      []string        `json:"name,omitempty"`
	Dir       *string         `json:"dir,omitempty"`
	Compress  *bool           `json:"compress,omitempty"`
	Retention *int            `json:"retention,omitempty"`
	S3        json.RawMessage `json:"s3,omitempty"`
}

// snapshotSettings is one resolved snapshot request: the CLI overrides applied
// on top of the server's own configuration.
type snapshotSettings struct {
	dir       string
	base      string
	compress  bool
	retention int
}

// Register mounts the snapshot API the k8e etcd-snapshot CLI drives. The route,
// the method and the role are the ones the embedded-etcd driver serves, so the
// CLI works unchanged against either backend.
func (d *Driver) Register(handler http.Handler) (http.Handler, error) {
	if d.control == nil {
		return nil, fmt.Errorf("tandem driver has no control config")
	}
	r := mux.NewRouter().SkipClean(true)
	r.NotFoundHandler = handler

	sr := r.Path("/db/snapshot").Subrouter()
	sr.Use(auth.HasRole(d.control, version.Program+":server"))
	sr.Handle("", d.snapshotHandler())

	return r, nil
}

// Snapshot takes an on-demand backup of this node's rqlite store using the
// server's configured snapshot directory, name, compression and retention.
func (d *Driver) Snapshot(ctx context.Context) (*managed.SnapshotResult, error) {
	settings, err := d.snapshotSettings(nil)
	if err != nil {
		return nil, err
	}
	return d.save(ctx, settings)
}

func (d *Driver) snapshotHandler() http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		request, err := readSnapshotRequest(req)
		if err != nil {
			sendSnapshotError(rw, http.StatusBadRequest, err)
			return
		}
		// S3 is not a tantem feature yet, and KIP-29 keeps it optional. Saying
		// so is the only honest answer: a local file would look like a success
		// while the backup the operator asked for was never uploaded.
		if hasS3(request.S3) {
			sendSnapshotError(rw, http.StatusBadRequest,
				fmt.Errorf("the tandem backend does not upload snapshots to S3; unset the S3 options or use the embedded-etcd backend"))
			return
		}
		settings, err := d.snapshotSettings(request)
		if err != nil {
			sendSnapshotError(rw, http.StatusBadRequest, err)
			return
		}
		switch request.Operation {
		case snapshotOperationSave:
			result, err := d.save(req.Context(), settings)
			respondSnapshot(rw, result, err)
		case snapshotOperationList:
			list, err := listSnapshots(settings.dir)
			if err != nil {
				sendSnapshotError(rw, http.StatusInternalServerError, err)
				return
			}
			writeSnapshotJSON(rw, http.StatusOK, list)
		case snapshotOperationPrune:
			deleted, err := pruneSnapshots(settings.dir, settings.base, settings.retention)
			respondSnapshot(rw, &managed.SnapshotResult{Deleted: deleted}, err)
		case snapshotOperationDelete:
			result, err := deleteSnapshots(settings.dir, request.Name)
			respondSnapshot(rw, result, err)
		default:
			sendSnapshotError(rw, http.StatusBadRequest,
				fmt.Errorf("invalid snapshot operation %q", request.Operation))
		}
	})
}

// snapshotSettings resolves a request against the control configuration. A nil
// request yields the server defaults, which is what Driver.Snapshot uses.
func (d *Driver) snapshotSettings(request *snapshotRequest) (snapshotSettings, error) {
	if d.control == nil {
		return snapshotSettings{}, fmt.Errorf("tandem driver has no control config")
	}
	settings := snapshotSettings{
		base:      d.control.EtcdSnapshotName,
		dir:       d.control.EtcdSnapshotDir,
		compress:  d.control.EtcdSnapshotCompress,
		retention: d.control.EtcdSnapshotRetention,
	}
	if settings.base == "" {
		settings.base = defaultSnapshotBase
	}
	if settings.dir == "" {
		settings.dir = filepath.Join(d.control.DataDir, "db", "snapshots")
	}
	if request == nil {
		return settings, nil
	}
	if len(request.Name) > 0 && request.Name[0] != "" {
		settings.base = request.Name[0]
	}
	if request.Dir != nil && *request.Dir != "" {
		settings.dir = *request.Dir
	}
	if request.Compress != nil {
		settings.compress = *request.Compress
	}
	if request.Retention != nil {
		settings.retention = *request.Retention
	}
	return settings, nil
}

// save writes one backup and then applies local retention to it. Retention runs
// after a successful backup only: pruning first would delete a good snapshot
// when the new one fails.
func (d *Driver) save(ctx context.Context, settings snapshotSettings) (*managed.SnapshotResult, error) {
	if d.supervisor.cfg.RqliteHTTP == "" {
		return nil, fmt.Errorf("tandem cannot take a snapshot: the local rqlite node is not running")
	}
	if err := os.MkdirAll(settings.dir, 0700); err != nil {
		return nil, fmt.Errorf("tandem snapshot dir %s: %w", settings.dir, err)
	}
	name := snapshotFileName(settings.base, snapshotNodeName(), time.Now(), settings.compress)
	path := filepath.Join(settings.dir, name)
	if _, err := d.supervisor.Backup(ctx, path, BackupOptions{Compress: settings.compress}); err != nil {
		return nil, err
	}
	deleted, err := pruneSnapshots(settings.dir, settings.base, settings.retention)
	if err != nil {
		// The new snapshot exists, so report it together with the retention
		// failure rather than pretending the whole operation failed.
		return &managed.SnapshotResult{Created: []string{name}}, err
	}
	return &managed.SnapshotResult{Created: []string{name}, Deleted: deleted}, nil
}

func readSnapshotRequest(req *http.Request) (*snapshotRequest, error) {
	if req.Method != http.MethodPost {
		return nil, fmt.Errorf("method %s not allowed", req.Method)
	}
	body, err := io.ReadAll(io.LimitReader(req.Body, maxSnapshotRequest))
	if err != nil {
		return nil, err
	}
	request := &snapshotRequest{}
	if err := json.Unmarshal(body, request); err != nil {
		return nil, fmt.Errorf("invalid snapshot request: %w", err)
	}
	return request, nil
}

func respondSnapshot(rw http.ResponseWriter, result *managed.SnapshotResult, err error) {
	if err != nil {
		sendSnapshotError(rw, http.StatusInternalServerError, err)
		return
	}
	writeSnapshotJSON(rw, http.StatusOK, result)
}

func writeSnapshotJSON(rw http.ResponseWriter, status int, body any) {
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(status)
	_ = json.NewEncoder(rw).Encode(body)
}

func sendSnapshotError(rw http.ResponseWriter, status int, err error) {
	writeSnapshotJSON(rw, status, map[string]string{"message": err.Error()})
}

// hasS3 reports whether the CLI asked for S3, which the request carries as a
// nested object rather than a flag.
func hasS3(s3 json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(s3))
	return trimmed != "" && trimmed != "null"
}

// snapshotFileName builds the on-disk name of one backup. The embedded-etcd
// driver uses the same <name>-<node>-<unix> shape, so an operator reads the
// same thing from either backend; the extension follows the content.
func snapshotFileName(base, node string, now time.Time, compressed bool) string {
	name := fmt.Sprintf("%s-%s-%d", base, node, now.Unix())
	if compressed {
		return name + ".gz"
	}
	return name + ".db"
}

// snapshotNodeName is the node identity recorded in the file name. The server
// process exports NODE_NAME, which the etcd driver also uses; a bare hostname is
// the fallback for an on-demand CLI run outside the server.
func snapshotNodeName() string {
	if name := os.Getenv("NODE_NAME"); name != "" {
		return name
	}
	if name, err := os.Hostname(); err == nil && name != "" {
		return name
	}
	return "node"
}

// listSnapshots describes the local backup files in the CLI's own list shape.
// A missing directory is an empty list: no backup has been taken yet.
func listSnapshots(dir string) (*k8e.ETCDSnapshotFileList, error) {
	list := &k8e.ETCDSnapshotFileList{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return list, nil
		}
		return nil, err
	}
	for _, entry := range entries {
		if entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		size := resource.NewQuantity(info.Size(), resource.BinarySI)
		created := metav1.NewTime(info.ModTime())
		ready := true
		list.Items = append(list.Items, k8e.ETCDSnapshotFile{
			Spec: k8e.ETCDSnapshotSpec{
				SnapshotName: entry.Name(),
				NodeName:     snapshotNodeFromFile(entry.Name()),
				Location:     "file://" + filepath.Join(dir, entry.Name()),
			},
			Status: k8e.ETCDSnapshotStatus{
				Size:         size,
				CreationTime: &created,
				ReadyToUse:   &ready,
			},
		})
	}
	return list, nil
}

// snapshotNodeFromFile recovers the node name from a <base>-<node>-<unix> file
// name. An unrecognised name reports an empty node rather than guessing.
func snapshotNodeFromFile(name string) string {
	name = strings.TrimSuffix(name, ".gz")
	parts := strings.Split(name, "-")
	if len(parts) < 3 {
		return ""
	}
	return strings.Join(parts[1:len(parts)-1], "-")
}

// deleteSnapshots removes the named files. A name that is not present is not an
// error: the CLI reports it as "not found", which is what the operator needs to
// know, and the embedded-etcd driver behaves the same way.
func deleteSnapshots(dir string, names []string) (*managed.SnapshotResult, error) {
	result := &managed.SnapshotResult{}
	for _, name := range names {
		path := filepath.Join(dir, filepath.Base(name))
		if err := os.Remove(path); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return result, err
		}
		result.Deleted = append(result.Deleted, name)
	}
	return result, nil
}

// pruneSnapshots keeps the newest retention backups whose name starts with the
// configured base name and removes the rest, returning the removed names. A
// retention below one keeps everything: an explicit save passes 0 and must
// never delete the backup it just took.
func pruneSnapshots(dir, base string, retention int) ([]string, error) {
	if retention < 1 {
		return nil, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	type backup struct {
		name string
		mod  time.Time
	}
	var backups []backup
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), base+"-") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		backups = append(backups, backup{name: entry.Name(), mod: info.ModTime()})
	}
	sort.Slice(backups, func(i, j int) bool {
		if backups[i].mod.Equal(backups[j].mod) {
			return backups[i].name < backups[j].name
		}
		return backups[i].mod.Before(backups[j].mod)
	})
	var deleted []string
	for _, old := range backups[:max(0, len(backups)-retention)] {
		if err := os.Remove(filepath.Join(dir, old.name)); err != nil {
			return deleted, err
		}
		deleted = append(deleted, old.name)
	}
	return deleted, nil
}
