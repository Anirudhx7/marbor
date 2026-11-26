package router

import "time"

// Stalled-generation tracking. A node can answer its health probe and still
// produce nothing for streaming requests. The proxy observes (never alters) a
// streaming response and reports here when no first byte has arrived by a
// deadline. This file only keeps that evidence; it never changes routing, so
// a stalled node stays eligible for placement and the signal is surface-only.
const (
	// StallThreshold is how many consecutive distinct requests must stall
	// before the node is reported stalled. One slow request proves little.
	StallThreshold = 3
	// StallExpiry is how long a stall record is trusted without a newer
	// observation. After this the node reads as unknown, never as stalled and
	// never as healthy-by-evidence.
	StallExpiry = 15 * time.Minute
	// StallFirstByteFloor is the smallest first-byte deadline. Prefill of a
	// long prompt can legitimately take tens of seconds.
	StallFirstByteFloor = 60 * time.Second
	// StallTTFTMultiple scales the node's slowest recent time-to-first-byte
	// into the deadline, so a node that is normally slow gets more room.
	StallTTFTMultiple = 4
	// StallPhaseFirstByte marks a stall where no output arrived at all.
	StallPhaseFirstByte = "first_byte"
)

// RecordStall notes that a streaming request to nodeName produced no first
// byte by its deadline. requestID identifies the request: the same request
// is counted once however many times it is reported, and an empty id is never
// counted. It reports whether the stall was counted. n.mu is taken once and
// nothing that locks n.mu again is called while holding it.
func (r *Router) RecordStall(nodeName, phase, requestID string) bool {
	if requestID == "" {
		return false
	}
	n := r.FindNode(nodeName)
	if n == nil {
		return false
	}
	now := time.Now()
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.lastStallRequestID == requestID {
		return false
	}
	if !n.LastStallAt.IsZero() && now.Sub(n.LastStallAt) > StallExpiry {
		n.ConsecutiveStalls = 0
	}
	n.ConsecutiveStalls++
	n.LastStallAt = now
	n.LastStallPhase = phase
	n.lastStallRequestID = requestID
	return true
}

// ClearStalls forgets the stall record for nodeName: a streaming request
// completed with output and never crossed its deadline.
func (r *Router) ClearStalls(nodeName string) {
	n := r.FindNode(nodeName)
	if n == nil {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	n.ConsecutiveStalls = 0
	n.LastStallAt = time.Time{}
	n.LastStallPhase = ""
	n.lastStallRequestID = ""
}

// IsStalled reports whether nodeName currently carries a live stalled verdict.
func (r *Router) IsStalled(nodeName string) bool {
	n := r.FindNode(nodeName)
	if n == nil {
		return false
	}
	n.mu.RLock()
	defer n.mu.RUnlock()
	stalled, _, _, _ := n.StallViewLocked(time.Now())
	return stalled
}

// StallViewLocked returns the live stall verdict for n. The caller must hold
// n.mu (read or write). A record older than StallExpiry, or one that was
// never made, returns the zero view: unknown, not a verdict either way.
func (n *NodeState) StallViewLocked(now time.Time) (stalled bool, consecutive int, lastAt time.Time, phase string) {
	if n.ConsecutiveStalls <= 0 || n.LastStallAt.IsZero() || now.Sub(n.LastStallAt) > StallExpiry {
		return false, 0, time.Time{}, ""
	}
	return n.ConsecutiveStalls >= StallThreshold, n.ConsecutiveStalls, n.LastStallAt, n.LastStallPhase
}

// FirstByteStallDeadline is how long a streaming request to n may go without
// any output before it counts as a stall: the larger of the floor and
// StallTTFTMultiple times the node's slowest recent first byte, widened by one
// share per other request already in flight on the node (a busy engine queues
// work before it starts), and never above the upstream timeout.
func (r *Router) FirstByteStallDeadline(n *NodeState, othersInFlight int) time.Duration {
	base := StallFirstByteFloor
	n.mu.RLock()
	var maxTTFT float64
	for _, v := range n.RecentTTFT {
		if v > maxTTFT {
			maxTTFT = v
		}
	}
	n.mu.RUnlock()
	if scaled := time.Duration(maxTTFT * float64(time.Second) * StallTTFTMultiple); scaled > base {
		base = scaled
	}
	if othersInFlight > 0 {
		base *= time.Duration(1 + othersInFlight)
	}
	if ceiling := r.UpstreamTimeout(); ceiling > 0 && base > ceiling {
		base = ceiling
	}
	return base
}

// ModelWarmOnNode reports whether model is already loaded on n. The proxy
// asks this once at dispatch, before any timer starts: a cold model load can
// legitimately emit nothing for minutes and must never read as a stall.
func (r *Router) ModelWarmOnNode(n *NodeState, model string) bool {
	return r.isModelWarm(n, model)
}
