// Package metrics defines the Prometheus series the node daemon and the
// regional tier export. Behavior per node and per tier lives here, never
// in custom resource status: at 10,000 nodes, status entries per node
// would make every Materialization a hot, megabyte-sized etcd object.
//
// Labels are bounded: tier, project, result, upstream kind. No series is
// labeled by object ID, pod, or node.
package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Registry holds every series of this process.
var Registry = prometheus.NewRegistry()

func init() {
	Registry.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
}

func counter(name, help string, labels ...string) *prometheus.CounterVec {
	c := prometheus.NewCounterVec(prometheus.CounterOpts{Name: name, Help: help}, labels)
	Registry.MustRegister(c)
	return c
}

func gauge(name, help string, labels ...string) *prometheus.GaugeVec {
	g := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: name, Help: help}, labels)
	Registry.MustRegister(g)
	return g
}

func histogram(name, help string, buckets []float64, labels ...string) *prometheus.HistogramVec {
	h := prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: name, Help: help, Buckets: buckets}, labels)
	Registry.MustRegister(h)
	return h
}

// Tier API (node daemons serving peers, and regional replicas).
var (
	TierRequests = counter("c4_tier_requests_total",
		"Requests answered by this tier, by result: hit, join (coalesced onto an in-flight fill), fill (started an upstream fetch), notfound, busy (shed with 503), denied, error.",
		"tier", "project", "result")
	TierServedBytes = counter("c4_tier_served_bytes_total", "Bytes sent to requesters.", "tier", "project")
	TierUpstream    = counter("c4_tier_upstream_requests_total",
		"Requests this process made to the next tier, by upstream kind (peer, regional, store) and result (ok, notfound, busy, error, short).",
		"tier", "upstream", "result")
	TierUpstreamBytes = counter("c4_tier_upstream_bytes_total", "Bytes received from the next tier.", "tier", "upstream")
	TierResumes       = counter("c4_tier_resumes_total", "Fills continued from a nonzero offset after an upstream failure.", "tier", "upstream")
	TierFills         = gauge("c4_tier_fills_inflight", "Upstream fills in progress.", "tier")
	TierWaiters       = gauge("c4_tier_waiters", "Requests waiting on in-flight fills.", "tier")
	TierIngest        = counter("c4_tier_ingest_total", "Objects written through PUT, by result (stored, present, mismatch, error).", "tier", "project", "result")
	TierIngestBytes   = counter("c4_tier_ingest_bytes_total", "Bytes received through PUT.", "tier", "project")
	TierAuth          = counter("c4_tier_auth_total", "Tier authentication decisions, by result (ok, cached, denied, invalid).", "tier", "result")
)

// Node daemon.
var (
	NodeObjects = counter("c4_node_object_requests_total",
		"Objects a node needed for publishes, by result: hit (cached), join (another publish was fetching it), fetch.",
		"project", "result")
	NodePublish = histogram("c4_node_publish_seconds",
		"NodePublishVolume latency, by outcome: warm (the stack existed), delta (one delta layer built), cold (a full layer built), failed.",
		[]float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300, 600},
		"project", "class", "outcome")
	NodeLayerBuilds = counter("c4_node_layer_builds_total", "Layers built, by kind (full, delta).", "project", "kind")
	NodeLayerFiles  = counter("c4_node_layer_entries_total", "Entries placed into layers (hardlinks, whiteouts, directories).", "kind")
	NodeLayers      = gauge("c4_node_layers", "Layers present on the node.", "kind")
	NodeMounts      = gauge("c4_node_mounts", "Volumes mounted on the node.", "project")
	NodeCapture     = counter("c4_node_capture_total", "Captures at unpublish, by result (changeset, empty, error).", "project", "result")
	NodeCaptureObj  = counter("c4_node_capture_objects_total", "Files captured, by kind (added, modified, deleted, reused).", "project", "kind")
	NodeCaptureB    = counter("c4_node_capture_bytes_total", "Bytes of new objects uploaded by capture.", "project")
	NodeCaptureSec  = histogram("c4_node_capture_seconds", "Capture duration.",
		[]float64{0.01, 0.05, 0.1, 0.5, 1, 5, 10, 30, 60, 300}, "project")
	NodeCacheBytes   = gauge("c4_node_cache_bytes", "Bytes held in the node object cache.")
	NodeEvictions    = counter("c4_node_evictions_total", "Evicted objects and layers.", "kind")
	NodeHashedBytes  = counter("c4_node_hashed_bytes_total", "Bytes hashed by the node, by reason (capture, verify-fill). The read path hashes nothing.", "reason")
	NodePrewarm      = counter("c4_node_prewarm_total", "Views pre-warmed for capacity requests.", "result")
	NodeTaskProgress = gauge("c4_task_progress", "Progress a running unit reports in .c4/progress (0 to 1).", "namespace", "pod")
)

// Handler serves the registry.
func Handler() http.Handler {
	return promhttp.HandlerFor(Registry, promhttp.HandlerOpts{Registry: Registry})
}
