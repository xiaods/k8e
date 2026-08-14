package e2b

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"testing"
)

func TestFilesWriteRead(t *testing.T) {
	gw := newFakeGateway()
	s, ts := testServer(t, gw)
	id := createSandboxID(t, ts)
	headers := envdHeaders(t, s, id)

	// Upload via multipart (the SDK's default shape: part filename = path).
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fw, err := w.CreateFormFile("file", "notes/hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fw.Write([]byte("你好,Dormice ✓\n"))
	_ = w.Close()

	req, _ := http.NewRequest("POST", ts.URL+"/e2b/envd/files", &buf)
	req.Header.Set("Content-Type", w.FormDataContentType())
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("upload: %d %s", resp.StatusCode, readBody(t, resp))
	}
	var written []map[string]any
	_ = json.Unmarshal([]byte(readBody(t, resp)), &written)
	if len(written) != 1 {
		t.Fatalf("written=%v", written)
	}
	if written[0]["path"] != "/workspace/notes/hello.txt" {
		t.Fatalf("path=%v", written[0]["path"])
	}

	// Download.
	dl, err := http.NewRequest("GET", ts.URL+"/e2b/envd/files?path=notes/hello.txt", nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range headers {
		dl.Header.Set(k, v)
	}
	dresp, err := ts.Client().Do(dl)
	if err != nil {
		t.Fatal(err)
	}
	if dresp.StatusCode != 200 {
		t.Fatalf("download: %d", dresp.StatusCode)
	}
	if body := readBody(t, dresp); body != "你好,Dormice ✓\n" {
		t.Fatalf("content=%q", body)
	}
}

func TestFilesOctetStreamUpload(t *testing.T) {
	gw := newFakeGateway()
	s, ts := testServer(t, gw)
	id := createSandboxID(t, ts)
	headers := envdHeaders(t, s, id)

	req, _ := http.NewRequest("POST", ts.URL+"/e2b/envd/files?path=blob.bin", bytes.NewReader([]byte{0, 1, 2, 255}))
	req.Header.Set("Content-Type", "application/octet-stream")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("octet upload: %d %s", resp.StatusCode, readBody(t, resp))
	}
	resp.Body.Close()

	gw.mu.Lock()
	content := gw.files["/workspace/blob.bin"]
	gw.mu.Unlock()
	if content != string([]byte{0, 1, 2, 255}) {
		t.Fatalf("content=%q", content)
	}
}

func TestFilesRangeDownload(t *testing.T) {
	gw := newFakeGateway()
	s, ts := testServer(t, gw)
	id := createSandboxID(t, ts)
	headers := envdHeaders(t, s, id)

	gw.mu.Lock()
	gw.files["/workspace/big.txt"] = "0123456789"
	gw.mu.Unlock()

	req, _ := http.NewRequest("GET", ts.URL+"/e2b/envd/files?path=big.txt", nil)
	req.Header.Set("Range", "bytes=2-5")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 206 {
		t.Fatalf("want 206, got %d", resp.StatusCode)
	}
	if body := readBody(t, resp); body != "2345" {
		t.Fatalf("range body=%q", body)
	}
	if resp.Header.Get("Content-Range") != "bytes 2-5/10" {
		t.Fatalf("content-range=%q", resp.Header.Get("Content-Range"))
	}
}

