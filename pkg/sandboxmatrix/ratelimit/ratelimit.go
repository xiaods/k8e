// Package ratelimit provides per-tenant token bucket rate limiting for sandbox gRPC calls.
package ratelimit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	v1alpha1 "github.com/xiaods/k8e/pkg/sandboxmatrix/api/v1alpha1"
)

const (
	defaultWriteBurst = 10
	defaultWriteRate  = 5.0
	defaultReadBurst  = 30
	defaultReadRate   = 20.0
)

// Limiter is a per-tenant token bucket rate limiter.
type Limiter struct {
	mu     sync.Mutex
	cache  map[string]*tenantBuckets
	config RateConfig
}

// tenantBuckets holds a tenant's read and write buckets.
//
// They are separate buckets on purpose: a shared one let a burst of writes
// consume the read budget (and the other way round), so the configured
// ReadBurst/WriteBurst did not mean what the operator set - a burst of
// writes could throttle reads for the same tenant.
type tenantBuckets struct {
	read  *tokenBucket
	write *tokenBucket
}

// RateConfig holds burst and rate for read and write operations.
type RateConfig struct {
	WriteBurst int
	WriteRate  float64
	ReadBurst  int
	ReadRate   float64
}

type tokenBucket struct {
	tokens   float64
	lastFill time.Time
	burst    int
	rate     float64
}

// DefaultRateConfig returns the default rate limits.
func DefaultRateConfig() RateConfig {
	return RateConfig{
		WriteBurst: defaultWriteBurst,
		WriteRate:  defaultWriteRate,
		ReadBurst:  defaultReadBurst,
		ReadRate:   defaultReadRate,
	}
}

// NewLimiter creates a Limiter with the given config.
func NewLimiter(cfg RateConfig) *Limiter {
	return &Limiter{
		cache:  make(map[string]*tenantBuckets),
		config: cfg,
	}
}

// NewLimiterFromSpec creates a Limiter from a SandboxMatrix CRD spec, falling back to defaults.
func NewLimiterFromSpec(spec *v1alpha1.RateLimitSpec) *Limiter {
	cfg := DefaultRateConfig()
	if spec != nil {
		if spec.WriteBurst > 0 {
			cfg.WriteBurst = spec.WriteBurst
		}
		if spec.WriteRate > 0 {
			cfg.WriteRate = spec.WriteRate
		}
		if spec.ReadBurst > 0 {
			cfg.ReadBurst = spec.ReadBurst
		}
		if spec.ReadRate > 0 {
			cfg.ReadRate = spec.ReadRate
		}
	}
	return NewLimiter(cfg)
}

// ReloadConfig updates the limiter config from a CRD spec.
func (l *Limiter) ReloadConfig(spec *v1alpha1.RateLimitSpec) {
	cfg := DefaultRateConfig()
	if spec != nil {
		if spec.WriteBurst > 0 {
			cfg.WriteBurst = spec.WriteBurst
		}
		if spec.WriteRate > 0 {
			cfg.WriteRate = spec.WriteRate
		}
		if spec.ReadBurst > 0 {
			cfg.ReadBurst = spec.ReadBurst
		}
		if spec.ReadRate > 0 {
			cfg.ReadRate = spec.ReadRate
		}
	}
	l.mu.Lock()
	l.config = cfg
	l.cache = make(map[string]*tenantBuckets) // reset buckets on config change
	l.mu.Unlock()
}

