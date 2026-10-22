package e2b

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	pb "github.com/xiaods/k8e/pkg/sandboxmatrix/grpc/pb/sandbox/v1"
)

// mimeByExtension maps common extensions to content types (real envd serves
// the true type; a bare octet-stream breaks consumers that read the header).
var mimeByExtension = map[string]string{
	"txt": "text/plain; charset=utf-8", "md": "text/markdown; charset=utf-8",
	"csv": "text/csv; charset=utf-8", "html": "text/html; charset=utf-8",
	"htm": "text/html; charset=utf-8", "css": "text/css; charset=utf-8",
	"js": "text/javascript; charset=utf-8", "json": "application/json",
	"xml": "text/xml; charset=utf-8", "pdf": "application/pdf",
	"png": "image/png", "jpg": "image/jpeg", "jpeg": "image/jpeg",
	"gif": "image/gif", "svg": "image/svg+xml", "webp": "image/webp",
	"ico": "image/x-icon", "mp3": "audio/mpeg", "wav": "audio/wav",
	"mp4": "video/mp4", "webm": "video/webm",
	"zip": "application/zip", "gz": "application/gzip",
	"tar": "application/x-tar", "wasm": "application/wasm",
}

func contentTypeOf(name string) string {
	dot := strings.LastIndex(name, ".")
	if dot < 0 {
		return "application/octet-stream"
	}
	if ct, ok := mimeByExtension[strings.ToLower(name[dot+1:])]; ok {
		return ct
	}
	return "application/octet-stream"
}

// handleFiles serves GET/POST /e2b/envd/files and the signed /files door
// (the same handler cores, two doors — see filesSigned vs filesEnvd).
func (s *Server) handleFiles(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodOptions:
		sendPreflight(w, r)
	case http.MethodGet:
		s.serveFileDownload(w, r, sandboxIDOf(r))
	case http.MethodPost:
		s.serveFileUpload(w, r, sandboxIDOf(r))
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// handleSignedFiles serves /files at the daemon root: signature-only auth.
// The signature itself identifies the sandbox (no headers needed — browsers
// add none).
func (s *Server) handleSignedFiles(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodOptions:
		sendPreflight(w, r)
		return
	}
	query := r.URL.Query()
	operation := "read"
	if r.Method == http.MethodPost {
		operation = "write"
	}
	sandboxID, e2e := s.identifySignedSandbox(query, operation)
	if e2e != nil {
		s.writeEnvdError(w, e2e)
		return
	}
	// Expiration is judged after identification so a valid-but-expired
	// signature reports the honest reason, not a generic mismatch.
	if e := checkSignedExpiration(query, signedMaterialFromQuery(query, operation)); e != nil {
		s.writeEnvdError(w, e)
		return
	}
	if r.Method == http.MethodGet {
		s.serveFileDownload(w, r, sandboxID)
		return
	}
	s.serveFileUpload(w, r, sandboxID)
}

// signedMaterialFromQuery rebuilds the signature material from a query.
func signedMaterialFromQuery(query map[string][]string, operation string) signatureMaterial {
	path := firstValue(query, "path")
	username := firstValue(query, "username")
	var expiration *int64
	if raw := firstValue(query, "signature_expiration"); raw != "" {
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
			expiration = &n
		}
	}
	return signatureMaterial{path: path, operation: operation, username: username, expirationUnix: expiration}
}

// identifySignedSandbox recovers the sandbox a signed URL speaks for by
// scanning the registered sandboxes' tokens (each sandbox's access token
// differs, so the signature binds the sandbox; at most N hashes per request).
func (s *Server) identifySignedSandbox(query map[string][]string, operation string) (string, *E2bError) {
	sig := firstValue(query, "signature")
	if sig == "" {
		return "", connectError("unauthenticated", "missing signature query parameter")
	}
	material := signedMaterialFromQuery(query, operation)
	for _, id := range s.registry.ids() {
		if signatureMatches(sig, id, s.signingSecret, material) {
			return id, nil
		}
	}
	return "", connectError("unauthenticated", "invalid signature")
}

// checkSignedExpiration applies the expiration half once the sandbox is
// identified.
func checkSignedExpiration(query map[string][]string, material signatureMaterial) *E2bError {
	return checkSignatureExpiration(material)
}

func firstValue(query map[string][]string, key string) string {
	if vals, ok := query[key]; ok && len(vals) > 0 {
		return vals[0]
	}
	return ""
}

