package tandem

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
)

// rqliteBackupTimeout bounds a /db/backup download. A backup streams the whole
// SQLite file, so this has to scale with database size rather than the one
// second the readiness probes use. The supervisor's HealthTimeout is the
// caller-configurable half of this; this is only the floor used by the
// snapshot handler, which has no caller-provided deadline.
const rqliteBackupTimeout = 10 * time.Minute

// rqliteLoadTimeout bounds a /db/load upload. Loading a SQLite file replays it
// through Raft, which is slower than reading it; rqlite documents load as
// CPU- and memory-intensive as the file approaches 100MB.
const rqliteLoadTimeout = 10 * time.Minute

// sqliteMagic identifies a SQLite database file at byte offset 0. /db/backup
// serves one, and /db/load requires one, so a restore path that is not a
// SQLite file has to be refused before the store is destroyed.
var sqliteMagic = []byte("SQLite format 3\x00")

// Backup streams a consistent snapshot of the local rqlite node to dest.
//
// rqlite's GET /db/backup serves the underlying SQLite file, which is the
// closest thing to an etcd snapshot the rqlite backend has — the etcd snapshot
// RPC is deliberately Unimplemented on this layer (see
// pkg/tandem/differential/known_differences.go) because the formats are not
// interchangeable. KIP-29 §7 maps this endpoint onto the K8E snapshot flow.
//
// The request goes to the node this process supervises. rqlite transparently
// forwards a backup request from a follower to the leader, so a node that is
// not the leader still produces a complete backup; noleader is deliberately
// not requested, as a backup of one unreplicated follower would be a
// point-in-time lie about the cluster.
//
// A streaming endpoint cannot signal failure with a status code once the body
// has started, so the caller checks X-Stream-Error on a short read.
func (s *Supervisor) Backup(ctx context.Context, dest string) error {
	if s.cfg.RqliteHTTP == "" {
		return fmt.Errorf("tandem backup: no rqlite node is configured")
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0700); err != nil {
		return fmt.Errorf("tandem backup directory: %w", err)
	}
	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("tandem backup file %s: %w", dest, err)
	}
	defer out.Close()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rqliteURL(s.cfg.RqliteHTTP)+"/db/backup", http.NoBody)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("tandem backup request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("tandem backup: rqlite answered %s", resp.Status)
	}
	if _, err := io.Copy(out, resp.Body); err != nil {
		// rqlite sets X-Stream-Error when the stream breaks partway through,
		// because a 500 cannot be sent after the body began. Prefer its
		// explanation over a generic copy error, and never leave a partial
		// file behind that a restore would later treat as a backup.
		_ = os.Remove(dest)
		if streamErr := resp.Header.Get("X-Stream-Error"); streamErr != "" {
			return fmt.Errorf("tandem backup: rqlite stream failed: %s", streamErr)
		}
		return fmt.Errorf("tandem backup: %w", err)
	}
	return nil
}

// Load replaces the rqlite node at base with the SQLite file at src.
//
// rqlite's POST /db/load accepts a SQLite file as application/octet-stream and
// replaces everything in the store. It requires a fresh cluster with no other
// write traffic: loading into a store that already holds data silently
// replaces it, and a concurrent write produces an undefined result. This
// driver therefore only ever loads into a node it has not started yet, and
// only ever on a node whose data directory was emptied first — the
// cluster-reset restore path. Loading into a running cluster would be a
// second, unsynchronized writer.
//
// base is the node to load into. The supervised pair's own address is the
// normal case during startup; a temporary node's address is the restore
// path's, where the supervised pair has not started yet.
//
// The node does not have to be the leader: rqlite forwards a load to the
// leader and replicates it through Raft. This driver loads before the node
// joins any group, so that path is not exercised in production.
func (s *Supervisor) Load(ctx context.Context, base, src string) error {
	if base == "" {
		return fmt.Errorf("tandem restore: no rqlite node address")
	}
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("tandem restore: open %s: %w", src, err)
	}
	defer in.Close()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/db/load", in)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("tandem restore request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("tandem restore: rqlite answered %s: %s", resp.Status, body)
	}
	return nil
}

// isSQLiteFile reports whether path begins with the SQLite database magic. A
// restore path that is not one is refused before the existing data directory
// is moved aside, so an operator pointing at the wrong file does not lose the
// store they were trying to protect.
func isSQLiteFile(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	header := make([]byte, len(sqliteMagic))
	if _, err := io.ReadFull(f, header); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return false, nil
		}
		return false, err
	}
	return string(header) == string(sqliteMagic), nil
}

// tandemSnapshotRetention prunes local backups past the configured count,
// newest-first. It mirrors the etcd backend's snapshotRetention so a cluster
// that switches backends keeps one retention policy. It is a local copy
// because importing pkg/etcd would pull the etcd runtime into the tandem
// build, and because the rqlite backend does not keep the etcd snapshot
// metadata files that version references.
func tandemSnapshotRetention(retention int, snapshotPrefix, snapshotDir string) ([]string, error) {
	if retention < 1 {
		return nil, nil
	}
	logrus.Infof("Applying tandem snapshot retention=%d to local snapshots with prefix %s in %s", retention, snapshotPrefix, snapshotDir)

	type snapFile struct {
		name      string
	createdAt time.Time
	}
	var snapshotFiles []snapFile
	if err := filepath.Walk(snapshotDir, func(path string, info os.FileInfo, err error) error {
		if info.IsDir() || err != nil {
			return err
		}
		if !strings.HasPrefix(info.Name(), snapshotPrefix) {
			return nil
		}
		ts := info.ModTime().Unix()
		base := strings.TrimSuffix(info.Name(), ".zip")
		if i := strings.LastIndex(base, "-"); i >= 0 {
			if parsed, perr := strconv.ParseInt(base[i+1:], 10, 64); perr == nil {
				ts = parsed
			}
		}
		snapshotFiles = append(snapshotFiles, snapFile{name: info.Name(), createdAt: time.Unix(ts, 0)})
		return nil
	}); err != nil {
		return nil, err
	}
	if len(snapshotFiles) <= retention {
		return nil, nil
	}
	sort.Slice(snapshotFiles, func(i, j int) bool {
		return snapshotFiles[j].createdAt.Before(snapshotFiles[i].createdAt)
	})
	var deleted []string
	for _, df := range snapshotFiles[retention:] {
		snapshotPath := filepath.Join(snapshotDir, df.name)
		logrus.Infof("Removing local tandem snapshot %s", snapshotPath)
		if err := os.Remove(snapshotPath); err != nil {
			return deleted, err
		}
		deleted = append(deleted, df.name)
	}
	return deleted, nil
}
