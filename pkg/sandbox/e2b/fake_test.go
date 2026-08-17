package e2b

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"

	pb "github.com/xiaods/k8e/pkg/sandboxmatrix/grpc/pb/sandbox/v1"
)

// fakeGateway is an in-memory Gateway for tests, modeling the k8e gateway's
// session semantics: sessions created with a session_id, exec answers from a
// canned map, files in a map, an exec-stream reader from a canned chunk list.
type fakeGateway struct {
	mu        sync.Mutex
	sessions  map[string]*pb.GetSessionResponse
	files     map[string]string
	execOut   map[string]*pb.ExecResponse
	streams   map[string][]*pb.ExecStreamResponse
	gated     []gatedEntry
	destroyed []string
	created   []*pb.CreateSessionRequest
	readPaths []string
	writeLog  []string

	// createErr, when set, is returned by CreateSession (capacity tests).
	createErr error
	// destroyErr, when set, is returned by DestroySession (conflict tests).
	destroyErr error
	// pauseErr, when set, is returned by PauseSession (ephemeral refusal).
	pauseErr error
	// getErr, when set, is returned by GetSession for the named sandbox.
	getErrs map[string]error
	paused  []string
	resumed []string

	// term records KIP-19 terminal RPCs for pty.* compat tests.
	term *terminalRows
	// hangTerminals makes unseeded TerminalStream calls hang forever (no
	// frames) so a pty.create stays alive across sendInput/resize/kill
	// tests, mirroring a long-running bash session.
	hangTerminals bool
}

func newFakeGateway() *fakeGateway {
	return &fakeGateway{
		sessions: map[string]*pb.GetSessionResponse{},
		files:    map[string]string{},
		execOut:  map[string]*pb.ExecResponse{},
		streams:  map[string][]*pb.ExecStreamResponse{},
		getErrs:  map[string]error{},
	}
}

func (f *fakeGateway) CreateSession(ctx context.Context, req *pb.CreateSessionRequest) (*pb.CreateSessionResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return nil, f.createErr
	}
	id := req.SessionId
	if id == "" {
		id = "sess-1"
	}
	f.sessions[id] = &pb.GetSessionResponse{
		SessionId:    id,
		Phase:        "Active",
		RuntimeClass: req.RuntimeClass,
		PodIp:        "10.0.0.1",
	}
	f.created = append(f.created, req)
	return &pb.CreateSessionResponse{SessionId: id}, nil
}

func (f *fakeGateway) GetSession(ctx context.Context, req *pb.GetSessionRequest) (*pb.GetSessionResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err, ok := f.getErrs[req.SessionId]; ok {
		return nil, err
	}
	s, ok := f.sessions[req.SessionId]
	if !ok {
		return nil, errors.New("session not found")
	}
	return s, nil
}

func (f *fakeGateway) ListSessions(ctx context.Context, req *pb.ListSessionsRequest) (*pb.ListSessionsResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*pb.GetSessionResponse
	for _, s := range f.sessions {
		if req.Phase == "all" || s.Phase == req.Phase || req.Phase == "" {
			out = append(out, s)
		}
	}
	return &pb.ListSessionsResponse{Sessions: out}, nil
}

func (f *fakeGateway) DestroySession(ctx context.Context, req *pb.DestroySessionRequest) (*pb.DestroySessionResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.destroyErr != nil {
		return nil, f.destroyErr
	}
	if _, ok := f.sessions[req.SessionId]; !ok {
		return nil, errors.New("session not found")
	}
	delete(f.sessions, req.SessionId)
	f.destroyed = append(f.destroyed, req.SessionId)
	return &pb.DestroySessionResponse{Ok: true}, nil
}

func (f *fakeGateway) Exec(ctx context.Context, req *pb.ExecRequest) (*pb.ExecResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if out, ok := f.execOut[req.Command]; ok {
		return out, nil
	}
	return &pb.ExecResponse{Stdout: "", Stderr: "", ExitCode: 0}, nil
}

