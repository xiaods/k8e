package grpc

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	sandboxv1 "github.com/xiaods/k8e/pkg/sandboxmatrix/api/v1alpha1"
	"github.com/xiaods/k8e/pkg/sandboxmatrix/grpc/pb/sandbox/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// KIP-24 service exposure via the k8e API Gateway: an in-pod service port is
// made reachable through the Cilium Gateway API (HTTPRoute :80/:443 → embedded
// e2b HTTP server → reverse proxy to http://<podIP>:<port>). No inbound
// exposure of the pod: the e2b server (same process as the gRPC gateway)
// proxies to the pod's cluster IP, and the per-session CNP gains an ingress
// rule allowing the gateway/e2b-server to the exposed port.
//
// Design: docs/sandbox-expose-tunnel.md

// ExposedEntry is one live gateway-proxied exposure for a session.
type ExposedEntry struct {
	Port      int
	Host      string // in-pod listen address recorded at expose time (informational)
	URL       string // public gateway URL
	StartedAt time.Time
}

// exposeURLPath is the e2b HTTP reverse-proxy route prefix (Gateway-API
// fronted). Full URL: <gateway-base>/k8e/expose/<session>/<port>/
const exposeURLPath = "/k8e/expose/%s/%d/"

// ExposeService registers an in-pod service port for gateway proxying and
// re-applies the session CNP so the gateway/e2b-server may reach the port.
// Idempotent: exposing the same port twice returns the existing URL.
//
// baseURL is the public gateway base (e.g. http://gw.example.com) the caller
// will use to reach the exposed service; the server supplies it from its
// advertised hostname.
func (o *Orchestrator) ExposeService(ctx context.Context, sessionID string, port int32, host, baseURL string) (*pb.ExposeServiceResponse, error) {
	if port <= 0 || port > 65535 {
		return nil, status.Errorf(codes.InvalidArgument, "port must be in [1, 65535]")
	}
	if host == "" {
		host = "127.0.0.1"
	}

	// Idempotent fast path: the port is already exposed for this session.
	o.exposeMu.Lock()
	for _, e := range o.exposed[sessionID] {
		if e.Port == int(port) {
			resp := &pb.ExposeServiceResponse{Url: e.URL}
			o.exposeMu.Unlock()
			return resp, nil
		}
	}
	o.exposeMu.Unlock()

	// The session must exist (and its pod must be reachable for the proxy).
	session, err := o.getSession(ctx, sessionID)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "session %s not found", sessionID)
	}

	url := fmt.Sprintf("%s%s", baseURL, fmt.Sprintf(exposeURLPath, sessionID, port))
	entry := &ExposedEntry{Port: int(port), Host: host, URL: url, StartedAt: time.Now()}
	o.exposeMu.Lock()
	o.exposed[sessionID] = append(o.exposed[sessionID], entry)
	o.exposeMu.Unlock()

	// Allow gateway/e2b-server ingress to the exposed port on the CNP.
	if err := o.applySessionCNP(ctx, session); err != nil {
		// Roll the registry entry back so a failed CNP apply never leaves a
		// half-exposed port that the proxy route will 404 on.
		o.removeExposed(sessionID, int(port))
		return nil, status.Errorf(codes.Internal, "expose: apply CNP: %v", err)
	}
	return &pb.ExposeServiceResponse{Url: url}, nil
}

// removeExposed deletes one port from a session's registry (no-op when absent).
func (o *Orchestrator) removeExposed(sessionID string, port int) {
	o.exposeMu.Lock()
	defer o.exposeMu.Unlock()
	entries := o.exposed[sessionID]
	rest := entries[:0]
	for _, e := range entries {
		if e.Port != port {
			rest = append(rest, e)
		}
	}
	if len(rest) == 0 {
		delete(o.exposed, sessionID)
	} else {
		o.exposed[sessionID] = rest
	}
}

