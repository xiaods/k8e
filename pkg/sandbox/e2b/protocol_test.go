package e2b

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"testing"
)

func TestEnvelopeRoundTrip(t *testing.T) {
	payload := map[string]any{"event": map[string]any{"start": map[string]int{"pid": 42}}}
	raw := envelope(FlagMessage, payload)
	if raw[0] != FlagMessage {
		t.Fatalf("flag byte = %d", raw[0])
	}
	length := binary.BigEndian.Uint32(raw[1:5])
	if int(length) != len(raw)-5 {
		t.Fatalf("length %d != body %d", length, len(raw)-5)
	}
	frames, err := parseEnvelopes(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 1 {
		t.Fatalf("frames=%d", len(frames))
	}
	if frames[0].flags != FlagMessage {
		t.Fatalf("flags=%d", frames[0].flags)
	}
	ev := frames[0].json["event"].(map[string]any)
	start := ev["start"].(map[string]any)
	if start["pid"] != float64(42) {
		t.Fatalf("pid=%v", start["pid"])
	}
}

func TestEnvelopeEndStream(t *testing.T) {
	raw := envelope(FlagEndStream, map[string]any{})
	frames, err := parseEnvelopes(raw)
	if err != nil {
		t.Fatal(err)
	}
	if frames[0].flags != FlagEndStream {
		t.Fatalf("flags=%d", frames[0].flags)
	}
	if len(frames[0].json) != 0 {
		t.Fatalf("end frame should be empty object: %v", frames[0].json)
	}
}

func TestReadFirstMessage(t *testing.T) {
	msg, err := readFirstMessage(envelope(FlagMessage, map[string]any{"process": map[string]any{"cmd": "/bin/bash"}}))
	if err != nil {
		t.Fatal(err)
	}
	proc := msg["process"].(map[string]any)
	if proc["cmd"] != "/bin/bash" {
		t.Fatalf("cmd=%v", proc["cmd"])
	}

	// Truncated body → invalid_argument.
	if _, err := readFirstMessage([]byte{0, 0, 0, 0}); err == nil {
		t.Fatal("expected error on truncated envelope")
	}
}

func TestEnvdTokenMintVerify(t *testing.T) {
	token := mintEnvdToken("secret", "sandbox-1")
	if token == "" {
		t.Fatal("empty token")
	}
	if !verifyEnvdToken("secret", "sandbox-1", token) {
		t.Fatal("token should verify")
	}
	if verifyEnvdToken("secret", "sandbox-2", token) {
		t.Fatal("token for sandbox-1 must not verify for sandbox-2")
	}
	if verifyEnvdToken("other", "sandbox-1", token) {
		t.Fatal("token must not verify with a different secret")
	}
}

func TestFileSignatureFormat(t *testing.T) {
	token := mintEnvdToken("secret", "s")
	sig := fileSignature(token, signatureMaterial{path: "a.txt", operation: "read"})
	if len(sig) < 5 || sig[:3] != "v1_" {
		t.Fatalf("signature prefix wrong: %q", sig)
	}
	// With expiration.
	exp := int64(12345)
	sig2 := fileSignature(token, signatureMaterial{path: "a.txt", operation: "read", expirationUnix: &exp})
	if sig2 == sig {
		t.Fatal("expiration should change the signature")
	}
	// Different operation changes it.
	sig3 := fileSignature(token, signatureMaterial{path: "a.txt", operation: "write"})
	if sig3 == sig {
		t.Fatal("operation should change the signature")
	}
}

func TestSignatureMatchAndExpiration(t *testing.T) {
	token := mintEnvdToken("secret", "s")
	exp := unixNow() + 3600
	m := signatureMaterial{path: "a.txt", operation: "read", expirationUnix: &exp}
	sig := fileSignature(token, m)
	if !signatureMatches(sig, "s", "secret", m) {
		t.Fatal("valid signature should match")
	}
	if signatureMatches("v1_forged", "s", "secret", m) {
		t.Fatal("forged signature must not match")
	}
	if e := checkSignatureExpiration(m); e != nil {
		t.Fatalf("future expiration should pass, got %v", e)
	}
	past := unixNow() - 10
	mp := signatureMaterial{path: "a.txt", operation: "read", expirationUnix: &past}
	if e := checkSignatureExpiration(mp); e == nil || e.Code != "unauthenticated" {
		t.Fatalf("past expiration should refuse: %v", e)
	}
}

func TestSSEFramer(t *testing.T) {
	f := &sseFramer{}
	out := f.push("data: hello\n\n")
	if string(out) != "hello" {
		t.Fatalf("out=%q", out)
	}
	// A frame straddling two chunks.
	f2 := &sseFramer{}
	_ = f2.push("data: wor")
	got := f2.push("ld\n\n")
	if string(got) != "world" {
		t.Fatalf("straddled=%q", got)
	}
}

func TestShellQuote(t *testing.T) {
	if shellQuote("a.txt") != "'a.txt'" {
		t.Fatalf("got %s", shellQuote("a.txt"))
	}
	if shellQuote("it's") != `'it'\''s'` {
		t.Fatalf("got %s", shellQuote("it's"))
	}
}

func TestResolveSandboxPath(t *testing.T) {
	p, err := resolveSandboxPath("notes/x.txt")
	if err != nil || p != "/workspace/notes/x.txt" {
		t.Fatalf("p=%q err=%v", p, err)
	}
	p, err = resolveSandboxPath("/workspace/notes/x.txt")
	if err != nil || p != "/workspace/notes/x.txt" {
		t.Fatalf("p=%q err=%v", p, err)
	}
	if _, err := resolveSandboxPath("../../etc/passwd"); err == nil {
		t.Fatal("traversal must be rejected")
	}
	if _, err := resolveSandboxPath("/etc/passwd"); err != nil {
		t.Fatal("absolute under / should be rejected or clamped")
	}
}

func TestEnvelopeJSONCodec(t *testing.T) {
	// The envelope payload must round-trip through json.Marshal (the SDK
	// uses the JSON codec, no protobuf wire format).
	msg := map[string]any{"process": map[string]any{"cmd": "/bin/bash", "args": []string{"-c", "echo hi"}}}
	raw := envelope(FlagMessage, msg)
	var check map[string]any
	if err := json.Unmarshal(raw[5:], &check); err != nil {
		t.Fatalf("payload not JSON: %v", err)
	}
	if !bytes.Equal(raw[5:], mustJSON(t, msg)) {
		t.Fatal("envelope payload must be the JSON encoding")
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