func (f *fakeGateway) ExecStream(ctx context.Context, req *pb.ExecRequest) (ExecStreamReader, error) {
	f.mu.Lock()
	var chunks []*pb.ExecStreamResponse
	for key, c := range f.streams {
		if strings.Contains(req.Command, key) {
			chunks = c
			break
		}
	}
	for _, g := range f.gated {
		if strings.Contains(req.Command, g.match) {
			f.mu.Unlock()
			return g.stream, nil
		}
	}
	f.mu.Unlock()
	return &fakeStream{chunks: chunks}, nil
}

func (f *fakeGateway) WriteFile(ctx context.Context, req *pb.WriteFileRequest) (*pb.WriteFileResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[req.Path] = req.Content
	f.writeLog = append(f.writeLog, req.Path)
	return &pb.WriteFileResponse{Ok: true}, nil
}

func (f *fakeGateway) ReadFile(ctx context.Context, req *pb.ReadFileRequest) (*pb.ReadFileResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.readPaths = append(f.readPaths, req.Path)
	content, ok := f.files[req.Path]
	if !ok {
		return nil, errors.New("file not found")
	}
	return &pb.ReadFileResponse{Content: content}, nil
}

func (f *fakeGateway) PauseSession(ctx context.Context, req *pb.PauseSessionRequest) (*pb.PauseSessionResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pauseErr != nil {
		return nil, f.pauseErr
	}
	s, ok := f.sessions[req.SessionId]
	if !ok {
		return nil, errors.New("session not found")
	}
	s.Phase = "Paused"
	f.paused = append(f.paused, req.SessionId)
	return &pb.PauseSessionResponse{Ok: true}, nil
}

func (f *fakeGateway) ResumeSession(ctx context.Context, req *pb.ResumeSessionRequest) (*pb.ResumeSessionResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.sessions[req.SessionId]
	if !ok {
		return nil, errors.New("session not found")
	}
	s.Phase = "Active"
	f.resumed = append(f.resumed, req.SessionId)
	return &pb.ResumeSessionResponse{Ok: true}, nil
}

func (f *fakeGateway) ListFiles(ctx context.Context, req *pb.ListFilesRequest) (*pb.ListFilesResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*pb.FileEntry
	for path := range f.files {
		out = append(out, &pb.FileEntry{Path: path, Modified: 0})
	}
	return &pb.ListFilesResponse{Files: out}, nil
}

// --- KIP-19 terminal primitive fakes ---------------------------------------

// terminalRows records terminal RPCs for pty.* compat tests.
type terminalRows struct {
	created   []*pb.CreateTerminalRequest
	writes    []*pb.TerminalWriteRequest
	resizes   []*pb.TerminalResizeRequest
	signals   []*pb.TerminalSignalRequest
	destroyed []string
	streams   map[string][]*pb.TerminalStreamResponse
}

func (f *fakeGateway) CreateTerminal(ctx context.Context, req *pb.CreateTerminalRequest) (*pb.CreateTerminalResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.term == nil {
		f.term = &terminalRows{streams: map[string][]*pb.TerminalStreamResponse{}}
	}
	id := req.SessionId + "/t" + strconv.Itoa(len(f.term.created)+1)
	f.term.created = append(f.term.created, req)
	return &pb.CreateTerminalResponse{TerminalId: id, Pid: int32(5000 + len(f.term.created))}, nil
}

func (f *fakeGateway) TerminalStream(ctx context.Context, req *pb.TerminalStreamRequest) (TerminalStreamReader, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.term == nil {
		return nil, errors.New("no terminal")
	}
	chunks := f.term.streams[req.TerminalId]
	if len(chunks) == 0 && f.hangTerminals {
		return &hangingTermStream{}, nil
	}
	// Default frames when a test did not seed a stream: one output chunk
	// then a clean exit, so pty.create/connect paths have a deterministic
	// stream to drain.
	if len(chunks) == 0 {
		chunks = []*pb.TerminalStreamResponse{
			{Frame: &pb.TerminalStreamResponse_Data{Data: []byte("hello tty")}},
			{Frame: &pb.TerminalStreamResponse_Exit{Exit: &pb.TerminalExit{ExitCode: 0}}},
		}
	}
	return &fakeTermStream{chunks: chunks}, nil
}

