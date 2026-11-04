package router

// queue_wait.go - the queued wait for a local node (WaitForNode and friends).

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/Anirudhx7/marbor/internal/metrics"
)

// WaitForNode is the queued variant of Route. It first tries Route() immediately;
// if no node is available it waits up to queueTimeout for one to free up (signaled
// by DecrConn). Returns nil after timeout or context cancellation, at which point
// the caller should fall through to cloud fallback or 503.
//
// runtimeFilter, when non-empty, restricts candidates to nodes whose Runtime
// matches exactly (e.g. "ollama"). Pass "" to allow any runtime.
//
// If the queue is already at queueMaxDepth, returns nil immediately without queuing
// to prevent unbounded memory growth under sustained overload.
func (r *Router) WaitForNode(ctx context.Context, modelName, sessionID, runtimeFilter string) (*NodeState, bool, *RoutingDecision) {
	// Fast path: immediate route.
	if node, warm, decision := r.Route(modelName, sessionID, runtimeFilter); node != nil {
		return node, warm, decision
	}
	res := r.waitForNodeInternal(ctx, modelName, sessionID, runtimeFilter, "", resolvedOpts{})
	return res.Node, res.Warm, res.Decision
}

// WaitForNodeWithPrefix is the prefix-locality-aware sibling of WaitForNode -
// see RouteWithPrefix. preferredNode, when non-empty, is a soft placement
// preference only (see the prefix_match ScoreComponent in placement.go);
// eligibility and every stronger optimization signal is unaffected. Pass
// preferredNode == "" for identical behavior to WaitForNode.
func (r *Router) WaitForNodeWithPrefix(ctx context.Context, modelName, sessionID, runtimeFilter, preferredNode string) (*NodeState, bool, *RoutingDecision) {
	res := r.WaitForNodeWithPrefixOpts(ctx, modelName, sessionID, runtimeFilter, preferredNode, WaitOpts{})
	return res.Node, res.Warm, res.Decision
}

// WaitOpts tunes one wait.
type WaitOpts struct {
	// CloudUsable reports whether a request that finds no local node can
	// really go to a cloud provider (a chain is configured, the key may use
	// it and the spend cap allows it). It is called at most once, and only
	// after the immediate route found no node, so requests that get a node
	// never pay for it. Only when it returns true may the wait end early:
	// hard classes skip the queue and the overflow SLA caps the wait. When
	// nil or false the wait lasts up to the queue timeout for every class.
	CloudUsable func() bool
}

// resolve evaluates CloudUsable once and fixes the answer for the wait.
func (o WaitOpts) resolve() resolvedOpts {
	return resolvedOpts{cloudUsable: o.CloudUsable != nil && o.CloudUsable()}
}

type resolvedOpts struct{ cloudUsable bool }

// Wait outcomes reported in WaitResult.Outcome.
const (
	WaitRouted        = "routed"         // a node was chosen
	WaitSkipHard      = "skip_hard"      // no local node can serve it now
	WaitSLAExpired    = "sla_expired"    // overflow SLA elapsed while local nodes were busy
	WaitQueueTimeout  = "queue_timeout"  // full queue timeout elapsed
	WaitQueueFull     = "queue_full"     // queue at its depth cap
	WaitQueueDisabled = "queue_disabled" // queue timeout or depth is zero
	WaitCtxCancelled  = "ctx_cancelled"  // the client went away
)

// WaitResult is the outcome of WaitForNodeWithPrefixOpts.
type WaitResult struct {
	Node *NodeState
	Warm bool
	// Decision is the routing decision of the successful route, or the last
	// no_candidate decision when no node was chosen (nil when none was made).
	Decision *RoutingDecision
	Outcome  string
	// NoRoute is the class of the last no_candidate decision, "" if unknown.
	NoRoute string
	// Waited is the real time spent inside the queue wait, 0 when the call
	// returned without queueing.
	Waited time.Duration
}

// WaitForNodeWithPrefixOpts is WaitForNodeWithPrefix with a typed result and
// the cloud-aware wait rules. It tries an immediate route first. On nil, when
// opts.CloudUsable and the no-route class is hard (no node serves the model,
// or every node that could is down or draining), it returns at once without
// claiming a queue slot. Otherwise it queues, waiting up to the overflow SLA
// when cloud is usable (never longer than the queue timeout), re-checking on
// every freed slot and every 500 ms, and re-reading the class each time so a
// node that dies while queued ends the wait early. One last route attempt is
// made when the timer fires so a slot freed at that instant is not lost.
func (r *Router) WaitForNodeWithPrefixOpts(ctx context.Context, modelName, sessionID, runtimeFilter, preferredNode string, opts WaitOpts) WaitResult {
	node, warm, decision := r.RouteWithPrefix(modelName, sessionID, runtimeFilter, preferredNode)
	if node != nil {
		return WaitResult{Node: node, Warm: warm, Decision: decision, Outcome: WaitRouted}
	}
	ro := opts.resolve()
	if nr := noRouteOf(decision); ro.cloudUsable && IsHardNoRoute(nr) {
		return WaitResult{Decision: decision, Outcome: WaitSkipHard, NoRoute: nr}
	}
	res := r.waitForNodeInternal(ctx, modelName, sessionID, runtimeFilter, preferredNode, ro)
	if res.Decision == nil {
		res.Decision = decision
		res.NoRoute = noRouteOf(decision)
	}
	return res
}

