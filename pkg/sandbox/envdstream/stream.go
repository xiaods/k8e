// Package envdstream decodes the E2B envd output framing.
//
// The envd data plane streams command output as server-sent events:
//
//	data: {"stdout":"<base64>"}\n\n
//	data: {"stderr":"<base64>"}\n\n
//	data: {"exit":3}\n\n
//
// The payloads are base64 so that newlines, ANSI escapes and other control
// characters inside the output can never break a frame boundary — a
// half-written escape sequence would otherwise be indistinguishable from
// the end of the frame.
//
// A client that wants the bytes the program actually printed has to decode
// them. This package is the single implementation of that decoding, shared
// by the e2b server (which forwards frames) and the CLI (which prints
// them).
package envdstream

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
)

// Channel is the output stream a frame carries.
type Channel string

const (
	// ChannelStdout carries the command's standard output.
	ChannelStdout Channel = "stdout"
	// ChannelStderr carries the command's standard error.
	ChannelStderr Channel = "stderr"
)

// Frame is one decoded output frame.
//
// Exactly one of Channel/Payload, Exit, PID, or Raw is set.
type Frame struct {
	// Channel is set for an output frame.
	Channel Channel
	// Payload holds the decoded bytes of an output frame.
	Payload []byte
	// Exit is set for the terminal frame and carries the command's
	// exit code.
	Exit *int
	// PID is set for the start frame and carries the guest pid of the
	// process the stream is attached to.
	PID *int
	// Raw holds a frame that is not an envd output frame (a comment, or
	// plain output from an older sandboxd). The decoder hands it through
	// unchanged so a client can show it verbatim.
	Raw []byte
}

// ParseFrame decodes a single frame — the bytes between two blank lines.
//
// The frame may carry the SSE "data:" prefix (the wire form, what a client
// receives) or be the bare payload (the form the e2b server works with
// after it strips the prefix). Both are accepted.
//
// It reports ok=false for anything that is not an envd output frame: a
// plain-text frame from an older sandboxd, a comment, or a frame carrying
// something else (a pid, a start event). The caller decides what to do
// with those.
func ParseFrame(frame []byte) (Frame, bool) {
	trimmed := bytes.TrimSpace(frame)
	if bytes.HasPrefix(trimmed, []byte("data:")) {
		trimmed = bytes.TrimSpace(trimmed[len("data:"):])
	}
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return Frame{}, false
	}

	var v struct {
		Stdout string `json:"stdout"`
		Stderr string `json:"stderr"`
		Exit   *int   `json:"exit"`
		PID    *int   `json:"pid"`
	}
	if err := json.Unmarshal(trimmed, &v); err != nil {
		return Frame{}, false
	}

	// Exactly one of stdout/stderr may be present, and it must be
	// non-empty: an empty payload carries no bytes and would be a no-op
	// frame.
	if v.Stdout != "" {
		decoded, err := base64.StdEncoding.DecodeString(v.Stdout)
		if err != nil {
			return Frame{}, false
		}
		return Frame{Channel: ChannelStdout, Payload: decoded}, true
	}
	if v.Stderr != "" {
		decoded, err := base64.StdEncoding.DecodeString(v.Stderr)
		if err != nil {
			return Frame{}, false
		}
		return Frame{Channel: ChannelStderr, Payload: decoded}, true
	}
	if v.Exit != nil {
		code := *v.Exit
		return Frame{Exit: &code}, true
	}
	if v.PID != nil && *v.PID != 0 {
		pid := *v.PID
		return Frame{PID: &pid}, true
	}
	return Frame{}, false
}

// StripDataPrefix removes the SSE "data:" prefix from a frame, leaving the
// payload. A frame that carries no prefix is returned unchanged.
func StripDataPrefix(frame []byte) []byte {
	trimmed := bytes.TrimSpace(frame)
	if !bytes.HasPrefix(trimmed, []byte("data:")) {
		return trimmed
	}
	return bytes.TrimSpace(trimmed[len("data:"):])
}

// Decoder decodes a byte stream of envd SSE frames.
//
// The gRPC layer forwards the SSE body in fixed 4096-byte chunks, so one
// chunk can hold several frames and a frame can be split across two
// chunks. The Decoder buffers the remainder so a frame is only decoded once
// it is complete.
type Decoder struct {
	buf []byte
}

// Feed appends bytes to the decoder and returns the frames that became
// complete. A frame that is not an envd output frame comes back as a Raw
// frame.
func (d *Decoder) Feed(p []byte) []Frame {
	d.buf = append(d.buf, p...)
	var frames []Frame
	for {
		idx := bytes.Index(d.buf, []byte("\n\n"))
		if idx < 0 {
			break
		}
		frame := d.buf[:idx]
		d.buf = d.buf[idx+2:]
		if f, ok := ParseFrame(frame); ok {
			frames = append(frames, f)
		} else {
			// Not an envd output frame. Hand the payload through
			// unchanged (minus the SSE prefix, which is framing
			// rather than content) so an older sandboxd that writes
			// plain text still shows its output.
			frames = append(frames, Frame{Raw: StripDataPrefix(frame)})
		}
	}
	return frames
}

// Flush returns what is left in the buffer once the stream ended — a
// truncated final frame, if the stream was cut short.
func (d *Decoder) Flush() []Frame {
	if len(d.buf) == 0 {
		return nil
	}
	frame := d.buf
	d.buf = nil
	if f, ok := ParseFrame(frame); ok {
		return []Frame{f}
	}
	return []Frame{{Raw: StripDataPrefix(frame)}}
}

// DecodeAll decodes a complete SSE body in one go. It is the convenience
// form for callers that already have the whole body.
func DecodeAll(body []byte) []Frame {
	d := &Decoder{}
	frames := d.Feed(body)
	return append(frames, d.Flush()...)
}