// serveFileDownload streams a file out: GET /files?path=.
func (s *Server) serveFileDownload(w http.ResponseWriter, r *http.Request, sandboxID string) {
	// Auto-resume a paused sandbox (CubeSandbox's auto_resume on file I/O).
	if _, _, e2e := s.wakeForTraffic(r, sandboxID); e2e != nil {
		s.writeEnvdError(w, e2e)
		return
	}
	path, err := resolveSandboxPath(r.URL.Query().Get("path"))
	if err != nil {
		s.writeEnvdError(w, connectError("invalid_argument", err.Error()))
		return
	}
	// Ask for a binary-safe payload. The legacy path returns the file as
	// escaped JSON text, which cannot carry non-UTF-8 bytes: the gateway's
	// ReadFileResponse.content is a proto string, so raw binary fails to
	// marshal and the SDK received an EMPTY file for every binary payload.
	// sandboxd's base64 encoding is byte-faithful, so it is the only correct
	// way to return arbitrary file content.
	resp, err := s.gw.ReadFile(r.Context(), &pb.ReadFileRequest{
		SessionId: sandboxID,
		Path:      path,
		Encoding:  "base64",
	})
	if err != nil {
		s.writeEnvdError(w, connectError("not_found", "file not found: "+path))
		return
	}
	content, derr := decodeReadContent(resp)
	if derr != nil {
		s.writeEnvdError(w, connectError("internal", "decode file content: "+derr.Error()))
		return
	}
	name := baseName(path)
	// Single-range support (video playback / resume).
	rangeHeader := r.Header.Get("Range")
	status := http.StatusOK
	body := content
	if rangeHeader != "" {
		if offset, length, ok := parseSingleRange(rangeHeader, len(content)); ok {
			status = http.StatusPartialContent
			body = content[offset : offset+length]
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", offset, offset+length-1, len(content)))
		}
	}
	w.Header().Set("Content-Type", contentTypeOf(name))
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Expose-Headers", "accept-ranges, content-range, content-disposition")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// decodeReadContent turns a ReadFileResponse payload into the raw file bytes.
// The response echoes the encoding it used: "base64" carries the bytes
// base64-encoded, anything else is the legacy escaped-text form, which is
// already plain text.
func decodeReadContent(resp *pb.ReadFileResponse) ([]byte, error) {
	if resp.Encoding == "base64" {
		decoded, err := base64.StdEncoding.DecodeString(resp.Content)
		if err != nil {
			return nil, fmt.Errorf("invalid base64 payload: %w", err)
		}
		return decoded, nil
	}
	return []byte(resp.Content), nil
}

// decodeBodyIfGzip applies the SDK's gzip Content-Encoding to an upload body,
// returning the decoded bytes; on any decode failure the original bytes are
// kept (the sandbox then reports the true error).
func decodeBodyIfGzip(data []byte, encoding string) []byte {
	if encoding != "gzip" {
		return data
	}
	dec, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return data
	}
	defer dec.Close()
	if decoded, derr := io.ReadAll(dec); derr == nil {
		return decoded
	}
	return data
}

// maxUploadBytes bounds a single upload body. The E2B SDK streams uploads
// and caps nothing but the sandbox's own disk, so this is deliberately
// generous (512 MiB): it exists only to stop an unbounded read from
// exhausting the daemon's memory, not to police file size. Over the limit
// the request fails 413 and the SDK surfaces a machine-readable error.
const maxUploadBytes = 512 << 20

// serveFileUpload stores a file: POST /files with multipart parts (filename
// carries the destination path) or octet-stream with ?path=.
func (s *Server) serveFileUpload(w http.ResponseWriter, r *http.Request, sandboxID string) {
	// Auto-resume a paused sandbox (CubeSandbox's auto_resume on file I/O).
	if _, _, e2e := s.wakeForTraffic(r, sandboxID); e2e != nil {
		s.writeEnvdError(w, e2e)
		return
	}
	// Bound the read (memory guard, not a file-size policy — see the
	// constant). Applies to the raw body; gzip decoding and base64 encoding
	// below stay within a small multiple of it.
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)

	var written []map[string]any
	var handleErr *E2bError

	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		written, handleErr = s.readMultipartFiles(r, sandboxID)
	} else {
		path := r.URL.Query().Get("path")
		if path == "" {
			handleErr = connectError("invalid_argument", "missing path query")
		} else {
			data, err := io.ReadAll(r.Body)
			if err != nil {
				// MaxBytesReader's over-limit read arrives here.
				s.writeUploadReadError(w, err)
				return
			}
			// The SDK's gzip option is a Content-Encoding on the whole body; the
			// sandbox must receive the decoded bytes.
			data = decodeBodyIfGzip(data, r.Header.Get("Content-Encoding"))
			entry, e2e := s.writeFilePart(r, sandboxID, path, data)
			if e2e != nil {
				handleErr = e2e
			} else {
				written = append(written, entry)
			}
		}
	}

	if handleErr != nil {
		s.writeEnvdError(w, handleErr)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(written)
}

