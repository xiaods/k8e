package tandem

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sqliteImage is the smallest payload that looks like what rqlite serves: a
// real SQLite header followed by opaque bytes.
func sqliteImage() []byte {
	return append([]byte(sqliteMagic), bytes.Repeat([]byte{0x5a}, 64)...)
}

// backupServer stands in for the local rqlite node: only the backup and load
// endpoints this package uses are served, and each test decides how they answer.
func backupServer(t *testing.T, handler http.HandlerFunc) *Supervisor {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return &Supervisor{cfg: withDefaults(Config{RqliteHTTP: strings.TrimPrefix(server.URL, "http://")})}
}

func TestBackupWritesTheImage(t *testing.T) {
	image := sqliteImage()
	s := backupServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/db/backup" {
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
			return
		}
		_, _ = w.Write(image)
	})
	dest := filepath.Join(t.TempDir(), "on-demand.db")
	written, err := s.Backup(context.Background(), dest, BackupOptions{})
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if written != int64(len(image)) {
		t.Fatalf("Backup wrote %d bytes, want %d", written, len(image))
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	if !bytes.Equal(got, image) {
		t.Fatal("backup file does not contain what rqlite served")
	}
	// No temporary file may be left behind beside the snapshot.
	entries, err := os.ReadDir(filepath.Dir(dest))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected one file in the snapshot dir, got %v", entries)
	}
}

// rqlite reports a failure that happens after the response headers in a
// trailing header, so a 200 alone does not mean the backup is complete.
func TestBackupRefusesAStreamError(t *testing.T) {
	s := backupServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Trailer", "X-Stream-Error")
		_, _ = w.Write(sqliteImage())
		w.Header().Set("X-Stream-Error", "I/O timeout")
	})
	dest := filepath.Join(t.TempDir(), "on-demand.db")
	if _, err := s.Backup(context.Background(), dest, BackupOptions{}); err == nil {
		t.Fatal("a stream error must fail the backup")
	} else if !strings.Contains(err.Error(), "I/O timeout") {
		t.Fatalf("error does not carry rqlite's message: %v", err)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatal("a failed backup must not leave a file behind")
	}
}

func TestBackupRefusesANonSQLitePayload(t *testing.T) {
	s := backupServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "<html>leader does not exist</html>")
	})
	dest := filepath.Join(t.TempDir(), "on-demand.db")
	if _, err := s.Backup(context.Background(), dest, BackupOptions{}); err == nil {
		t.Fatal("a payload that is not an SQLite image must fail the backup")
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatal("a failed backup must not leave a file behind")
	}
}

func TestBackupAsksForTheDocumentedParameters(t *testing.T) {
	query := make(chan string, 2)
	s := backupServer(t, func(w http.ResponseWriter, r *http.Request) {
		query <- r.URL.RawQuery
		if strings.Contains(r.URL.RawQuery, "compress") {
			_, _ = w.Write(gzipImage(t))
			return
		}
		_, _ = w.Write(sqliteImage())
	})
	dest := filepath.Join(t.TempDir(), "on-demand.db")
	if _, err := s.Backup(context.Background(), dest, BackupOptions{}); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if got := <-query; got != "" {
		t.Fatalf("a plain backup must not add query parameters, got %q", got)
	}
	if _, err := s.Backup(context.Background(), dest, BackupOptions{Compress: true, Vacuum: true}); err != nil {
		t.Fatalf("Backup(compressed): %v", err)
	}
	if got := <-query; got != "compress&vacuum" {
		t.Fatalf("backup parameters = %q, want compress&vacuum", got)
	}
}

// gzipImage is the compressed form of an SQLite image, which is what rqlite
// serves for ?compress.
func gzipImage(t *testing.T) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := gzip.NewWriter(&buffer)
	if _, err := writer.Write(sqliteImage()); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func TestBackupAcceptsACompressedImage(t *testing.T) {
	compressed := gzipImage(t)
	s := backupServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(compressed)
	})
	dest := filepath.Join(t.TempDir(), "on-demand.db.gz")
	if _, err := s.Backup(context.Background(), dest, BackupOptions{Compress: true}); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if _, err := os.Stat(dest); err != nil {
		t.Fatalf("compressed backup missing: %v", err)
	}
}

func TestRestorePostsTheImage(t *testing.T) {
	type upload struct {
		path        string
		method      string
		contentType string
		body        []byte
	}
	received := make(chan upload, 1)
	s := backupServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		received <- upload{path: r.URL.Path, method: r.Method, contentType: r.Header.Get("Content-Type"), body: body}
		_, _ = io.WriteString(w, `{"results":[{"rows_affected":1}]}`)
	})
	backup := filepath.Join(t.TempDir(), "on-demand.db")
	image := sqliteImage()
	if err := os.WriteFile(backup, image, 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.RestoreFrom(context.Background(), backup); err != nil {
		t.Fatalf("RestoreFrom: %v", err)
	}
	got := <-received
	if got.method != http.MethodPost || got.path != "/db/load" {
		t.Fatalf("restore request was %s %s, want POST /db/load", got.method, got.path)
	}
	if got.contentType != "application/octet-stream" {
		t.Fatalf("restore content type = %q, want application/octet-stream", got.contentType)
	}
	if !bytes.Equal(got.body, image) {
		t.Fatal("restore uploaded different bytes than the backup holds")
	}
}

// A compressed backup is a gzip stream, and /db/load takes a SQLite image, so
// the driver has to expand it first.
func TestRestoreExpandsACompressedBackup(t *testing.T) {
	image := sqliteImage()
	var buffer bytes.Buffer
	writer := gzip.NewWriter(&buffer)
	if _, err := writer.Write(image); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	body := make(chan []byte, 1)
	s := backupServer(t, func(w http.ResponseWriter, r *http.Request) {
		uploaded, _ := io.ReadAll(r.Body)
		body <- uploaded
		_, _ = io.WriteString(w, "{}")
	})
	backup := filepath.Join(t.TempDir(), "on-demand.db.gz")
	if err := os.WriteFile(backup, buffer.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.RestoreFrom(context.Background(), backup); err != nil {
		t.Fatalf("RestoreFrom: %v", err)
	}
	if got := <-body; !bytes.Equal(got, image) {
		t.Fatal("restore must upload the expanded SQLite image")
	}
	// The backup itself is left exactly as it was, so a failed restore can be
	// retried with the same file.
	onDisk, err := os.ReadFile(backup)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(onDisk, buffer.Bytes()) {
		t.Fatal("restore modified the backup file")
	}
}

func TestRestoreReportsAnErrorInTheBody(t *testing.T) {
	s := backupServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"results":[{"error":"load failed: disk I/O error"}]}`)
	})
	backup := filepath.Join(t.TempDir(), "on-demand.db")
	if err := os.WriteFile(backup, sqliteImage(), 0600); err != nil {
		t.Fatal(err)
	}
	err := s.RestoreFrom(context.Background(), backup)
	if err == nil {
		t.Fatal("a body error must fail the restore")
	}
	if !strings.Contains(err.Error(), "disk I/O error") {
		t.Fatalf("error does not carry rqlite's message: %v", err)
	}
}

func TestRestoreRejectsAnHTTPError(t *testing.T) {
	s := backupServer(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "leader not found", http.StatusServiceUnavailable)
	})
	backup := filepath.Join(t.TempDir(), "on-demand.db")
	if err := os.WriteFile(backup, sqliteImage(), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.RestoreFrom(context.Background(), backup); err == nil {
		t.Fatal("a non-200 load must fail")
	}
}
