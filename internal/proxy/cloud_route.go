package proxy

import (
	"net/http"
	"strings"
	"time"

	"github.com/Anirudhx7/marbor/internal/admin"
	"github.com/Anirudhx7/marbor/internal/audit"
	"github.com/Anirudhx7/marbor/internal/router"
)

// Reason codes recorded for a request that went to cloud (or was refused
// because its key is local_only). They are a fixed set so the request log,
// the Admin API, the CLI, the UI and the metric label all agree.
const (
	CloudReasonNoNodeServesModel    = "no_node_serves_model"
	CloudReasonAllNodesUnavailable  = "all_nodes_unavailable"
	CloudReasonSaturatedSLAExpired  = "saturated_sla_expired"
	CloudReasonQueueTimeout         = "queue_timeout_cloud"
	CloudReasonQueueFull            = "queue_full_cloud"
	CloudReasonLocalUpstreamFailed  = "local_upstream_failed"
	CloudReasonLocalOnlyBlocked     = "local_only_blocked"
	CloudReasonCloudOnlyPolicy      = "cloud_only_policy"
	cloudReasonUnclassifiedFallback = CloudReasonQueueTimeout
)

// cloudRoute carries why a request is going to cloud and the measured time it
// spent waiting for a local node, so the cloud path can record both without
// guessing. cloudStart is when the cloud phase began, including any failover
// between providers.
type cloudRoute struct {
	reason  string
	noRoute string
	// waited is the real time the request spent in the local queue wait before
	// this route was decided. It is 0 when the request skipped the queue. For
	// local_upstream_failed it is the wait that preceded the local attempt that
	// then failed, not time spent before cloud.
	waited     time.Duration
	base       *router.RoutingDecision
	cloudStart time.Time
}

// isCloudOnly is true when the request named a model on the cloud-only list.
// Such a request is intended cloud traffic, not overflow, so it is kept out of
// the per-key spill counts, and a cloud failure is explained as never retried
// locally.
func (c *cloudRoute) isCloudOnly() bool {
	return c.reason == CloudReasonCloudOnlyPolicy
}

// newCloudOnlyRoute builds the route record for a request whose model is on
// the cloud-only list. Nothing waited and no local filter ran, so the wait is
// a real zero and there is no exclusion list.
func newCloudOnlyRoute() *cloudRoute {
	return &cloudRoute{reason: CloudReasonCloudOnlyPolicy, cloudStart: time.Now()}
}

// newCloudRoute builds the route record for a request that waitRes could not
// place locally. retryFailure is true when a local node was chosen and then
// failed, rather than none being found.
func newCloudRoute(res router.WaitResult, retryFailure bool) *cloudRoute {
	c := &cloudRoute{noRoute: res.NoRoute, waited: res.Waited, base: res.Decision, cloudStart: time.Now()}
	if retryFailure {
		// The decision belongs to the local node that failed, so its exclusion
		// list would be stale for the cloud route.
		c.base = nil
	}
	switch {
	case retryFailure:
		c.reason = CloudReasonLocalUpstreamFailed
	case res.Outcome == router.WaitSkipHard && res.NoRoute == router.NoRouteNoNodeServesModel:
		c.reason = CloudReasonNoNodeServesModel
	case res.Outcome == router.WaitSkipHard:
		c.reason = CloudReasonAllNodesUnavailable
	case res.Outcome == router.WaitSLAExpired:
		c.reason = CloudReasonSaturatedSLAExpired
	case res.Outcome == router.WaitQueueFull || res.Outcome == router.WaitQueueDisabled:
		c.reason = CloudReasonQueueFull
	case res.Outcome == router.WaitQueueTimeout:
		c.reason = CloudReasonQueueTimeout
	default:
		// Any other outcome (a local node that failed, a cancelled client) is not a
		// queue timeout; fall back to the generic label.
		c.reason = cloudReasonUnclassifiedFallback
	}
	return c
}

// sentence is one plain-language line for the operator. It names no URL, key
// or provider endpoint.
func (c *cloudRoute) sentence() string {
	switch c.reason {
	case CloudReasonNoNodeServesModel:
		return "No local node serves this model, so it went to cloud without queueing."
	case CloudReasonAllNodesUnavailable:
		return "Every local node that could serve this model was down or draining, so it went to cloud without queueing."
	case CloudReasonSaturatedSLAExpired:
		return "Local nodes were busy for longer than the overflow SLA, so it went to cloud."
	case CloudReasonQueueFull:
		return "The local queue was full or disabled, so it went to cloud."
	case CloudReasonLocalUpstreamFailed:
		return "The local node failed this request, so it went to cloud."
	case CloudReasonCloudOnlyPolicy:
		return "This model is on the cloud-only list, so local routing was skipped."
	case CloudReasonLocalOnlyBlocked:
		return "This key is local_only and no local node could serve the request, so it was refused and never sent to cloud."
	default:
		return "No local node became free within the queue timeout, so it went to cloud."
	}
}

// decision is the routing decision recorded for the request. Node is the
// provider the request went to ("none" when it was refused).
func (c *cloudRoute) decision(node string) *router.RoutingDecision {
	d := &router.RoutingDecision{Node: node, Reason: c.reason, Detail: c.sentence(), NoRoute: c.noRoute}
	if c.base != nil {
		d.Excluded = c.base.Excluded
		d.ExcludedTotal = c.base.ExcludedTotal
	}
	return d
}

// split returns the measured queue wait and cloud time in milliseconds, both
// bounded so that wait + cloud never exceeds the request total. All three
// durations come from one set of clock reads taken by the caller.
func (c *cloudRoute) split(total, cloud time.Duration) (queueWait, cloudTime int64) {
	totalMs := total.Milliseconds()
	queueWait = min(c.waited.Milliseconds(), totalMs)
	cloudTime = max(min(cloud.Milliseconds(), totalMs-queueWait), 0)
	return queueWait, cloudTime
}

// logBlocked records the 503 a local_only key gets when no local node can
// take its request. The row has no provider (node "none"), carries the
// local_only_blocked reason and the real queue wait, and no cloud time.
func (h *Handler) logBlocked(r *http.Request, keyName, modelName, requestID string, start time.Time, res router.WaitResult) {
	route := newCloudRoute(res, false)
	route.reason = CloudReasonLocalOnlyBlocked
	total := time.Since(start)
	waitMs, _ := route.split(total, 0)
	if h.admin != nil {
		h.admin.LogRequestTimed(requestID, keyName, h.clientIP(r), modelName, "none", "error",
			http.StatusServiceUnavailable, int(total.Milliseconds()), 0, 0,
			route.decision("none"), admin.RequestTiming{QueueWaitMs: &waitMs})
	}
	if h.audit != nil {
		h.audit.Log(audit.Entry{
			Time:          time.Now(),
			RequestID:     requestID,
			KeyName:       keyName,
			Model:         modelName,
			Node:          "none",
			Status:        "503",
			LatencyMs:     int(total.Milliseconds()),
			RoutingReason: route.reason,
			QueueWaitMs:   &waitMs,
		})
	}
}

// clientIP is the caller's address, honouring X-Forwarded-For / X-Real-IP
// only when the operator opted in with trust_proxy_headers.
func (h *Handler) clientIP(r *http.Request) string {
	ip := r.RemoteAddr
	if h.trustProxyHeaders {
		if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
			if parts := strings.SplitN(fwd, ",", 2); len(parts) > 0 {
				return strings.TrimSpace(parts[0])
			}
		} else if fwd2 := r.Header.Get("X-Real-IP"); fwd2 != "" {
			return fwd2
		}
	}
	return ip
}
