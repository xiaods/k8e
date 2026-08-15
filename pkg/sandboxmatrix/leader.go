// Leader election for the SandboxMatrix controller.
//
// k8e supports HA control planes (cluster-init / datastore-endpoint), and the
// sandbox gRPC gateway + orchestrator runs embedded in every server process.
// The gateway itself is stateless and harmless per-node, but the reconcilers
// (warm pool, idle reaper, resetting detector, GC) are NOT — two servers
// reconciling the same warm pool would double-create pods, double-GC, and
// double-reap. This file gates exactly those goroutines behind a single
// coordination.k8s.io Lease: only the elected leader runs the reconcilers;
// every node keeps serving the gateway.
package sandboxmatrix

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/sirupsen/logrus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"

	"github.com/xiaods/k8e/pkg/daemons/config"
)

// leaderElectionLeaseName is the Lease object name the reconcilers contend
// over. All k8e server nodes in an HA cluster share it (same namespace), so
// exactly one runs the warm-pool/GC/idle loops.
const leaderElectionLeaseName = "sandboxmatrix-controller"

// leaderElectionIdentity returns a stable per-node identity for the Lease
// holder: the configured identity if set, else the hostname.
func leaderElectionIdentity(cfg config.SandboxConfig) string {
	if cfg.LeaderElectionIdentity != "" {
		return cfg.LeaderElectionIdentity
	}
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "unknown"
}

// runLeaderGated runs reconcilers only on the elected leader. The run
// callback receives a context cancelled when leadership is lost, so a leader
// that stops renewing (crash, partition) stops reconciling promptly and the
// next candidate takes over. The callback is invoked on the elector's own
// goroutine and should spawn its (blocking) reconcilers concurrently; when it
// returns, leadership is still held and the reconcilers keep running until
// the passed context is cancelled. Blocks until ctx is cancelled.
func runLeaderGated(ctx context.Context, k8s kubernetes.Interface, cfg config.SandboxConfig, run func(leaderCtx context.Context)) error {
	lock := &resourcelock.LeaseLock{
		LeaseMeta: metav1.ObjectMeta{
			Name:      leaderElectionLeaseName,
			Namespace: cfg.Namespace,
		},
		Client: k8s.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{
			Identity: leaderElectionIdentity(cfg),
		},
	}

	leaderCtx, leaderCancel := context.WithCancel(ctx)
	defer leaderCancel()

	le, err := leaderelection.NewLeaderElector(leaderelection.LeaderElectionConfig{
		Lock:          lock,
		LeaseDuration: 15 * time.Second,
		RenewDeadline: 10 * time.Second,
		RetryPeriod:   2 * time.Second,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(leadingCtx context.Context) {
				logrus.Infof("sandbox-matrix: elected leader (%s), starting reconcilers",
					leaderElectionIdentity(cfg))
				// The elector passes its own context; ours is equivalent.
				run(leaderCtx)
			},
			OnStoppedLeading: func() {
				logrus.Warnf("sandbox-matrix: leadership lost (%s), reconcilers stopped; gateway keeps serving",
					leaderElectionIdentity(cfg))
				leaderCancel()
			},
			OnNewLeader: func(identity string) {
				if identity != leaderElectionIdentity(cfg) {
					logrus.Infof("sandbox-matrix: leader is %s, standing by", identity)
				}
			},
		},
		ReleaseOnCancel: true,
	})
	if err != nil {
		return fmt.Errorf("sandbox-matrix leader election: %w", err)
	}

	// Blocks until ctx is cancelled; RunOrDie only exists for the
	// fatal-on-error path, we prefer to return the error to the caller.
	le.Run(ctx)
	return nil
}
