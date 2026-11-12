package proxy

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Anirudhx7/marbor/internal/admin"
	"github.com/Anirudhx7/marbor/internal/audit"
	"github.com/Anirudhx7/marbor/internal/metrics"
)

// Suffixes added to a cloud failure so the operator can see that a local
// fallback was deliberately skipped for a cloud-only model.
const (
	cloudOnlyBudgetSuffix  = " (the model is cloud-only and is not run locally)"
	cloudOnlyFailureSuffix = " (the model is cloud-only and was not retried locally)"
)

// isCloudOnlyExemptPath reports whether path is a read-only path that names a
// model but runs none: a listed model on one of these keeps today's local
// routing, and is never proxied to the paid cloud provider. Trailing slashes
// are ignored, the same way the management guard does.
func isCloudOnlyExemptPath(path string) bool {
	switch policyPath(path) {
	case "/api/show", "/api/ps", "/api/tags", "/v1/models":
		return true
	}
	return false
}

// isCloudCapablePath reports whether a cloud provider can serve path. A
// non-exempt request for a cloud-only model on any other path has no cloud
// translation, so it is refused rather than run on a local node.
func isCloudCapablePath(path string) bool {
	switch policyPath(path) {
	case "/api/chat", "/api/generate", "/api/embed", "/api/embeddings",
		"/v1/chat/completions", "/v1/completions", "/v1/embeddings":
		return true
	}
	return false
}

// policyPath is the form of a request path the cloud-only path checks compare
// against: trailing slashes ignored.
func policyPath(path string) string {
	return strings.TrimRight(path, "/")
}

// cloudOnlyReq groups what serveCloudOnly and its refusals need about the
// request, so the several same-typed strings cannot be passed in the wrong
// order.
type cloudOnlyReq struct {
	body            []byte
	modelName       string // resolved name, used for metrics and the cloud call
	clientModelName string // the name the client sent, used in messages and logs
	aliased         bool
	keyName         string
	requestID       string
	start           time.Time
	label           func(string) string
}

// serveCloudOnly handles a request that names a cloud-only model on a
// non-exempt path. It always writes the response: the request goes to a cloud
// provider, or is refused with a 503 and never runs on a local node. Checks
// run cheapest first, with the spend-budget store query last.
func (h *Handler) serveCloudOnly(w http.ResponseWriter, r *http.Request, q cloudOnlyReq) {
	// A key that may not leave local nodes cannot have a cloud-only model: the
	// two policies contradict, and the request is refused before any cloud
	// provider is even looked up.
	if h.auth != nil && h.auth.IsLocalOnly(q.keyName) {
		msg := fmt.Sprintf("model %q is on the cloud-only list and this key is local_only; the request was not sent to any cloud provider or local node", q.clientModelName)
		writeAPIError(w, http.StatusServiceUnavailable, msg, "server_error", CloudReasonLocalOnlyBlocked)
		metrics.RequestsTotal(q.keyName, q.label(q.modelName), "none", "503")
		metrics.CloudOverflowReason(CloudReasonLocalOnlyBlocked)
		h.logCloudOnlyRefusal(r, q.keyName, q.clientModelName, q.requestID, q.start, CloudReasonLocalOnlyBlocked, "Refused: the model is on the cloud-only list and this key is local_only.")
		return
	}
	// A path no cloud provider can serve (and that is not a read-only exempt
	// path) must not fall through to a local node either.
	if !isCloudCapablePath(r.URL.Path) {
		msg := fmt.Sprintf("model %q is on the cloud-only list and this endpoint cannot be sent to a cloud provider; the request was not run on a local node", q.clientModelName)
		h.refuseCloudOnly(w, r, q, msg, "cloud_only_unsupported_path")
		return
	}
	clouds := h.router.CloudChain()
	if len(clouds) == 0 {
		msg := fmt.Sprintf("model %q is on the cloud-only list and no cloud provider is enabled; enable one on the Routing page or remove the model from the list", q.clientModelName)
		h.refuseCloudOnly(w, r, q, msg, "cloud_only_no_provider")
		return
	}
	if h.admin != nil {
		if exceeded, reason := h.admin.CloudBudgetExceeded(q.keyName); exceeded {
			h.refuseCloudOnly(w, r, q, reason+cloudOnlyBudgetSuffix, "cloud_budget_exceeded")
			return
		}
	}
	cloudBody, cloudModel := cloudRequestFor(q.body, q.modelName, q.clientModelName, q.aliased)
	h.proxyToCloud(w, r, cloudBody, cloudModel, q.keyName, q.requestID, q.start, clouds, 0, newCloudOnlyRoute())
}

// refuseCloudOnlyUnreadable refuses a request while the stored cloud-only
// models list is unreadable: which models are listed is unknown, so the
// request is neither run locally nor sent to cloud.
func (h *Handler) refuseCloudOnlyUnreadable(w http.ResponseWriter, r *http.Request, q cloudOnlyReq) {
	msg := fmt.Sprintf("model %q cannot be routed right now because the cloud-only models policy could not be loaded; the request was not run on a local node or sent to a cloud provider", q.clientModelName)
	h.refuseCloudOnly(w, r, q, msg, "cloud_only_list_unreadable")
}

// refuseCloudOnly writes the 503 for a cloud-only request that cannot be sent
// to cloud, with its metric and request-log row (reason cloud_only_policy, no
// node, no cloud time).
func (h *Handler) refuseCloudOnly(w http.ResponseWriter, r *http.Request, q cloudOnlyReq, msg, code string) {
	writeAPIError(w, http.StatusServiceUnavailable, msg, "server_error", code)
	metrics.RequestsTotal(q.keyName, q.label(q.modelName), "none", "503")
	metrics.CloudOverflowReason(CloudReasonCloudOnlyPolicy)
	h.logCloudOnlyRefusal(r, q.keyName, q.clientModelName, q.requestID, q.start, CloudReasonCloudOnlyPolicy, msg)
}

// logCloudOnlyRefusal records the request-log and audit rows for a refused
// cloud-only request: no provider (node "none"), a measured total, a real zero
// queue wait and no cloud time, so the row shows "-" for what never happened.
func (h *Handler) logCloudOnlyRefusal(r *http.Request, keyName, modelName, requestID string, start time.Time, reason, detail string) {
	route := newCloudOnlyRoute()
	route.reason = reason
	total := time.Since(start)
	waitMs := int64(0)
	if h.admin != nil {
		d := route.decision("none")
		d.Detail = detail
		h.admin.LogRequestTimed(requestID, keyName, h.clientIP(r), modelName, "none", "error",
			http.StatusServiceUnavailable, int(total.Milliseconds()), 0, 0,
			d, admin.RequestTiming{QueueWaitMs: &waitMs})
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
			RoutingReason: reason,
			QueueWaitMs:   &waitMs,
		})
	}
}
