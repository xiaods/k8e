package grpc

import (
	"encoding/base64"
	"testing"

	pb "github.com/xiaods/k8e/pkg/sandboxmatrix/grpc/pb/sandbox/v1"
)

func TestTerminalSignalName(t *testing.T) {
	cases := []struct {
		sig  pb.TerminalSignal
		want string
	}{
		{pb.TerminalSignal_TERMINAL_SIGNAL_INT, "SIGINT"},
		{pb.TerminalSignal_TERMINAL_SIGNAL_TERM, "SIGTERM"},
		{pb.TerminalSignal_TERMINAL_SIGNAL_KILL, "SIGKILL"},
		{pb.TerminalSignal_TERMINAL_SIGNAL_TSTP, "SIGTSTP"},
		{pb.TerminalSignal_TERMINAL_SIGNAL_HUP, "SIGHUP"},
	}
	for _, c := range cases {
		got, err := terminalSignalName(c.sig)
		if err != nil {
			t.Fatalf("terminalSignalName(%v): unexpected error %v", c.sig, err)
		}
		if got != c.want {
			t.Errorf("terminalSignalName(%v) = %q, want %q", c.sig, got, c.want)
		}
	}
	if _, err := terminalSignalName(pb.TerminalSignal_TERMINAL_SIGNAL_UNSPECIFIED); err == nil {
		t.Error("terminalSignalName(UNSPECIFIED) should error")
	}
}

func TestParsePTYStreamFrame(t *testing.T) {
	// pid frame: no data, no exit
	f, err := parsePTYStreamFrame(`{"pid":123}`)
	if err != nil {
		t.Fatalf("pid frame: %v", err)
	}
	if f.data != nil || f.exit != nil {
		t.Errorf("pid frame should carry no data/exit, got %+v", f)
	}

	// data frame: base64-decoded output
	payload := base64.StdEncoding.EncodeToString([]byte("hello\nworld"))
	f, err = parsePTYStreamFrame(payload)
	if err != nil {
		t.Fatalf("data frame: %v", err)
	}
	if string(f.data) != "hello\nworld" {
		t.Errorf("data frame decoded to %q", f.data)
	}
	if f.exit != nil {
		t.Errorf("data frame should carry no exit, got %+v", f.exit)
	}

	// exit frame: terminal exit facts
	f, err = parsePTYStreamFrame(`{"exit":42,"signal":"SIGTERM"}`)
	if err != nil {
		t.Fatalf("exit frame: %v", err)
	}
	if f.exit == nil || f.exit.ExitCode != 42 || f.exit.Signal != "SIGTERM" {
		t.Errorf("exit frame = %+v", f.exit)
	}
	if f.data != nil {
		t.Errorf("exit frame should carry no data, got %q", f.data)
	}

	// invalid base64
	if _, err := parsePTYStreamFrame("not-base64!!"); err == nil {
		t.Error("invalid base64 should error")
	}

	// invalid JSON control frame
	if _, err := parsePTYStreamFrame(`{"exit":`); err == nil {
		t.Error("malformed JSON control frame should error")
	}
}

func TestTerminalStreamResponseWrapHelpers(t *testing.T) {
	// Guard the oneof wrapper types we rely on remain constructible.
	_ = &pb.TerminalStreamResponse{Frame: &pb.TerminalStreamResponse_Data{Data: []byte("x")}}
	_ = &pb.TerminalStreamResponse{Frame: &pb.TerminalStreamResponse_Exit{Exit: &pb.TerminalExit{ExitCode: 0, Signal: ""}}}
}