// UnexposeService removes the gateway proxy registration for a port and
// re-applies the CNP without it. Idempotent: unexposing a port that is not
// exposed returns ok=false without error.
func (o *Orchestrator) UnexposeService(ctx context.Context, sessionID string, port int32) (*pb.UnexposeServiceResponse, error) {
	o.exposeMu.Lock()
	_, found := o.findExposedLocked(sessionID, int(port))
	o.exposeMu.Unlock()
	if !found {
		return &pb.UnexposeServiceResponse{Ok: false}, nil
	}
	o.removeExposed(sessionID, int(port))

	if session, err := o.getSession(ctx, sessionID); err == nil {
		if err := o.applySessionCNP(ctx, session); err != nil {
			return nil, status.Errorf(codes.Internal, "unexpose: apply CNP: %v", err)
		}
	}
	return &pb.UnexposeServiceResponse{Ok: true}, nil
}

func (o *Orchestrator) findExposedLocked(sessionID string, port int) (*ExposedEntry, bool) {
	for _, e := range o.exposed[sessionID] {
		if e.Port == port {
			return e, true
		}
	}
	return nil, false
}

// ListExposed returns the current exposures for a session. Entries whose
// session is gone are pruned.
func (o *Orchestrator) ListExposed(ctx context.Context, sessionID string) (*pb.ListExposedResponse, error) {
	o.exposeMu.Lock()
	entries := append([]*ExposedEntry(nil), o.exposed[sessionID]...)
	o.exposeMu.Unlock()

	// Prune entries for sessions that no longer exist.
	if _, err := o.getSession(ctx, sessionID); err != nil {
		o.exposeMu.Lock()
		delete(o.exposed, sessionID)
		o.exposeMu.Unlock()
		entries = nil
	}

	services := make([]*pb.ExposedService, 0, len(entries))
	for _, e := range entries {
		services = append(services, &pb.ExposedService{
			Port:      int32(e.Port),
			Url:       e.URL,
			Host:      e.Host,
			StartedAt: e.StartedAt.Unix(),
		})
	}
	return &pb.ListExposedResponse{Services: services}, nil
}

// UpdateAllowedHosts replaces the session's egress allowlist and re-applies
// the per-session CNP so the change is live (FQDN mode) / declared (default
// world-443 mode). Empty list clears the override (falls back to matrix
// defaults on the next create). Existing exposed ports are preserved.
func (o *Orchestrator) UpdateAllowedHosts(ctx context.Context, sessionID string, hosts []string) ([]string, error) {
	session, err := o.getSession(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	session.Spec.AllowedHosts = hosts
	o.updateSession(ctx, session)
	if err := o.applySessionCNP(ctx, session); err != nil {
		return nil, status.Errorf(codes.Internal, "update allowed hosts: apply CNP: %v", err)
	}
	return hosts, nil
}

// applySessionCNP rebuilds and applies the session CNP including any exposed
// service ports (gateway/e2b-server ingress) plus the session's allowedHosts
// egress. Central chokepoint so expose/unexpose/allow-hosts never clobber
// each other.
func (o *Orchestrator) applySessionCNP(ctx context.Context, session *sandboxv1.SandboxSession) error {
	o.exposeMu.Lock()
	var ports []int32
	for _, e := range o.exposed[session.Name] {
		ports = append(ports, int32(e.Port))
	}
	o.exposeMu.Unlock()

	obj := buildSessionCNPExposed(session, o.fqdnEnabled(), ports)
	name := fmt.Sprintf("sandbox-session-%s", session.Name)
	_, err := o.dynamic.Resource(cnpGVR).Namespace(session.Namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = o.dynamic.Resource(cnpGVR).Namespace(session.Namespace).Create(ctx, obj, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}
	_, err = o.dynamic.Resource(cnpGVR).Namespace(session.Namespace).Update(ctx, obj, metav1.UpdateOptions{})
	return err
}