// readMultipartFiles reads a multipart upload into a list of written entries.
func (s *Server) readMultipartFiles(r *http.Request, sandboxID string) ([]map[string]any, *E2bError) {
	reader, err := r.MultipartReader()
	if err != nil {
		return nil, connectError("invalid_argument", "invalid multipart body")
	}
	var written []map[string]any
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, connectError("invalid_argument", "invalid multipart body")
		}
		// Go's multipart reader strips the filename to its basename; the
		// destination path IS the filename here ('notes/x.txt' must keep
		// its directory), so read it from the raw Content-Disposition.
		filename := multipartFilename(part)
		if filename == "" {
			continue
		}
		data, err := io.ReadAll(io.LimitReader(part, maxUploadBytes+1))
		if err != nil {
			return nil, connectError("internal", "read part failed")
		}
		if len(data) > maxUploadBytes {
			return nil, connectError("resource_exhausted",
				fmt.Sprintf("upload part exceeds %d bytes", maxUploadBytes))
		}
		entry, e2e := s.writeFilePart(r, sandboxID, filename, data)
		if e2e != nil {
			return nil, e2e
		}
		written = append(written, entry)
	}
	return written, nil
}

// writeFilePart resolves, encodes and writes a single uploaded file, returning
// the entry to report back to the client.
func (s *Server) writeFilePart(r *http.Request, sandboxID, path string, data []byte) (map[string]any, *E2bError) {
	resolved, err := resolveSandboxPath(path)
	if err != nil {
		return nil, connectError("invalid_argument", err.Error())
	}
	// The gateway's WriteFileRequest.Content is a proto string, so raw
	// non-UTF-8 bytes make gRPC marshal fail ("string field contains
	// invalid UTF-8"). sandboxd accepts encoding="base64" (its own
	// chunked-transfer format), so binary payloads ride base64; text
	// payloads keep the legacy direct path.
	content := string(data)
	encoding := ""
	if !utf8.Valid(data) {
		content = base64.StdEncoding.EncodeToString(data)
		encoding = "base64"
	}
	_, werr := s.gw.WriteFile(r.Context(), &pb.WriteFileRequest{
		SessionId: sandboxID, Path: resolved, Content: content, Mode: "w",
		Encoding: encoding,
	})
	if werr != nil {
		return nil, connectError("internal", "write failed: "+werr.Error())
	}
	return map[string]any{
		"name": baseName(resolved), "type": "file", "path": resolved,
	}, nil
}

// writeUploadReadError turns a body-read failure into the envd error shape,
// distinguishing an over-limit read (413-equivalent) from a transport fault.
func (s *Server) writeUploadReadError(w http.ResponseWriter, err error) {
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		s.writeEnvdError(w, connectError("resource_exhausted",
			fmt.Sprintf("upload body exceeds %d bytes; use chunked uploads (write at an offset) for larger files", maxErr.Limit)))
		return
	}
	s.writeEnvdError(w, connectError("internal", "read body failed: "+err.Error()))
}

// parseSingleRange parses a single "bytes=A-B" range against a known size.
// Returns (offset, length, ok); a spec that names only bytes past EOF is the
// one hard refusal (416) because serving the full file would loop a naive
// resumer forever.
func parseSingleRange(header string, size int) (int, int, bool) {
	if size == 0 {
		return 0, 0, false
	}
	var start, end int
	n, err := fmt.Sscanf(header, "bytes=%d-%d", &start, &end)
	if err != nil || n < 1 {
		return 0, 0, false
	}
	if start < 0 || start >= size {
		return 0, 0, false
	}
	if n == 1 || end >= size {
		end = size - 1
	}
	if end < start {
		return 0, 0, false
	}
	return start, end - start + 1, true
}

// multipartFilename reads the destination path from a part's raw
// Content-Disposition header (Go's Part.FileName strips directories).
func multipartFilename(part *multipart.Part) string {
	_, params, err := mime.ParseMediaType(part.Header.Get("Content-Disposition"))
	if err != nil {
		return ""
	}
	return params["filename"]
}

// sendPreflight is the browser preflight answer (204 + methods + 2h cache),
// real E2B's shape.
func sendPreflight(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", r.Header.Get("Access-Control-Request-Headers"))
	w.Header().Set("Access-Control-Max-Age", "7200")
	w.WriteHeader(http.StatusNoContent)
}
