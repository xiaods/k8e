package e2b

// ptyRow is the compat layer's pid -> terminal_id bridge (KIP-19 M4). The
// E2B SDK addresses pty handles by the session-leader pid returned from
// pty.create; the k8e gateway only speaks canonical terminal_id, so the
// create path records the association here and every later pty RPC
// (SendInput / Update / SendSignal / Connect / kill) resolves through it.
type ptyRow struct {
	terminalID string
	sandboxID  string
	// pid is the E2B-facing session-leader pid echoed to the SDK.
	pid int
	// rows/cols is the latest known window size (resize target).
	rows int32
	cols int32
}

// registerPty records the pid -> terminal association for a pty.create.
func (s *Server) registerPty(row *ptyRow) {
	s.ptysMu.Lock()
	defer s.ptysMu.Unlock()
	s.ptys[row.pid] = row
}

// ptyFor resolves an E2B-facing pid to its terminal row, if it is a PTY
// session created through this compat layer.
func (s *Server) ptyFor(pid int) (*ptyRow, bool) {
	s.ptysMu.Lock()
	defer s.ptysMu.Unlock()
	row, ok := s.ptys[pid]
	return row, ok
}

// dropPty removes the pid -> terminal association (terminal exit or
// destroy). Returns the row that was removed, if any.
func (s *Server) dropPty(pid int) (*ptyRow, bool) {
	s.ptysMu.Lock()
	defer s.ptysMu.Unlock()
	row, ok := s.ptys[pid]
	if ok {
		delete(s.ptys, pid)
	}
	return row, ok
}
