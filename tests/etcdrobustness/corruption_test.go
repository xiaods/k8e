package etcdrobustness

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
)

// TestEmbeddedEtcdWALCorruptionIsRefused covers the disk-fault boundary: a node
// whose WAL was damaged must never come up serving a store.
//
// Two damage sites are exercised because they fail differently:
//
//   - the first (metadata) record makes etcd 3.7.1-k3s1 panic while loading it;
//   - the tail of the written region makes etcd refuse with a record decode
//     error.
//
// A refused or crashed start is a fail-fast observation, not a pass for the
// node; the test records which one it saw. The damage is injected into a copy,
// so the test also proves the untouched data directory still recovers.
func TestEmbeddedEtcdWALCorruptionIsRefused(t *testing.T) {
	dir := workDir(t)
	clientURL, peerURL := reserveURLs(t)
	opts := NodeOptions{Name: "robustness", Dir: dir, ClientURL: clientURL, PeerURL: peerURL}

	node := startNode(t, opts)
	client := mustClient(t, clientURL)

	const records = 50
	writeCtx, writeCancel := context.WithTimeout(context.Background(), 60*time.Second)
	for i := 0; i < records; i++ {
		key := fmt.Sprintf("corrupt/%03d", i)
		value := fmt.Sprintf("value-%03d-%s", i, strings.Repeat("c", 16))
		if _, err := client.Put(writeCtx, key, value); err != nil {
			writeCancel()
			t.Fatal(err)
		}
	}
	writeCancel()
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	node.Close()

	damageSites := []struct {
		name     string
		offsetOf func(written int64) int64
	}{
		{name: "metadata-record", offsetOf: func(int64) int64 { return 16 }},
		{name: "written-tail", offsetOf: func(written int64) int64 { return written - 17 }},
	}
	for i, site := range damageSites {
		site := site
		t.Run(site.name, func(t *testing.T) {
			corruptDir := filepath.Join(dir, fmt.Sprintf("corrupt-%d", i))
			if err := copyTree(opts.DataDir(), filepath.Join(corruptDir, "data")); err != nil {
				t.Fatalf("copy data directory: %v", err)
			}
			walPath := newestWAL(t, filepath.Join(corruptDir, "data"))
			corruptWAL(t, walPath, site.offsetOf)
			damagedSize := fileSize(t, walPath)

			// The damaged copy uses its own ports so the failure cannot
			// interfere with the healthy original.
			corruptClientURL, corruptPeerURL := reserveURLs(t)
			corruptNode, corruptErr, panicValue := startCorruptedNode(NodeOptions{
				Name:         "robustness",
				Dir:          corruptDir,
				ClientURL:    corruptClientURL,
				PeerURL:      corruptPeerURL,
				CorruptCheck: true,
			})
			if corruptNode != nil {
				corruptNode.Close()
				t.Fatal("a node with a damaged WAL started serving; the damage was ignored or the store was reset")
			}
			switch {
			case panicValue != nil:
				t.Logf("damaged WAL refused by crash while loading it: %v", panicValue)
			case corruptErr == nil:
				t.Fatal("the damaged node neither returned an error nor panicked")
			case !isWALDamage(corruptErr):
				t.Fatalf("damaged WAL start error = %v; want a checksum or record decode failure. "+
					"A readiness timeout here means the member started but never became ready: it must fail fast instead", corruptErr)
			default:
				t.Logf("damaged WAL refused with a clean startup error: %v", corruptErr)
			}
			if size := fileSize(t, walPath); size != damagedSize {
				// Observed on etcd 3.7.1-k3s1: the WAL repair path rewrites the
				// segment before the node fails. Recorded, because a repair that
				// destroys evidence is itself a robustness result.
				t.Logf("the failed start rewrote the damaged WAL: %d -> %d bytes", damagedSize, size)
			}
		})
	}

	// The healthy original still starts and still has every record.
	startNode(t, opts)
	originalClient := mustClient(t, clientURL)
	verifyCtx, verifyCancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer verifyCancel()
	count, err := originalClient.Get(verifyCtx, "corrupt/", clientv3.WithPrefix(), clientv3.WithCountOnly())
	if err != nil {
		t.Fatal(err)
	}
	if count.Count != records {
		t.Fatalf("the untouched data directory returned %d keys, want %d", count.Count, records)
	}
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return info.Size()
}

