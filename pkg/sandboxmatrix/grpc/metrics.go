package grpc

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/xiaods/k8e/pkg/metrics"
)

// Sandbox metrics (KIP-16 M5 / issue #513). These are registered against the
// shared k8e legacy registry so they surface on the existing /metrics endpoint
// (pkg/metrics) without a new process or listener. Values are read from the
// orchestrator's atomic counters at scrape time — no extra work in the request
// hot path (mirrors ephemeral-sandbox's activity-gated observability).
type sandboxMetricsCollector struct {
	orch *Orchestrator
}

var (
	// metric descriptors are static; the collector reports values lazily.
	sandboxWarmClaimsDesc = prometheus.NewDesc(
		"k8e_sandbox_warm_claims_total",
		"Total sessions claimed from the warm pool.",
		nil, nil,
	)
	sandboxColdStartsDesc = prometheus.NewDesc(
		"k8e_sandbox_cold_starts_total",
		"Total sessions started from a cold pod (no warm claim).",
		nil, nil,
	)
	sandboxAvgClaimLatencyMsDesc = prometheus.NewDesc(
		"k8e_sandbox_claim_latency_ms_average",
		"Average pod acquisition latency in milliseconds (warm + cold).",
		nil, nil,
	)
	sandboxBackgroundRunsDesc = prometheus.NewDesc(
		"k8e_sandbox_background_runs",
		"Currently registered background runs.",
		nil, nil,
	)
)

// NewSandboxMetricsCollector returns a Prometheus Collector bound to the
// orchestrator's live counters. Register with a registry to expose metrics.
func NewSandboxMetricsCollector(orch *Orchestrator) prometheus.Collector {
	return &sandboxMetricsCollector{orch: orch}
}

// Describe implements prometheus.Collector.
func (c *sandboxMetricsCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- sandboxWarmClaimsDesc
	ch <- sandboxColdStartsDesc
	ch <- sandboxAvgClaimLatencyMsDesc
	ch <- sandboxBackgroundRunsDesc
}

// Collect implements prometheus.Collector — reads atomics at scrape time.
func (c *sandboxMetricsCollector) Collect(ch chan<- prometheus.Metric) {
	warm, cold, avgMs := c.orch.Metrics()
	ch <- prometheus.MustNewConstMetric(sandboxWarmClaimsDesc, prometheus.CounterValue, float64(warm))
	ch <- prometheus.MustNewConstMetric(sandboxColdStartsDesc, prometheus.CounterValue, float64(cold))
	ch <- prometheus.MustNewConstMetric(sandboxAvgClaimLatencyMsDesc, prometheus.GaugeValue, float64(avgMs))
	ch <- prometheus.MustNewConstMetric(sandboxBackgroundRunsDesc, prometheus.GaugeValue, float64(c.orch.countAllBackgroundRuns()))
}

// RegisterSandboxMetrics adds the sandbox collectors to the shared k8e metrics
// registry (idempotent-safe: a second registration is a no-op on error).
func RegisterSandboxMetrics(orch *Orchestrator) {
	if err := metrics.DefaultRegisterer.Register(NewSandboxMetricsCollector(orch)); err != nil {
		// Already-registered collectors are expected on re-initialization;
		// other errors (name collision) are surfaced through the registry.
		return
	}
}
