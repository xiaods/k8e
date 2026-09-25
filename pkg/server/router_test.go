package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestServeStatic(t *testing.T) {
	parent := t.TempDir()
	staticDir := filepath.Join(parent, "static")
	if err := os.MkdirAll(filepath.Join(staticDir, "nested"), 0700); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"secret":                 "outside secret",
		"static/asset.txt":       "asset contents",
		"static/asset..txt":      "dotted filename",
		"static/nested/file.txt": "nested contents",
	} {
		if err := os.WriteFile(filepath.Join(parent, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for name, target := range map[string]string{
		"escape":     "../secret",
		"escape-dir": parent,
		"internal":   "asset.txt",
	} {
		if err := os.Symlink(target, filepath.Join(staticDir, name)); err != nil {
			t.Fatal(err)
		}
	}
	handler := serveStatic("/static/", staticDir)
	for _, tc := range []struct {
		path   string
		status int
		body   string
	}{
		{"asset.txt", http.StatusOK, "asset contents"},
		{"asset..txt", http.StatusOK, "dotted filename"},
		{"nested/file.txt", http.StatusOK, "nested contents"},
		{"internal", http.StatusOK, "asset contents"},
		{"", http.StatusNotFound, ""},
		{"nested/", http.StatusNotFound, ""},
		{"missing", http.StatusNotFound, ""},
		{"../secret", http.StatusNotFound, ""},
		{"nested/../asset.txt", http.StatusNotFound, ""},
		{"%2e%2e%2fsecret", http.StatusNotFound, ""},
		{"escape", http.StatusNotFound, ""},
		{"escape-dir/secret", http.StatusNotFound, ""},
	} {
		t.Run(tc.path, func(t *testing.T) {
			resp := httptest.NewRecorder()
			handler.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/static/"+tc.path, http.NoBody))
			if resp.Code != tc.status {
				t.Fatalf("status = %d, want %d", resp.Code, tc.status)
			}
			if tc.status == http.StatusOK && resp.Body.String() != tc.body {
				t.Fatalf("body = %q, want %q", resp.Body.String(), tc.body)
			}
		})
	}
	t.Run("range", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/static/asset.txt", http.NoBody)
		req.Header.Set("Range", "bytes=0-4")
		resp := httptest.NewRecorder()
		handler.ServeHTTP(resp, req)
		if resp.Code != http.StatusPartialContent || resp.Body.String() != "asset" {
			t.Fatalf("range response: %d %q", resp.Code, resp.Body.String())
		}
	})
}
