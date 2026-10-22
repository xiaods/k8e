package grpc

import (
	"strings"
	"testing"
)

// TestDecodeSandboxdResponse_ValidBody verifies a well-formed sandboxd body
// decodes into the caller's struct.
func TestDecodeSandboxdResponse_ValidBody(t *testing.T) {
	var out struct {
		Status string `json:"status"`
		Stdout string `json:"stdout"`
	}
	body := `{"status":"completed","stdout":"hello\n"}`
	if err := decodeSandboxdResponse(strings.NewReader(body), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Status != "completed" || out.Stdout != "hello\n" {
		t.Fatalf("decoded = %+v", out)
	}
}

// TestDecodeSandboxdResponse_RawControlCharIsAnError verifies that a body
// carrying a raw ESC (0x1B) inside a JSON string is reported as an error
// instead of silently leaving the destination at its zero value.
//
// This is the exact shape sandboxd produced for a command whose output
// contained ANSI colour codes: the bytes were there, but the document was
// invalid JSON, so the strict parser rejected it. Swallowing that error
// made the run look like it had produced no output at all.
func TestDecodeSandboxdResponse_RawControlCharIsAnError(t *testing.T) {
	var out struct {
		Status string `json:"status"`
		Stdout string `json:"stdout"`
	}
	// ESC as a raw byte inside the string, not the two-character escape.
	body := "{\"status\":\"completed\",\"stdout\":\"\x1b[31mred\x1b[0m\"}"
	err := decodeSandboxdResponse(strings.NewReader(body), &out)
	if err == nil {
		t.Fatalf("expected a decode error for a raw control character, got %+v", out)
	}
	if !strings.Contains(err.Error(), "decode sandboxd response") {
		t.Fatalf("error should name the decode step, got %v", err)
	}
}

// TestDecodeSandboxdResponse_TruncatedBodyIsAnError verifies a truncated
// body is an error too, not a partially filled struct.
func TestDecodeSandboxdResponse_TruncatedBodyIsAnError(t *testing.T) {
	var out struct {
		Stdout string `json:"stdout"`
	}
	if err := decodeSandboxdResponse(strings.NewReader(`{"stdout":"trunc`), &out); err == nil {
		t.Fatal("expected a decode error for a truncated body")
	}
}