func noRouteOf(d *RoutingDecision) string {
	if d == nil {
		return ""
	}
	return d.NoRoute
}

// waitForNodeInternal is the shared queued-wait body, called only after the
// caller has already tried an immediate route and found nothing. preferredNode
// is threaded through to every retry route call so a queued request keeps
// benefiting from its locality hint on each wake-up.
func (r *Router) waitForNodeInternal(ctx context.Context, modelName, sessionID, runtimeFilter, preferredNode string, ro resolvedOpts) WaitResult {
	// Queue disabled (timeout or depth == 0): fall through immediately.
	// config.Validate() sets the production defaults; callers that bypass
	// Validate() (unit tests, zero-config New()) get no queue.
	queueTimeout := time.Duration(r.queueTimeoutMs.Load()) * time.Millisecond
	maxDepth := int(r.queueMaxDepth.Load())
	if queueTimeout <= 0 || maxDepth <= 0 {
		return WaitResult{Outcome: WaitQueueDisabled}
	}

	// Claim a queue slot atomically. Reject if already at capacity.
	depth := atomic.AddInt32(&r.queueDepth, 1)
	if int(depth) > maxDepth {
		atomic.AddInt32(&r.queueDepth, -1)
		return WaitResult{Outcome: WaitQueueFull}
	}
	// Publish via a fresh load, not the locally-captured `depth` return
	// value: two concurrent waiters' AddInt32-then-publish pairs can
	// interleave, so publishing a stale captured value would leave the
	// exported gauge showing something other than the true current depth.
	metrics.QueueDepth(float64(atomic.LoadInt32(&r.queueDepth)))
	defer func() {
		atomic.AddInt32(&r.queueDepth, -1)
		metrics.QueueDepth(float64(atomic.LoadInt32(&r.queueDepth)))
	}()

	// Overflow SLA: when cloud can take the request, the wait for a busy local
	// node ends after overflow_sla_ms instead of the longer queue timeout. An
	// SLA of 0 keeps the full queue timeout. This never changes which nodes
	// Route() considers, only how long a request waits for capacity.
	waitTimeout := queueTimeout
	slaCapped := false
	if sla := time.Duration(r.overflowSLAMs.Load()) * time.Millisecond; ro.cloudUsable && sla > 0 && sla < waitTimeout {
		waitTimeout = sla
		slaCapped = true
	}
	started := time.Now()
	timer := time.NewTimer(waitTimeout)
	defer timer.Stop()
	// Periodic safety-net retry: coalesced notifyCh signals can miss concurrent
	// DecrConn bursts (channel capacity 1). 500ms poll is the fallback safety net;
	// immediate wakeups are handled via the notifyCh channel.
	retryTick := time.NewTicker(500 * time.Millisecond)
	defer retryTick.Stop()

	var last *RoutingDecision
	// recheck routes again and reports whether the wait is over.
	recheck := func() (WaitResult, bool) {
		node, warm, decision := r.RouteWithPrefix(modelName, sessionID, runtimeFilter, preferredNode)
		if node != nil {
			return WaitResult{Node: node, Warm: warm, Decision: decision, Outcome: WaitRouted, Waited: time.Since(started)}, true
		}
		if decision != nil {
			last = decision
		}
		if nr := noRouteOf(decision); ro.cloudUsable && IsHardNoRoute(nr) {
			return WaitResult{Decision: decision, Outcome: WaitSkipHard, NoRoute: nr, Waited: time.Since(started)}, true
		}
		return WaitResult{}, false
	}

	for {
		r.notifyMu.Lock()
		ch := r.notifyCh
		r.notifyMu.Unlock()

		select {
		case <-ctx.Done():
			return WaitResult{Decision: last, Outcome: WaitCtxCancelled, NoRoute: noRouteOf(last), Waited: time.Since(started)}
		case <-timer.C:
			// One last look: a slot may have freed as the timer fired.
			if res, done := recheck(); done {
				return res
			}
			metrics.QueueTimeout()
			outcome := WaitQueueTimeout
			if slaCapped {
				outcome = WaitSLAExpired
			}
			return WaitResult{Decision: last, Outcome: outcome, NoRoute: noRouteOf(last), Waited: time.Since(started)}
		case <-ch:
			if res, done := recheck(); done {
				return res
			}
		case <-retryTick.C:
			if res, done := recheck(); done {
				return res
			}
		}
	}
}
