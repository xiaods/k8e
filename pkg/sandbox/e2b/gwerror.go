package e2b

import (
	"errors"
	"net/http"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// gwErrorToE2B maps a gateway gRPC error onto the E2B control-plane dialect
// with the fine-grained semantics CubeSandbox established:
//
//	NotFound / pod gone       → 404  (SDK's kill()===false key)
//	ResourceExhausted (pool)  → 409  (capacity conflict, not a quota error)
//	Unavailable               → 503  (backend not reachable — retry)
//	FailedPrecondition        → 409  (lifecycle conflict — e.g. secret resolve)
//	Canceled / deadline       → 504
//	everything else           → 500  (generic; details stay server-side)
func gwErrorToE2B(err error, fallbackMessage string) *E2bError {
	if err == nil {
		return nil
	}
	st, ok := status.FromError(err)
	if !ok {
		return apiError(http.StatusInternalServerError, fallbackMessage)
	}
	switch st.Code() {
	case codes.NotFound:
		return apiError(http.StatusNotFound, st.Message())
	case codes.ResourceExhausted:
		// Warm-pool capacity: a resource conflict, not a rate limit. 409
		// matches CubeSandbox's capacity refusal and avoids the SDK treating
		// it as a retry-after quota.
		return apiError(http.StatusConflict, st.Message())
	case codes.Unavailable:
		return apiError(http.StatusServiceUnavailable, st.Message())
	case codes.FailedPrecondition:
		return apiError(http.StatusConflict, st.Message())
	case codes.Canceled, codes.DeadlineExceeded:
		return apiError(http.StatusGatewayTimeout, "operation timed out: "+st.Message())
	default:
		return apiError(http.StatusInternalServerError, fallbackMessage)
	}
}

// retryAfterHeader attaches a Retry-After header for lifecycle-conflict
// errors, mirroring CubeSandbox's 503 + Retry-After contract (a client should
// wait before retrying a conflicting lifecycle operation).
func (s *Server) writeConflict(w http.ResponseWriter, e *E2bError, retrySeconds int) {
	w.Header().Set("Retry-After", itoa(retrySeconds))
	s.writeControlError(w, e)
}

// isConflictError reports whether a gateway error is a transient lifecycle
// conflict (pause/resume in progress) that should carry Retry-After.
func isConflictError(err error) bool {
	if err == nil {
		return false
	}
	st, ok := status.FromError(err)
	if !ok {
		return false
	}
	switch st.Code() {
	case codes.FailedPrecondition, codes.Aborted, codes.Unavailable:
		return true
	}
	return strings.Contains(strings.ToLower(st.Message()), "paus")
}

// goneOrNotFound decides between 404 and 410 for a destroyed sandbox.
// The SDK keys kill()===false on 404; a sandbox destroyed while a lifecycle
// operation was in flight answers 410 Gone so SDK clients stop retrying
// (CubeSandbox's auto-kill failure mode).
func (s *Server) goneOrNotFound(w http.ResponseWriter, err error, sandboxID string) {
	if isConflictError(err) {
		s.writeConflict(w, apiError(http.StatusServiceUnavailable,
			"sandbox lifecycle operation in progress; retry"), 2)
		return
	}
	s.writeControlError(w, apiError(http.StatusNotFound, "sandbox \""+sandboxID+"\" not found"))
}

var _ = errors.Is
