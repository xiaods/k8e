package e2b

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"

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
	resp, err := s.gw.ReadFile(r.Context(), &pb.ReadFileRequest{SessionId: sandboxID, Path: path})
	if err != nil {
		s.writeEnvdError(w, connectError("not_found", "file not found: "+path))
		return
	}
	content := []byte(resp.Content)
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

// serveFileUpload stores a file: POST /files with multipart parts (filename
// carries the destination path) or octet-stream with ?path=.
func (s *Server) serveFileUpload(w http.ResponseWriter, r *http.Request, sandboxID string) {
	// Auto-resume a paused sandbox (CubeSandbox's auto_resume on file I/O).
	if _, _, e2e := s.wakeForTraffic(r, sandboxID); e2e != nil {
		s.writeEnvdError(w, e2e)
		return
	}
	var written []map[string]any
	writeOne := func(path, content string) {
		resolved, err := resolveSandboxPath(path)
		if err != nil {
			s.writeEnvdError(w, connectError("invalid_argument", err.Error()))
			return
		}
		_, werr := s.gw.WriteFile(r.Context(), &pb.WriteFileRequest{
			SessionId: sandboxID, Path: resolved, Content: content, Mode: "w",
		})
		if werr != nil {
			s.writeEnvdError(w, connectError("internal", "write failed: "+werr.Error()))
			return
		}
		written = append(written, map[string]any{
			"name": baseName(resolved), "type": "file", "path": resolved,
		})
	}

	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		reader, err := r.MultipartReader()
		if err != nil {
			s.writeEnvdError(w, connectError("invalid_argument", "invalid multipart body"))
			return
		}
		for {
			part, err := reader.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				s.writeEnvdError(w, connectError("invalid_argument", "invalid multipart body"))
				return
			}
			// Go's multipart reader strips the filename to its basename; the
			// destination path IS the filename here ('notes/x.txt' must keep
			// its directory), so read it from the raw Content-Disposition.
			filename := multipartFilename(part)
			if filename == "" {
				continue
			}
			data, err := io.ReadAll(part)
			if err != nil {
				s.writeEnvdError(w, connectError("internal", "read part failed"))
				return
			}
			writeOne(filename, string(data))
		}
	} else {
		path := r.URL.Query().Get("path")
		if path == "" {
			s.writeEnvdError(w, connectError("invalid_argument", "missing path query"))
			return
		}
		data, err := io.ReadAll(r.Body)
		if err != nil {
			s.writeEnvdError(w, connectError("internal", "read body failed"))
			return
		}
		// The SDK's gzip option is a Content-Encoding on the whole body; the
		// sandbox must receive the decoded bytes.
		if r.Header.Get("Content-Encoding") == "gzip" {
			if dec, err := gzip.NewReader(strings.NewReader(string(data))); err == nil {
				if decoded, derr := io.ReadAll(dec); derr == nil {
					data = decoded
				}
			}
		}
		writeOne(path, string(data))
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(written)
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