// hangingTermStream blocks forever — a terminal whose process is still
// running (no exit frame yet).
type hangingTermStream struct{}

func (h *hangingTermStream) Recv() (*pb.TerminalStreamResponse, error) {
	<-make(chan struct{})
	return nil, nil
}

func (f *fakeGateway) TerminalWrite(ctx context.Context, req *pb.TerminalWriteRequest) (*pb.TerminalWriteResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.term == nil {
		return nil, errors.New("no terminal")
	}
	f.term.writes = append(f.term.writes, req)
	return &pb.TerminalWriteResponse{Ok: true}, nil
}

func (f *fakeGateway) TerminalResize(ctx context.Context, req *pb.TerminalResizeRequest) (*pb.TerminalResizeResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.term == nil {
		return nil, errors.New("no terminal")
	}
	f.term.resizes = append(f.term.resizes, req)
	return &pb.TerminalResizeResponse{Ok: true}, nil
}

func (f *fakeGateway) TerminalSignal(ctx context.Context, req *pb.TerminalSignalRequest) (*pb.TerminalSignalResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.term == nil {
		return nil, errors.New("no terminal")
	}
	f.term.signals = append(f.term.signals, req)
	return &pb.TerminalSignalResponse{ProcessGroupId: 1}, nil
}

func (f *fakeGateway) TerminalDestroy(ctx context.Context, req *pb.TerminalDestroyRequest) (*pb.TerminalDestroyResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.term.destroyed = append(f.term.destroyed, req.TerminalId)
	return &pb.TerminalDestroyResponse{Ok: true}, nil
}

// fakeTermStream yields canned terminal frames then EOF.
type fakeTermStream struct {
	chunks []*pb.TerminalStreamResponse
	i      int
}

func (f *fakeTermStream) Recv() (*pb.TerminalStreamResponse, error) {
	if f.i >= len(f.chunks) {
		return nil, errors.New("EOF")
	}
	c := f.chunks[f.i]
	f.i++
	return c, nil
}

// fakeStream yields canned chunks then EOF.
type fakeStream struct {
	chunks []*pb.ExecStreamResponse
	i      int
}

func (f *fakeStream) Recv() (*pb.ExecStreamResponse, error) {
	if f.i >= len(f.chunks) {
		return nil, errors.New("EOF")
	}
	c := f.chunks[f.i]
	f.i++
	return c, nil
}

// gatedEntry pairs a command match with a gated stream.
type gatedEntry struct {
	match  string
	stream *gatedStream
}

// gatedStream yields chunks; Recv blocks after blockAfter deliveries until
// release is closed, then drains the rest and EOF. Lets a test hold a
// process alive while asserting on its table record (the first chunk — the
// pid frame — must flow immediately so the server records the guest pid).
type gatedStream struct {
	release    chan struct{}
	chunks     []*pb.ExecStreamResponse
	blockAfter int
	i          int
}

func (g *gatedStream) Recv() (*pb.ExecStreamResponse, error) {
	if g.i == g.blockAfter {
		<-g.release
	}
	if g.i >= len(g.chunks) {
		return nil, errors.New("EOF")
	}
	c := g.chunks[g.i]
	g.i++
	return c, nil
}

// gatedExecStream makes the fake gateway stream the given chunks, holding
// the stream open until the returned release func is called. release is
// idempotent (safe for explicit calls plus defer).
func (f *fakeGateway) gatedExecStream(match string, chunks []*pb.ExecStreamResponse) func() {
	release := make(chan struct{})
	var once sync.Once
	f.mu.Lock()
	f.gated = append(f.gated, gatedEntry{match: match, stream: &gatedStream{release: release, chunks: chunks, blockAfter: 1}})
	f.mu.Unlock()
	return func() { once.Do(func() { close(release) }) }
}