func TestFilesMissingAuth(t *testing.T) {
	gw := newFakeGateway()
	_, ts := testServer(t, gw)
	id := createSandboxID(t, ts)

	req, _ := http.NewRequest("GET", ts.URL+"/e2b/envd/files?path=x", nil)
	req.Header.Set("E2b-Sandbox-Id", id) // no token
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 401 {
		t.Fatalf("want 401, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestSignedFilesDownload(t *testing.T) {
	gw := newFakeGateway()
	s, ts := testServer(t, gw)
	id := createSandboxID(t, ts)
	token := mintEnvdToken(s.signingSecret, id)

	gw.mu.Lock()
	gw.files["/workspace/signed.txt"] = "signed content\n"
	gw.mu.Unlock()

	// The SDK's downloadUrl signature: path + read + empty user + token.
	sig := fileSignature(token, signatureMaterial{path: "signed.txt", operation: "read"})
	url := ts.URL + "/files?path=signed.txt&signature=" + url.QueryEscape(sig)
	resp, err := ts.Client().Get(url)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("signed download: %d %s", resp.StatusCode, readBody(t, resp))
	}
	if body := readBody(t, resp); body != "signed content\n" {
		t.Fatalf("content=%q", body)
	}
}

func TestSignedFilesForgedSignature(t *testing.T) {
	gw := newFakeGateway()
	s, ts := testServer(t, gw)
	id := createSandboxID(t, ts)
	_ = mintEnvdToken(s.signingSecret, id)

	gw.mu.Lock()
	gw.files["/workspace/signed.txt"] = "x"
	gw.mu.Unlock()

	url := ts.URL + "/files?path=signed.txt&signature=v1_forged"
	resp, err := ts.Client().Get(url)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 401 {
		t.Fatalf("forged signature: want 401, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestSignedFilesExpired(t *testing.T) {
	gw := newFakeGateway()
	s, ts := testServer(t, gw)
	id := createSandboxID(t, ts)
	token := mintEnvdToken(s.signingSecret, id)

	gw.mu.Lock()
	gw.files["/workspace/signed.txt"] = "x"
	gw.mu.Unlock()

	past := unixNow() - 10
	sig := fileSignature(token, signatureMaterial{path: "signed.txt", operation: "read", expirationUnix: &past})
	url := ts.URL + "/files?path=signed.txt&signature=" + url.QueryEscape(sig) + "&signature_expiration=" + strconv.FormatInt(past, 10)
	resp, err := ts.Client().Get(url)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 401 {
		t.Fatalf("expired: want 401, got %d", resp.StatusCode)
	}
	body := readBody(t, resp)
	if !containsJSON(body, "signature is already expired") {
		t.Fatalf("body=%s", body)
	}
	resp.Body.Close()
}

func TestFSServiceNative(t *testing.T) {
	gw := newFakeGateway()
	stub := newSandboxdStub()
	s, ts := testServerWithSandboxd(t, gw, stub)
	id := createSandboxID(t, ts)
	headers := envdHeaders(t, s, id)

	stub.mu.Lock()
	stub.files["/workspace/a.txt"] = "hello"
	stub.dirs["/workspace/notes"] = true
	stub.mu.Unlock()

	envdFS := func(method, payload string) (int, map[string]any) {
		req, _ := http.NewRequest("POST", ts.URL+"/e2b/envd/filesystem.Filesystem/"+method, bytes.NewReader([]byte(payload)))
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body := map[string]any{}
		_ = json.Unmarshal([]byte(readBody(t, resp)), &body)
		return resp.StatusCode, body
	}

	// Stat a file.
	status, body := envdFS("Stat", `{"path":"a.txt"}`)
	if status != 200 {
		t.Fatalf("stat: %d", status)
	}
	entry := body["entry"].(map[string]any)
	if entry["type"] != "FILE_TYPE_FILE" {
		t.Fatalf("type=%v", entry["type"])
	}
	if entry["permissions"] != "rw-r--r--" {
		t.Fatalf("permissions=%v", entry["permissions"])
	}

	// MakeDir, then already_exists on the second attempt.
	status, _ = envdFS("MakeDir", `{"path":"notes/deep"}`)
	if status != 200 {
		t.Fatalf("mkdir: %d", status)
	}
	status, body = envdFS("MakeDir", `{"path":"notes/deep"}`)
	if status != 409 {
		t.Fatalf("mkdir existing should be 409, got %d: %v", status, body)
	}

	// Move.
	status, body = envdFS("Move", `{"source":"a.txt","destination":"b.txt"}`)
	if status != 200 {
		t.Fatalf("move: %d %v", status, body)
	}
	stub.mu.Lock()
	_, hasOld := stub.files["/workspace/a.txt"]
	_, hasNew := stub.files["/workspace/b.txt"]
	stub.mu.Unlock()
	if hasOld || !hasNew {
		t.Fatalf("move failed: old=%v new=%v", hasOld, hasNew)
	}

	// Remove.
	status, _ = envdFS("Remove", `{"path":"notes/deep"}`)
	if status != 200 {
		t.Fatalf("remove: %d", status)
	}
	stub.mu.Lock()
	_, still := stub.dirs["/workspace/notes/deep"]
	stub.mu.Unlock()
	if still {
		t.Fatal("remove should delete the dir")
	}
}

func TestFSPathTraversalRejected(t *testing.T) {
	gw := newFakeGateway()
	s, ts := testServer(t, gw)
	id := createSandboxID(t, ts)
	headers := envdHeaders(t, s, id)

	req, _ := http.NewRequest("POST", ts.URL+"/e2b/envd/filesystem.Filesystem/Remove", bytes.NewReader([]byte(`{"path":"../../etc/passwd"}`)))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 400 {
		t.Fatalf("traversal: want 400, got %d", resp.StatusCode)
	}
	body := readBody(t, resp)
	if !containsJSON(body, "traversal") {
		t.Fatalf("body=%s", body)
	}
	resp.Body.Close()
}

func containsJSON(body, needle string) bool {
	return jsonContains(body, needle)
}

func jsonContains(body, needle string) bool {
	return bytes.Contains([]byte(body), []byte(needle))
}

// TestFilesystemListDirDepth verifies the depth parameter: depth 1 returns
// direct children only, depth 2 also includes grandchildren. The SDK exposes
// list(path, depth=N); k8e now honors arbitrary depth (P1 audit item).
func TestFilesystemListDirDepth(t *testing.T) {
	gw := newFakeGateway()
	stub := newSandboxdStub()
	s, ts := testServerWithSandboxd(t, gw, stub)
	id := createSandboxID(t, ts)

	// Seed a nested tree: /workspace/a.txt, /workspace/sub/b.txt.
	gw.mu.Lock()
	gw.files["/workspace/a.txt"] = "a"
	gw.files["/workspace/sub/b.txt"] = "b"
	gw.mu.Unlock()
	stub.mu.Lock()
	stub.dirs["/workspace/"] = true
	stub.dirs["/workspace/sub"] = true
	stub.files["/workspace/a.txt"] = "a"
	stub.files["/workspace/sub/b.txt"] = "b"
	stub.mu.Unlock()

	listDepth := func(depth int) []string {
		req, _ := http.NewRequest("POST", ts.URL+"/e2b/envd/filesystem.Filesystem/ListDir",
			bytes.NewReader([]byte(fmt.Sprintf(`{"path":"/workspace","depth":%d}`, depth))))
		for k, v := range envdHeaders(t, s, id) {
			req.Header.Set(k, v)
		}
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body := readBody(t, resp)
		var out struct {
			Entries []map[string]any `json:"entries"`
		}
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatalf("list body: %s", body)
		}
		names := []string{}
		for _, e := range out.Entries {
			names = append(names, e["name"].(string))
		}
		return names
	}

	d1 := listDepth(1)
	if len(d1) != 1 || d1[0] != "a.txt" {
		t.Fatalf("depth 1 = %v, want [a.txt] (direct child file)", d1)
	}
	d2 := listDepth(2)
	// Depth 2 includes the grandchild b.txt alongside a.txt (k8e's ListDir
	// lists files from the gateway's recursive walk; directory entries are
	// not synthesized).
	found := map[string]bool{}
	for _, n := range d2 {
		found[n] = true
	}
	if !found["a.txt"] || !found["b.txt"] {
		t.Fatalf("depth 2 = %v, want both a.txt and b.txt", d2)
	}
}
