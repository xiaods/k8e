package envdstream

import (
	"bytes"
	"encoding/base64"
	"testing"
)

func encode(s string) string {
	return base64.StdEncoding.EncodeToString([]byte(s))
}

func TestParseFrame_Stdout(t *testing.T) {
	f, ok := ParseFrame([]byte(`data: {"stdout":"` + encode("hello") + `"}`))
	if !ok {
		t.Fatal("expected the frame to parse")
	}
	if f.Channel != ChannelStdout {
		t.Fatalf("channel = %q, want stdout", f.Channel)
	}
	if string(f.Payload) != "hello" {
		t.Fatalf("payload = %q, want %q", f.Payload, "hello")
	}
}

func TestParseFrame_Stderr(t *testing.T) {
	f, ok := ParseFrame([]byte(`{"stderr":"` + encode("boom") + `"}`))
	if !ok {
		t.Fatal("expected the frame to parse")
	}
	if f.Channel != ChannelStderr {
		t.Fatalf("channel = %q, want stderr", f.Channel)
	}
	if string(f.Payload) != "boom" {
		t.Fatalf("payload = %q, want %q", f.Payload, "boom")
	}
}

func TestParseFrame_Exit(t *testing.T) {
	f, ok := ParseFrame([]byte(`{"exit":3}`))
	if !ok {
		t.Fatal("expected the frame to parse")
	}
	if f.Exit == nil || *f.Exit != 3 {
		t.Fatalf("exit = %v, want 3", f.Exit)
	}
}

// The server works with the bare payload (it strips the SSE prefix
// itself); the CLI receives the wire form. Both must decode.
func TestParseFrame_AcceptsBothPrefixForms(t *testing.T) {
	bare, ok := ParseFrame([]byte(`{"stdout":"` + encode("x") + `"}`))
	if !ok || string(bare.Payload) != "x" {
		t.Fatalf("bare payload: ok=%v frame=%+v", ok, bare)
	}
	wire, ok := ParseFrame([]byte(`data: {"stdout":"` + encode("x") + `"}`))
	if !ok || string(wire.Payload) != "x" {
		t.Fatalf("wire form: ok=%v frame=%+v", ok, wire)
	}
}

func TestParseFrame_RejectsNonOutputFrames(t *testing.T) {
	for _, frame := range []string{
		"plain output",
		"data: ",
		"data: not json",
		"",
	} {
		if _, ok := ParseFrame([]byte(frame)); ok {
			t.Fatalf("frame %q should not parse as an output frame", frame)
		}
	}
}

// The pid frame is a frame, but not an output frame — the caller must not
// print it as command output.
func TestParseFrame_PidIsNotAnOutputFrame(t *testing.T) {
	f, ok := ParseFrame([]byte(`{"pid":42}`))
	if !ok {
		t.Fatal("expected the pid frame to parse")
	}
	if f.PID == nil || *f.PID != 42 {
		t.Fatalf("pid = %v, want 42", f.PID)
	}
	if f.Channel != "" || f.Payload != nil {
		t.Fatalf("the pid frame must not carry output: %+v", f)
	}
}

// A frame whose payload is not valid base64 is not an output frame — the
// caller falls back to treating it as raw output.
func TestParseFrame_RejectsInvalidBase64(t *testing.T) {
	if _, ok := ParseFrame([]byte(`{"stdout":"!!!not base64!!!"}`)); ok {
		t.Fatal("expected invalid base64 to be rejected")
	}
}

// The gRPC layer forwards the SSE body in fixed 4096-byte chunks, so a
// frame can be split across two chunks.
func TestDecoder_FrameSplitAcrossChunks(t *testing.T) {
	body := `data: {"stdout":"` + encode("split me") + `"}` + "\n\n"

	d := &Decoder{}
	var frames []Frame
	for i := 0; i < len(body); i += 7 {
		end := i + 7
		if end > len(body) {
			end = len(body)
		}
		frames = append(frames, d.Feed([]byte(body[i:end]))...)
	}
	frames = append(frames, d.Flush()...)

	if len(frames) != 1 {
		t.Fatalf("got %d frames, want 1: %+v", len(frames), frames)
	}
	if string(frames[0].Payload) != "split me" {
		t.Fatalf("payload = %q, want %q", frames[0].Payload, "split me")
	}
}

// One chunk can hold several frames.
func TestDecoder_MultipleFramesInOneChunk(t *testing.T) {
	body := `data: {"stdout":"` + encode("one") + `"}` + "\n\n" +
		`data: {"stderr":"` + encode("two") + `"}` + "\n\n" +
		`data: {"exit":0}` + "\n\n"

	frames := DecodeAll([]byte(body))
	if len(frames) != 3 {
		t.Fatalf("got %d frames, want 3: %+v", len(frames), frames)
	}
	if string(frames[0].Payload) != "one" || frames[0].Channel != ChannelStdout {
		t.Fatalf("frame 0 = %+v", frames[0])
	}
	if string(frames[1].Payload) != "two" || frames[1].Channel != ChannelStderr {
		t.Fatalf("frame 1 = %+v", frames[1])
	}
	if frames[2].Exit == nil || *frames[2].Exit != 0 {
		t.Fatalf("frame 2 = %+v", frames[2])
	}
}

// A frame that is not an envd output frame comes back verbatim, so an older
// sandboxd that writes plain text still shows its output.
func TestDecoder_UnknownFrameIsPassedThrough(t *testing.T) {
	frames := DecodeAll([]byte("data: plain text\n\n"))
	if len(frames) != 1 {
		t.Fatalf("got %d frames, want 1", len(frames))
	}
	if !bytes.Contains(frames[0].Raw, []byte("plain text")) {
		t.Fatalf("raw = %q, want it to contain the original text", frames[0].Raw)
	}
}

// A truncated stream still yields what arrived.
func TestDecoder_FlushReturnsTheRemainder(t *testing.T) {
	d := &Decoder{}
	d.Feed([]byte(`data: {"stdout":"` + encode("cut")))
	frames := d.Flush()
	if len(frames) != 1 {
		t.Fatalf("got %d frames, want 1", len(frames))
	}
}

// The whole point of the base64 payload: control characters inside the
// output must survive the round trip without breaking the framing.
func TestDecoder_ControlCharactersSurvive(t *testing.T) {
	raw := "\x1b[1;36m★\x1b[0m\r\n\x00\x01"
	frames := DecodeAll([]byte(`data: {"stdout":"` + encode(raw) + `"}` + "\n\n"))
	if len(frames) != 1 {
		t.Fatalf("got %d frames, want 1", len(frames))
	}
	if string(frames[0].Payload) != raw {
		t.Fatalf("payload = %q, want %q", frames[0].Payload, raw)
	}
}
