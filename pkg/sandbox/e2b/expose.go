package e2b

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
	pb "github.com/xiaods/k8e/pkg/sandboxmatrix/grpc/pb/sandbox/v1"
)

// handleExposeProxy reverse-proxies /k8e/expose/<session>/<port>[/path...] to
// the sandbox pod's service port through the k8e API Gateway surface (KIP-24):
// this HTTP server is fronted by the Cilium Gateway API (HTTPRoute :80/:443),
// so an exposed service is reachable at
// http(s)://<gateway>/k8e/expose/<session>/<port>/.
//
// Authorization: the port must be registered in the gateway's expose registry
// (ListExposed RPC, populated by `k8e sandbox expose` / the dsh plugin). Only
// then is the request proxied to http://<podIP>:<port>.
func (s *Server) handleExposeProxy(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/k8e/expose/")
	parts := strings.SplitN(rest, "/", 3)
	if len(parts) < 2 {
		http.Error(w, "usage: /k8e/expose/<session>/<port>[/path...]", http.StatusBadRequest)
		return
	}
	sessionID := parts[0]
	port, err := strconv.Atoi(parts[1])
	if err != nil || port <= 0 || port > 65535 {
		http.Error(w, "invalid port", http.StatusBadRequest)
		return
	}

	// Authorization: the port must be currently exposed for this session.
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	listed, err := s.gw.ListExposed(ctx, &pb.ListExposedRequest{SessionId: sessionID})
	if err != nil {
		// Surface the real cause: this page is exactly where a stale gateway
		// (missing KIP-24 RPCs) or an auth failure shows up first.
		logrus.Warnf("k8e expose proxy %s/%d: ListExposed failed: %v", sessionID, port, err)
		http.Error(w, fmt.Sprintf("gateway unreachable: %v", err), http.StatusBadGateway)
		return
	}
	exposed := false
	for _, svc := range listed.Services {
		if int(svc.Port) == port {
			exposed = true
			break
		}
	}
	if !exposed {
		http.Error(w, fmt.Sprintf("port %d not exposed for session %s", port, sessionID), http.StatusNotFound)
		return
	}

	// Resolve the sandbox pod IP via the gateway.
	sess, err := s.gw.GetSession(ctx, &pb.GetSessionRequest{SessionId: sessionID})
	if err != nil {
		logrus.Warnf("k8e expose proxy %s/%d: GetSession failed: %v", sessionID, port, err)
		http.Error(w, fmt.Sprintf("session unreachable: %v", err), http.StatusServiceUnavailable)
		return
	}
	if sess.PodIp == "" {
		http.Error(w, "session has no pod IP yet", http.StatusServiceUnavailable)
		return
	}

	target := &url.URL{Scheme: "http", Host: fmt.Sprintf("%s:%d", sess.PodIp, port)}
	suffix := "/"
	if len(parts) > 2 && parts[2] != "" {
		suffix = "/" + parts[2]
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	originalHost := r.Host
	proxy.Director = func(req *http.Request) {
		req.URL.Scheme = target.Scheme
		req.URL.Host = target.Host
		// The in-pod service sees only the suffix after /k8e/expose/<sid>/<port>.
		req.URL.Path = suffix
		req.URL.RawPath = ""
		// Preserve the original Host so in-pod services that route on
		// Host/SNI keep working; the proxy only rewrites the dial address.
		req.Host = originalHost
		req.Header.Set("X-K8E-Expose-Session", sessionID)
	}
	proxy.ErrorHandler = func(rw http.ResponseWriter, req *http.Request, proxyErr error) {
		logrus.Debugf("k8e expose proxy %s/%d: %v", sessionID, port, proxyErr)
		rw.WriteHeader(http.StatusBadGateway)
	}
	proxy.ServeHTTP(w, r)
}
