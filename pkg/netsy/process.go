package netsy

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	defaultReadyTimeout = 90 * time.Second
	defaultPollInterval = 500 * time.Millisecond
	// logTailBytes bounds the captured Netsy output included in errors.
	logTailBytes = 16 * 1024
	stopTimeout  = 10 * time.Second
)

// Process is a running Netsy datastore supervised by k8e.
type Process struct {
	cmd        *exec.Cmd
	certs      CertPaths
	endpoint   string
	readyAfter time.Duration
	poll       time.Duration
	logs       *tailBuffer
	done       chan struct{}
	waitErr    error
}

// Start renders the Netsy config, ensures the mTLS PKI exists, launches the
// netsy process and blocks until its client API accepts writes, i.e. until the
// elector has promoted this node to Primary. On any failure the child process is
// terminated before returning.
func Start(ctx context.Context, cfg Config) (*Process, error) {
	cfg = cfg.WithDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(cfg.DataDir, 0700); err != nil {
		return nil, fmt.Errorf("failed to create netsy data dir: %w", err)
	}

	certs, err := EnsurePKI(cfg.CertDir, cfg.ClusterID, cfg.NodeID, DefaultClientName, cfg.ServerHosts())
	if err != nil {
		return nil, err
	}

	data, err := cfg.RenderClusterConfig()
	if err != nil {
		return nil, err
	}
	configPath := filepath.Join(cfg.DataDir, "netsy.jsonc")
	if err := os.WriteFile(configPath, data, 0600); err != nil {
		return nil, fmt.Errorf("failed to write netsy config %s: %w", configPath, err)
	}

	logs := &tailBuffer{max: logTailBytes}
	cmd := exec.CommandContext(ctx, cfg.Binary, "--config", configPath)
	cmd.Env = mergeEnv(os.Environ(), cfg.Environ(configPath, certs))
	cmd.Stdout = logs
	cmd.Stderr = logs

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start netsy (%s): %w", cfg.Binary, err)
	}

	p := &Process{
		cmd:        cmd,
		certs:      certs,
		endpoint:   cfg.Endpoint(),
		readyAfter: cfg.ReadyTimeout,
		poll:       cfg.HealthPollInterval,
		logs:       logs,
		done:       make(chan struct{}),
	}
	go func() {
		p.waitErr = cmd.Wait()
		close(p.done)
	}()

	if err := p.waitReady(ctx, certs); err != nil {
		p.Stop()
		return nil, err
	}
	return p, nil
}

// Endpoint returns the etcd-compatible client API endpoint.
func (p *Process) Endpoint() string { return p.endpoint }

// CertPaths returns the PKI k8e uses to authenticate to Netsy.
func (p *Process) CertPaths() CertPaths { return p.certs }

// Logs returns the captured Netsy output.
func (p *Process) Logs() string { return p.logs.String() }

// Wait blocks until the Netsy process exits and returns its error, if any.
func (p *Process) Wait() error {
	<-p.done
	return p.waitErr
}

// Stop terminates the Netsy process, escalating to SIGKILL after a grace period.
func (p *Process) Stop() {
	if p.cmd == nil || p.cmd.Process == nil {
		return
	}
	select {
	case <-p.done:
		return
	default:
	}
	_ = p.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-p.done:
	case <-time.After(stopTimeout):
		_ = p.cmd.Process.Kill()
		<-p.done
	}
}

// mergeEnv overlays override entries on base, dropping any existing key so the
// override wins deterministically.
func mergeEnv(base, override []string) []string {
	keys := make(map[string]struct{}, len(override))
	for _, e := range override {
		if i := strings.IndexByte(e, '='); i > 0 {
			keys[e[:i]] = struct{}{}
		}
	}
	merged := make([]string, 0, len(base)+len(override))
	for _, e := range base {
		if i := strings.IndexByte(e, '='); i > 0 {
			if _, ok := keys[e[:i]]; ok {
				continue
			}
		}
		merged = append(merged, e)
	}
	return append(merged, override...)
}

// tailBuffer keeps the last max bytes written to it, unsynchronized for use as
// a subprocess output sink.
type tailBuffer struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	if len(b.buf) > b.max {
		b.buf = append(b.buf[:0], b.buf[len(b.buf)-b.max:]...)
	}
	return len(p), nil
}

func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}