// walDamageSignatures are etcd's startup failures when a WAL byte inside the
// written region is flipped: a record checksum mismatch, or an undecodable
// record when the flip lands in the framing.
var walDamageSignatures = []string{"crc mismatch", "cannot parse invalid wire-format data"}

func isWALDamage(err error) bool {
	for _, signature := range walDamageSignatures {
		if strings.Contains(err.Error(), signature) {
			return true
		}
	}
	return false
}

// startCorruptedNode reports what happens when a node starts against a damaged
// data directory. etcd panics while loading some damaged WAL records, so the
// panic is captured instead of taking the test binary down with it.
func startCorruptedNode(opts NodeOptions) (*Node, error, any) {
	var (
		node       *Node
		err        error
		panicValue any
	)
	func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				panicValue = recovered
			}
		}()
		node, err = StartNode(opts)
	}()
	return node, err, panicValue
}

// newestWAL returns the highest-sequence WAL segment of a data directory.
func newestWAL(t *testing.T, dataDir string) string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dataDir, "member", "wal", "*.wal"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) == 0 {
		t.Fatalf("no WAL segment under %s/member/wal", dataDir)
	}
	sort.Strings(matches)
	return matches[len(matches)-1]
}

// corruptWAL flips bytes inside the region the node actually wrote. A WAL
// segment is preallocated, so corrupting the zero tail would prove nothing:
// etcd would read it as unused space.
func corruptWAL(t *testing.T, path string, offsetOf func(written int64) int64) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	written, err := writtenBytes(file)
	if err != nil {
		t.Fatal(err)
	}
	const width = 16
	offset := offsetOf(written)
	if offset < width || offset+width > written {
		t.Fatalf("corruption offset %d is outside the written region [%d,%d) of %s", offset, width, written, path)
	}
	flipped := make([]byte, width)
	if _, err := file.ReadAt(flipped, offset); err != nil {
		t.Fatal(err)
	}
	for i := range flipped {
		flipped[i] ^= 0xff
	}
	if _, err := file.WriteAt(flipped, offset); err != nil {
		t.Fatal(err)
	}
	if err := file.Sync(); err != nil {
		t.Fatal(err)
	}
}

// writtenBytes is the offset just after the last non-zero byte.
func writtenBytes(file *os.File) (int64, error) {
	info, err := file.Stat()
	if err != nil {
		return 0, err
	}
	size := info.Size()
	const chunk = 64 * 1024
	buffer := make([]byte, chunk)
	for end := size; end > 0; {
		start := end - chunk
		if start < 0 {
			start = 0
		}
		n := int(end - start)
		if _, err := file.ReadAt(buffer[:n], start); err != nil && err != io.EOF {
			return 0, err
		}
		for i := n - 1; i >= 0; i-- {
			if buffer[i] != 0 {
				return start + int64(i) + 1, nil
			}
		}
		end = start
	}
	return 0, nil
}

// copyTree copies a data directory, preserving permissions, so the fault is
// injected into a copy and the original stays recoverable.
func copyTree(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("stat %s: %w", src, err)
	}
	if err := os.MkdirAll(dst, info.Mode().Perm()); err != nil {
		return fmt.Errorf("create %s: %w", dst, err)
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return fmt.Errorf("read %s: %w", src, err)
	}
	for _, entry := range entries {
		childInfo, err := entry.Info()
		if err != nil {
			return err
		}
		srcPath := filepath.Join(src, entry.Name())
		dstPath := filepath.Join(dst, entry.Name())
		if childInfo.IsDir() {
			if err := copyTree(srcPath, dstPath); err != nil {
				return err
			}
			continue
		}
		if !childInfo.Mode().IsRegular() {
			continue
		}
		if err := copyFile(srcPath, dstPath, childInfo.Mode().Perm()); err != nil {
			return err
		}
	}
	return nil
}

func copyFile(src, dst string, perm os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open %s: %w", src, err)
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, perm)
	if err != nil {
		return fmt.Errorf("create %s: %w", dst, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return fmt.Errorf("copy %s: %w", src, err)
	}
	return out.Close()
}
