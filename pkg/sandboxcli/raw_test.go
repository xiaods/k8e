package sandboxcli

import (
	"bytes"
	"encoding/base64"
	"testing"

	"github.com/xiaods/k8e/pkg/sandbox/envdstream"
)

func frame(channel, payload string) string {
	return "data: {\"" + channel + "\":\"" + base64.StdEncoding.EncodeToString([]byte(payload)) + "\"}\n\n"
}

// TestRawWriter_StdoutAndStderrAreSeparated verifies the decoded payloads
// land on the right stream.
func TestRawWriter_StdoutAndStderrAreSeparated(t *testing.T) {
	var out, errw bytes.Buffer
	w := &rawWriter{out: &out, errw: &errw}

	frames := envdstream.DecodeAll([]byte(frame("stdout", "OUT") + frame("stderr", "ERR")))
	for _, f := range frames {
		w.write(f)
	}

	if out.String() != "OUT" {
		t.Fatalf("stdout = %q, want %q", out.String(), "OUT")
	}
	if errw.String() != "ERR" {
		t.Fatalf("stderr = %q, want %q", errw.String(), "ERR")
	}
}

// TestRawWriter_ControlCharactersSurvive verifies an ANSI-coloured payload
// reaches the terminal intact — the reason the frames are base64 in the
// first place.
func TestRawWriter_ControlCharactersSurvive(t *testing.T) {
	var out, errw bytes.Buffer
	w := &rawWriter{out: &out, errw: &errw}

	raw := "\x1b[1;36m★\x1b[0m\r\n"
	for _, f := range envdstream.DecodeAll([]byte(frame("stdout", raw))) {
		w.write(f)
	}
	if out.String() != raw {
		t.Fatalf("stdout = %q, want %q", out.String(), raw)
	}
}

// TestRawWriter_ExitCodeIsCaptured verifies the terminal frame's exit code
// is reported. Without it a failing command would look like a success.
func TestRawWriter_ExitCodeIsCaptured(t *testing.T) {
	var out, errw bytes.Buffer
	w := &rawWriter{out: &out, errw: &errw}

	for _, f := range envdstream.DecodeAll([]byte(frame("stdout", "x") + "data: {\"exit\":3}\n\n")) {
		w.write(f)
	}
	if w.exit == nil {
		t.Fatal("expected the exit code to be captured")
	}
	if *w.exit != 3 {
		t.Fatalf("exit = %d, want 3", *w.exit)
	}
}

// TestRawWriter_PidFrameIsNotPrinted verifies the start frame does not
// corrupt the command's output.
func TestRawWriter_PidFrameIsNotPrinted(t *testing.T) {
	var out, errw bytes.Buffer
	w := &rawWriter{out: &out, errw: &errw}

	for _, f := range envdstream.DecodeAll([]byte("data: {\"pid\":42}\n\n" + frame("stdout", "OUT"))) {
		w.write(f)
	}
	if out.String() != "OUT" {
		t.Fatalf("stdout = %q, want %q (the pid frame must not be printed)", out.String(), "OUT")
	}
}

// TestRawWriter_UnknownFrameIsPassedThrough verifies output from a sandboxd
// that writes plain text is not swallowed.
func TestRawWriter_UnknownFrameIsPassedThrough(t *testing.T) {
	var out, errw bytes.Buffer
	w := &rawWriter{out: &out, errw: &errw}

	for _, f := range envdstream.DecodeAll([]byte("data: plain text\n\n")) {
		w.write(f)
	}
	if out.String() != "plain text" {
		t.Fatalf("stdout = %q, want %q", out.String(), "plain text")
	}
}
