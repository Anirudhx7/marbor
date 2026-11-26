package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"sync/atomic"
	"time"

	"github.com/Anirudhx7/marbor/internal/router"
)

// Stalled-generation watchdog. For a streaming request to a node whose model
// is already warm, a timer notes when no response byte has arrived by a
// deadline and tells the router, which keeps the evidence for the node list.
// It only observes: the timer never touches the response, never cancels the
// request and never reroutes it, so the stream stays a stream.

// stallWatch states. A watch ends in exactly one of done or stalled; the
// compare-and-swap makes whichever happens first win, so a timer that fires
// just after the request finished cannot report a stall for it.
const (
	stallWatchRunning int32 = iota
	stallWatchDone
	stallWatchStalled
)

type stallWatch struct {
	state atomic.Int32
	timer *time.Timer
}

// isStreamingRequest reports whether the client asked for a streamed answer.
// An explicit "stream" boolean in the body always wins. Without one, the
// Ollama-native chat and generate endpoints stream (the same default
// clientWantsStream applies on the cloud path) and every other endpoint does
// not. It reads the body marbor already buffered, so nothing new is read.
func isStreamingRequest(path string, body []byte) bool {
	if path == "/api/chat" || path == "/api/generate" {
		return clientWantsStream(body)
	}
	var b struct {
		Stream *bool `json:"stream"`
	}
	if json.Unmarshal(body, &b) != nil || b.Stream == nil {
		return false
	}
	return *b.Stream
}

// isHeaderTimeout reports whether err is the upstream response-header timeout
// (a network timeout from the transport), as opposed to a refused connection
// or a cancelled request.
func isHeaderTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout() && !errors.Is(err, context.Canceled)
}

// startStallWatch arms the first-byte timer for one request attempt, or
// returns nil when the attempt must not be judged: not a streaming request,
// or a model that was not already warm on the node when the request was
// dispatched (a cold load legitimately emits nothing for a long time). The
// warm check was made by the caller before this call; the timer callback
// never takes a router lock for it.
func (h *Handler) startStallWatch(ctx context.Context, rec *statusRecorder, node *router.NodeState, requestID string, deadline time.Duration) *stallWatch {
	w := &stallWatch{}
	w.timer = time.AfterFunc(deadline, func() {
		if ctx.Err() != nil || rec.sawAnyByte() {
			return
		}
		if !w.state.CompareAndSwap(stallWatchRunning, stallWatchStalled) {
			return
		}
		h.router.RecordStall(node.Name, router.StallPhaseFirstByte, requestID)
	})
	return w
}

// markStalled ends the watch as stalled and reports whether this call did it.
// The header-timeout path uses it so a request is counted once even when the
// timer fired first. Safe on a nil watch.
func (w *stallWatch) markStalled() bool {
	if w == nil {
		return false
	}
	w.timer.Stop()
	return w.state.CompareAndSwap(stallWatchRunning, stallWatchStalled)
}

// finish stops the timer and ends the watch as done. It reports true only when
// the watch never crossed its deadline, so the caller may clear the node's
// record after a clean stream. Safe on a nil watch.
func (w *stallWatch) finish() bool {
	if w == nil {
		return false
	}
	w.timer.Stop()
	return w.state.CompareAndSwap(stallWatchRunning, stallWatchDone)
}

// stallDeadline picks the first-byte deadline for an attempt on node.
func (h *Handler) stallDeadline(node *router.NodeState) time.Duration {
	others := int(atomic.LoadInt32(&node.ActiveConns)) - 1 // this request already holds a slot
	if others < 0 {
		others = 0
	}
	if h.stallDeadlineFor != nil {
		return h.stallDeadlineFor(node, others)
	}
	return h.router.FirstByteStallDeadline(node, others)
}