// Allow checks if the tenant has a token available for the given operation type.
func (l *Limiter) Allow(tenant string, isWrite bool) bool {
	// The config is read under the lock: ReloadConfig replaces it, and
	// reading it outside would race with that (and could pair a new burst
	// with an old rate).
	l.mu.Lock()
	defer l.mu.Unlock()

	cfg := l.config
	rate := cfg.ReadRate
	if isWrite {
		rate = cfg.WriteRate
	}
	if rate <= 0 {
		return true // rate limiting disabled
	}

	tb, ok := l.cache[tenant]
	if !ok {
		tb = &tenantBuckets{
			read:  &tokenBucket{tokens: float64(cfg.ReadBurst), lastFill: time.Now(), burst: cfg.ReadBurst, rate: cfg.ReadRate},
			write: &tokenBucket{tokens: float64(cfg.WriteBurst), lastFill: time.Now(), burst: cfg.WriteBurst, rate: cfg.WriteRate},
		}
		l.cache[tenant] = tb
	}

	b := tb.read
	if isWrite {
		b = tb.write
	}

	now := time.Now()
	elapsed := now.Sub(b.lastFill).Seconds()
	b.tokens = min(float64(b.burst), b.tokens+elapsed*b.rate)
	b.lastFill = now

	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// ReapStale removes tenants that haven't been seen for the given duration.
// Call periodically to prevent unbounded memory growth.
func (l *Limiter) ReapStale(maxAge time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := time.Now().Add(-maxAge)
	for tenant, tb := range l.cache {
		// A tenant is stale only when BOTH of its buckets are idle: the read
		// and the write bucket are filled on their own schedule, so either
		// one being fresh means the tenant is still active.
		if tb.read.lastFill.Before(cutoff) && tb.write.lastFill.Before(cutoff) {
			delete(l.cache, tenant)
		}
	}
}

// extractTenant derives the rate-limit bucket key from the AUTHENTICATED
// caller identity.
//
// It deliberately ignores any client-supplied tenant metadata. The bucket key
// is a security control: a value the caller controls would let it pick a fresh
// bucket per request (evading the limit entirely) or deliberately target
// another tenant's bucket (denying that tenant service). The identity used
// here is, in order:
//
//  1. the mTLS client certificate's CommonName — the API key name the gateway
//     signed the cert for, which the caller cannot change without the CA key;
//  2. a SHA-256 of the bearer token presented to Login (never the raw token,
//     and never a short prefix of it, which would be collision-prone);
//  3. "anon" for unauthenticated callers (loopback with no cert).
//
// This also fixes the pre-existing behaviour where every mTLS client hashed to
// "anon" (they send neither the tenant header nor an Authorization header), so
// all remote clients shared one global bucket and one noisy tenant could starve
// everyone else.
func extractTenant(ctx context.Context) string {
	if p, ok := peer.FromContext(ctx); ok {
		if tlsInfo, ok := p.AuthInfo.(credentials.TLSInfo); ok && len(tlsInfo.State.PeerCertificates) > 0 {
			if cn := tlsInfo.State.PeerCertificates[0].Subject.CommonName; cn != "" {
				return "cert:" + cn
			}
		}
		// Loopback peers (the embedded e2b server) get their own bucket so a
		// remote unauthenticated client cannot drain the budget the in-process
		// data plane depends on.
		if isLoopbackAddr(p.Addr) {
			return "local"
		}
	}
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if vals := md.Get("authorization"); len(vals) > 0 {
			token := strings.TrimPrefix(vals[0], "Bearer ")
			if token != "" {
				sum := sha256.Sum256([]byte(token))
				return "key:" + hex.EncodeToString(sum[:8])
			}
		}
	}
	return "anon"
}

// isLoopbackAddr reports whether addr is a loopback TCP address.
func isLoopbackAddr(addr net.Addr) bool {
	if addr == nil {
		return false
	}
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// UnaryInterceptor returns a gRPC unary interceptor that enforces rate limits.
func (l *Limiter) UnaryInterceptor(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	tenant := extractTenant(ctx)
	isWrite := isWriteRPC(info.FullMethod)
	if !l.Allow(tenant, isWrite) {
		return nil, status.Errorf(codes.ResourceExhausted, "rate limit exceeded for tenant %s — slow down and retry", tenant)
	}
	return handler(ctx, req)
}

// StreamInterceptor returns a gRPC stream interceptor that enforces rate limits.
func (l *Limiter) StreamInterceptor(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	tenant := extractTenant(ss.Context())
	isWrite := isWriteRPC(info.FullMethod)
	if !l.Allow(tenant, isWrite) {
		return status.Errorf(codes.ResourceExhausted, "rate limit exceeded for tenant %s — slow down and retry", tenant)
	}
	return handler(srv, ss)
}

// isWriteRPC returns true for mutating sandbox RPCs.
func isWriteRPC(method string) bool {
	switch method {
	case "/sandbox.v1.SandboxService/CreateSession",
		"/sandbox.v1.SandboxService/DestroySession",
		"/sandbox.v1.SandboxService/Exec",
		"/sandbox.v1.SandboxService/ExecStream",
		"/sandbox.v1.SandboxService/PipInstall",
		"/sandbox.v1.SandboxService/RunSubAgent",
		"/sandbox.v1.SandboxService/WriteFile",
		"/sandbox.v1.SandboxService/GetCACert",
		"/sandbox.v1.SandboxService/ConfirmAction",
		"/sandbox.v1.SandboxService/ApproveAction":
		return true
	default:
		return false
	}
}
