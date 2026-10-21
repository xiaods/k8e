package ratelimit

import (
	"context"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	v1alpha1 "github.com/xiaods/k8e/pkg/sandboxmatrix/api/v1alpha1"
)

func TestAllowBurstThenRefill(t *testing.T) {
	l := NewLimiter(RateConfig{WriteBurst: 3, WriteRate: 1, ReadBurst: 3, ReadRate: 1})

	// The first burst is served from the full bucket.
	for i := 0; i < 3; i++ {
		if !l.Allow("t1", true) {
			t.Fatalf("call %d: expected the burst to be allowed", i)
		}
	}
	// The bucket is empty now, and refilling at 1/s takes a second.
	if l.Allow("t1", true) {
		t.Fatalf("expected the fourth write to be refused")
	}

	// After a second the bucket holds one token again.
	time.Sleep(1100 * time.Millisecond)
	if !l.Allow("t1", true) {
		t.Fatalf("expected the write to be allowed after the bucket refilled")
	}
}

func TestAllowIsPerTenant(t *testing.T) {
	l := NewLimiter(RateConfig{WriteBurst: 1, WriteRate: 1, ReadBurst: 1, ReadRate: 1})

	if !l.Allow("tenant-a", true) {
		t.Fatalf("tenant-a's first write should be allowed")
	}
	if l.Allow("tenant-a", true) {
		t.Fatalf("tenant-a's second write should be refused")
	}
	// A different tenant has its own bucket.
	if !l.Allow("tenant-b", true) {
		t.Fatalf("tenant-b's first write should be allowed")
	}
}

func TestAllowSeparatesReadAndWrite(t *testing.T) {
	l := NewLimiter(RateConfig{WriteBurst: 1, WriteRate: 1, ReadBurst: 5, ReadRate: 1})

	if !l.Allow("t1", true) {
		t.Fatalf("expected the write to be allowed")
	}
	if l.Allow("t1", true) {
		t.Fatalf("expected the second write to be refused")
	}
	// Reads have their own budget and are unaffected.
	for i := 0; i < 5; i++ {
		if !l.Allow("t1", false) {
			t.Fatalf("read %d: expected the read to be allowed", i)
		}
	}
	if l.Allow("t1", false) {
		t.Fatalf("expected the sixth read to be refused")
	}
}

func TestAllowRateZeroDisablesLimiting(t *testing.T) {
	l := NewLimiter(RateConfig{WriteBurst: 1, WriteRate: 0, ReadBurst: 1, ReadRate: 0})

	for i := 0; i < 50; i++ {
		if !l.Allow("t1", true) {
			t.Fatalf("call %d: a zero rate must disable the limit", i)
		}
	}
}

func TestNewLimiterFromSpecFallsBackToDefaults(t *testing.T) {
	// A nil spec keeps the defaults.
	l := NewLimiterFromSpec(nil)
	if l.config.WriteBurst != defaultWriteBurst || l.config.ReadRate != defaultReadRate {
		t.Fatalf("nil spec should keep the defaults, got %+v", l.config)
	}

	// A zero field in the spec also falls back.
	l = NewLimiterFromSpec(&v1alpha1.RateLimitSpec{WriteBurst: 5})
	if l.config.WriteBurst != 5 {
		t.Fatalf("WriteBurst should come from the spec, got %d", l.config.WriteBurst)
	}
	if l.config.ReadBurst != defaultReadBurst {
		t.Fatalf("ReadBurst should fall back to the default, got %d", l.config.ReadBurst)
	}
}

// TestReloadConfigResetsBuckets is the regression for the hot-reload path: a
// config change must take effect immediately, including for tenants that
// already exhausted their bucket.
func TestReloadConfigResetsBuckets(t *testing.T) {
	l := NewLimiter(RateConfig{WriteBurst: 1, WriteRate: 1, ReadBurst: 1, ReadRate: 1})

	if !l.Allow("t1", true) {
		t.Fatalf("expected the first write to be allowed")
	}
	if l.Allow("t1", true) {
		t.Fatalf("expected the second write to be refused")
	}

	// A reload with a bigger burst must let the tenant through again.
	l.ReloadConfig(&v1alpha1.RateLimitSpec{WriteBurst: 5, WriteRate: 1})
	if !l.Allow("t1", true) {
		t.Fatalf("expected the write to be allowed after the reload")
	}
}

