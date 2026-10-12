package tandem

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// sqliteMagic is the fixed 16-byte header of an SQLite database file. rqlite
// serves a real SQLite image, so checking it turns a truncated download or an
// error page into a failed backup instead of a file that an operator only
// discovers is useless when they try to restore it.
const sqliteMagic = "SQLite format 3\x00"

// maxErrorBody bounds how much of a failed response is quoted back in an error.
const maxErrorBody = 4 << 10

// BackupOptions selects the variants rqlite documents for GET /db/backup.
// KIP-29 §8.2 pins the endpoint: the backup is a hot copy of the SQLite store,
// and a compressed one is a gzip stream rather than an SQLite image.
type BackupOptions struct {
	// Vacuum asks rqlite to VACUUM the copy before serving it. It costs disk
	// space on the node, so it is never the default.
	Vacuum bool
	// Compress asks rqlite for the gzip-compressed copy (?compress).
	Compress bool
}

// Backup writes a hot copy of this node's rqlite store to dest.
//
// rqlite streams the image and reports a failure that happens after the
// response headers through the trailing X-Stream-Error header, so a 200 status
// alone does not mean the file is complete (the rqlite docs are explicit about
// this, and about periodically testing backups). The image is therefore written
// to a temporary file beside dest, checked for the trailer and for the expected
// file header, and only then renamed into place: an interrupted backup can
// never masquerade as a usable snapshot.
func (s *Supervisor) Backup(ctx context.Context, dest string, opts BackupOptions) (int64, error) {
	url := s.rqliteBase() + "/db/backup"
	var query []string
	if opts.Compress {
		query = append(query, "compress")
	}
	if opts.Vacuum {
		query = append(query, "vacuum")
	}
	if len(query) > 0 {
		url += "?" + strings.Join(query, "&")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	client := &http.Client{}
	defer client.CloseIdleConnections()
	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("rqlite backup: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("rqlite backup: %s: %s", resp.Status, errorBody(resp.Body))
	}

	tmp, err := os.CreateTemp(filepath.Dir(dest), ".backup-*")
	if err != nil {
		return 0, err
	}
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
	}()

	written, err := io.Copy(tmp, resp.Body)
	if err != nil {
		return 0, fmt.Errorf("rqlite backup: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return 0, err
	}
	if err := tmp.Close(); err != nil {
		return 0, err
	}
	// The trailer is only complete once the body has been read to the end.
	if streamErr := resp.Trailer.Get("X-Stream-Error"); streamErr != "" {
		return 0, fmt.Errorf("rqlite backup: %s", streamErr)
	}
	if err := verifyBackupImage(tmp.Name(), opts.Compress); err != nil {
		return 0, err
	}
	if err := os.Rename(tmp.Name(), dest); err != nil {
		return 0, err
	}
	return written, nil
}

// RestoreFrom replaces this node's store with the backup image at src, through
// rqlite's documented load endpoint. A compressed backup is decompressed first:
// /db/load accepts an SQLite file or an SQL dump, not a gzip stream.
//
// The rqlite docs are explicit that a load replaces all data and belongs on a
// freshly deployed (or freshly reset) node with no other writer, so the caller
// is responsible for quiescing the store; this function only performs the load.
func (s *Supervisor) RestoreFrom(ctx context.Context, src string) error {
	image, cleanup, err := openBackupImage(src)
	if err != nil {
		return err
	}
	defer cleanup()

	file, err := os.Open(image)
	if err != nil {
		return err
	}
	defer file.Close()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.rqliteBase()+"/db/load", file)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	client := &http.Client{}
	defer client.CloseIdleConnections()
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("rqlite load: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	if err != nil {
		return fmt.Errorf("rqlite load: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("rqlite load: %s: %s", resp.Status, bytes.TrimSpace(body))
	}
	if err := rqliteBodyError(body); err != nil {
		return fmt.Errorf("rqlite load: %w", err)
	}
	return nil
}

// openBackupImage returns a path to an SQLite image and a function that removes
// any temporary file it created. A gzip-compressed backup is expanded beside
// the temporary directory rather than over the original, which stays untouched
// so a failed restore can be retried with the same file.
func openBackupImage(src string) (string, func(), error) {
	file, err := os.Open(src)
	if err != nil {
		return "", nil, err
	}
	defer file.Close()
	compressed, err := isGzip(file)
	if err != nil {
		return "", nil, err
	}
	if !compressed {
		return src, func() {}, nil
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", nil, err
	}
	reader, err := gzip.NewReader(file)
	if err != nil {
		return "", nil, fmt.Errorf("%s is not a readable gzip backup: %w", src, err)
	}
	defer reader.Close()
	tmp, err := os.CreateTemp("", "tandem-restore-*.sqlite")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
	}
	if _, err := io.Copy(tmp, reader); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("decompress %s: %w", src, err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return "", nil, err
	}
	return tmp.Name(), func() { _ = os.Remove(tmp.Name()) }, nil
}

func isGzip(file *os.File) (bool, error) {
	header := make([]byte, 2)
	n, err := io.ReadFull(file, header)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return false, err
	}
	return n == len(header) && header[0] == 0x1f && header[1] == 0x8b, nil
}

// verifyBackupImage refuses a download that is not the file the caller asked
// for. Both checks are cheap and turn "the backup looked fine" into a fact.
func verifyBackupImage(path string, compressed bool) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	if compressed {
		ok, err := isGzip(file)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("rqlite backup: response is not a gzip stream")
		}
		return nil
	}
	header := make([]byte, len(sqliteMagic))
	if _, err := file.ReadAt(header, 0); err != nil {
		return fmt.Errorf("rqlite backup: truncated image: %w", err)
	}
	if string(header) != sqliteMagic {
		return fmt.Errorf("rqlite backup: response is not an SQLite image")
	}
	return nil
}

// rqliteBase is the loopback HTTP address of the supervised rqlite node.
func (s *Supervisor) rqliteBase() string {
	host := s.cfg.RqliteHTTP
	if host == "" {
		host = "127.0.0.1:4001"
	}
	return rqliteURL(host)
}

// rqliteBodyError reports the failure rqlite records inside a 200 response.
// rqlite signals several failures — including an unreachable leader — as HTTP
// 200 with an error in the body, so the status code alone is not the result.
func rqliteBodyError(body []byte) error {
	if len(bytes.TrimSpace(body)) == 0 {
		return nil
	}
	var response struct {
		Error   string `json:"error"`
		Results []struct {
			Error string `json:"error"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		// A body that is not the documented JSON envelope carries no failure
		// information; the status code already decided the outcome.
		return nil
	}
	if response.Error != "" {
		return errors.New(response.Error)
	}
	for _, result := range response.Results {
		if result.Error != "" {
			return errors.New(result.Error)
		}
	}
	return nil
}

func errorBody(reader io.Reader) string {
	body, _ := io.ReadAll(io.LimitReader(reader, maxErrorBody))
	return string(bytes.TrimSpace(body))
}