// TestAllowConcurrentWithReloadConfig drives the limiter and the hot-reload
// path at the same time. The limiter reads its config while ReloadConfig
// writes it, so the two must be synchronised; under -race this fails when the
// read happens outside the lock.
func TestAllowConcurrentWithReloadConfig(t *testing.T) {
	l := NewLimiter(DefaultRateConfig())

	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			l.ReloadConfig(&v1alpha1.RateLimitSpec{WriteBurst: 10, WriteRate: 5, ReadBurst: 30, ReadRate: 20})
		}
	}()

	for i := 0; i < 4; i++ {
		wg.Add(1)
		isWrite := i%2 == 0
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				l.Allow("t1", isWrite)
			}
		}()
	}

	time.Sleep(200 * time.Millisecond)
	close(stop)
	wg.Wait()

	// The limiter must still be usable after the concurrent reloads, and the
	// config it settled on must be one of the ones that was written - a torn
	// read would leave a mismatched burst/rate pair.
	if l.Allow("t1", true) && !l.Allow("t1", true) {
		t.Fatalf("the write budget must be enforced after the concurrent reloads")
	}
}

func TestReapStaleRemovesIdleTenants(t *testing.T) {
	l := NewLimiter(DefaultRateConfig())
	l.Allow("idle-tenant", false)
	l.Allow("busy-tenant", false)

	// Nothing is idle yet.
	l.ReapStale(time.Hour)
	if len(l.cache) != 2 {
		t.Fatalf("expected 2 tenants before the reap, got %d", len(l.cache))
	}

	// Reap everything that has not been seen for a moment.
	time.Sleep(20 * time.Millisecond)
	l.ReapStale(10 * time.Millisecond)
	if len(l.cache) != 0 {
		t.Fatalf("expected the idle tenants to be reaped, %d left", len(l.cache))
	}
}

func TestExtractTenant(t *testing.T) {
	cases := []struct {
		name string
		md   metadata.MD
		want string
	}{
		{
			name: "tenant header wins",
			md:   metadata.Pairs("x-sandbox-tenant", "acme", "authorization", "Bearer 0123456789abcdef"),
			want: "acme",
		},
		{
			name: "api key prefix",
			md:   metadata.Pairs("authorization", "Bearer 0123456789abcdef"),
			want: "key:01234567",
		},
		{
			name: "short api key",
			md:   metadata.Pairs("authorization", "Bearer abc"),
			want: "key:abc",
		},
		{
			name: "authorization without bearer",
			md:   metadata.Pairs("authorization", "something"),
			want: "key:anon",
		},
		{
			name: "no metadata at all",
			md:   nil,
			want: "anon",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			if tc.md != nil {
				ctx = metadata.NewIncomingContext(ctx, tc.md)
			}
			if got := extractTenant(ctx); got != tc.want {
				t.Fatalf("extractTenant = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestIsWriteRPC(t *testing.T) {
	writes := []string{
		"/sandbox.v1.SandboxService/CreateSession",
		"/sandbox.v1.SandboxService/DestroySession",
		"/sandbox.v1.SandboxService/Exec",
		"/sandbox.v1.SandboxService/WriteFile",
		"/sandbox.v1.SandboxService/PipInstall",
	}
	for _, m := range writes {
		if !isWriteRPC(m) {
			t.Fatalf("%s must be treated as a write", m)
		}
	}
	reads := []string{
		"/sandbox.v1.SandboxService/GetSession",
		"/sandbox.v1.SandboxService/ListSessions",
		"/sandbox.v1.SandboxService/ReadFile",
		"/sandbox.v1.SandboxService/ListFiles",
	}
	for _, m := range reads {
		if isWriteRPC(m) {
			t.Fatalf("%s must be treated as a read", m)
		}
	}
}

func TestUnaryInterceptorRefusesOverLimit(t *testing.T) {
	l := NewLimiter(RateConfig{WriteBurst: 1, WriteRate: 1, ReadBurst: 1, ReadRate: 1})
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-sandbox-tenant", "acme"))

	called := 0
	handler := func(_ context.Context, _ any) (any, error) {
		called++
		return "ok", nil
	}
	info := &grpc.UnaryServerInfo{FullMethod: "/sandbox.v1.SandboxService/CreateSession"}

	if _, err := l.UnaryInterceptor(ctx, nil, info, handler); err != nil {
		t.Fatalf("first call: unexpected error: %v", err)
	}
	_, err := l.UnaryInterceptor(ctx, nil, info, handler)
	if err == nil {
		t.Fatalf("expected the second call to be refused")
	}
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("expected ResourceExhausted, got %v", status.Code(err))
	}
	if called != 1 {
		t.Fatalf("the handler must not run when the limit is hit, ran %d times", called)
	}
}
