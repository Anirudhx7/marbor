package main

// kvsim.go - opt-in simulated inference engine behavior for the OpenAI-
// compatible mock, enabled with SIM_KV=1. It exists ONLY so a benchmark
// harness can be validated end to end without a GPU: it plants a known,
// documented effect (a per-node prefix cache that makes repeated prefixes
// cheaper, and a bounded number of concurrent slots that makes busy nodes
// slower) so the harness can be shown to detect that effect when it is there
// and to report nothing when it is not (SIM_CAPACITY_BLOCKS=0).
//
// Numbers produced by this simulation are never measurements of a real
// engine and must never be published or compared with real-hardware results.
//
// Model: a prompt is split into words ("tokens"); every SIM_BLOCK_WORDS words
// form a block whose identity chains the previous block's identity, so two
// prompts share cached blocks only for their common leading prefix, like a
// real paged prefix cache. Blocks live in a per-process LRU of
// SIM_CAPACITY_BLOCKS entries. A request waits for one of SIM_SLOTS slots,
// pays SIM_BASE_MS plus SIM_PREFILL_MS_PER_TOKEN for each uncached prompt
// token before its first token, then holds the slot for SIM_OUT_TOKENS decode
// steps of SIM_DECODE_MS_PER_TOKEN each. A client disconnect releases the slot.

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type kvSim struct {
	blockWords       int
	capBlocks        int
	baseMs           float64
	prefillMsPerTok  float64
	decodeMsPerTok   float64
	outTokens        int
	slots            chan struct{}
	mu               sync.Mutex
	lru              *list.List // front = most recent; values are string block ids
	index            map[string]*list.Element
	requests         atomic.Int64
	promptTokens     atomic.Int64
	cachedTokens     atomic.Int64
	running          atomic.Int64
	waiting          atomic.Int64
	peakWaiting      atomic.Int64
	aborted          atomic.Int64
	generatedTokens  atomic.Int64
	generatedOnAbort atomic.Int64
}

// newKVSimFromEnv returns nil unless SIM_KV=1.
func newKVSimFromEnv() *kvSim {
	if envOrDefault("SIM_KV", "") != "1" {
		return nil
	}
	return &kvSim{
		blockWords:      envInt("SIM_BLOCK_WORDS", 16),
		capBlocks:       envInt("SIM_CAPACITY_BLOCKS", 4096),
		baseMs:          float64(envInt("SIM_BASE_MS", 20)),
		prefillMsPerTok: float64(envInt("SIM_PREFILL_US_PER_TOKEN", 400)) / 1000.0,
		decodeMsPerTok:  float64(envInt("SIM_DECODE_MS_PER_TOKEN", 8)),
		outTokens:       envInt("SIM_OUT_TOKENS", 24),
		slots:           make(chan struct{}, envInt("SIM_SLOTS", 4)),
		lru:             list.New(),
		index:           make(map[string]*list.Element),
	}
}

func envInt(key string, def int) int {
	v, err := strconv.Atoi(envOrDefault(key, strconv.Itoa(def)))
	if err != nil || v < 0 {
		return def
	}
	return v
}

// blockIDs returns the chained identity of every FULL block of words.
func (k *kvSim) blockIDs(words []string) []string {
	n := len(words) / k.blockWords
	ids := make([]string, 0, n)
	prev := ""
	for i := 0; i < n; i++ {
		h := sha256.New()
		h.Write([]byte(prev))
		h.Write([]byte(strings.Join(words[i*k.blockWords:(i+1)*k.blockWords], " ")))
		prev = hex.EncodeToString(h.Sum(nil))[:16]
		ids = append(ids, prev)
	}
	return ids
}

// lookupAndStore returns how many leading blocks were cached, then stores all
// of the prompt's blocks (refreshing recency), evicting the oldest on overflow.
func (k *kvSim) lookupAndStore(ids []string) int {
	k.mu.Lock()
	defer k.mu.Unlock()
	cached := 0
	for _, id := range ids {
		if _, ok := k.index[id]; !ok {
			break
		}
		cached++
	}
	if k.capBlocks == 0 {
		return 0
	}
	for _, id := range ids {
		if el, ok := k.index[id]; ok {
			k.lru.MoveToFront(el)
			continue
		}
		k.index[id] = k.lru.PushFront(id)
	}
	for k.lru.Len() > k.capBlocks {
		last := k.lru.Back()
		k.lru.Remove(last)
		delete(k.index, last.Value.(string))
	}
	return cached
}

// simRun is one admitted request.
type simRun struct {
	k            *kvSim
	promptTokens int
	cachedTokens int
	outTokens    int
	released     bool
	generated    int
}

// begin waits for a slot (or the client going away), then pays the prefill
// delay for the uncached part of the prompt. It returns nil when ctx ended
// first, in which case the caller must stop without writing a response.
func (k *kvSim) begin(ctx context.Context, prompt string) *simRun {
	words := strings.Fields(prompt)
	k.requests.Add(1)
	w := k.waiting.Add(1)
	for {
		p := k.peakWaiting.Load()
		if w <= p || k.peakWaiting.CompareAndSwap(p, w) {
			break
		}
	}
	select {
	case k.slots <- struct{}{}:
		k.waiting.Add(-1)
	case <-ctx.Done():
		k.waiting.Add(-1)
		k.aborted.Add(1)
		return nil
	}
	k.running.Add(1)
	cached := k.lookupAndStore(k.blockIDs(words)) * k.blockWords
	if cached > len(words) {
		cached = len(words)
	}
	k.promptTokens.Add(int64(len(words)))
	k.cachedTokens.Add(int64(cached))
	prefill := time.Duration((k.baseMs + float64(len(words)-cached)*k.prefillMsPerTok) * float64(time.Millisecond))
	select {
	case <-time.After(prefill):
	case <-ctx.Done():
		k.finishSlot()
		k.aborted.Add(1)
		return nil
	}
	return &simRun{k: k, promptTokens: len(words), cachedTokens: cached, outTokens: k.outTokens}
}

func (k *kvSim) finishSlot() {
	k.running.Add(-1)
	<-k.slots
}

// decodeStep sleeps one decode step and counts the generated token. It returns
// false when the client has gone away, so the caller stops generating the way
// an engine that honors aborts would.
func (r *simRun) decodeStep(ctx context.Context) bool {
	select {
	case <-time.After(time.Duration(r.k.decodeMsPerTok * float64(time.Millisecond))):
		r.generated++
		r.k.generatedTokens.Add(1)
		return true
	case <-ctx.Done():
		return false
	}
}

// finish releases the slot exactly once.
func (r *simRun) finish(ctx context.Context) {
	if r.released {
		return
	}
	r.released = true
	if ctx.Err() != nil {
		r.k.aborted.Add(1)
		r.k.generatedOnAbort.Add(int64(r.generated))
	}
	r.k.finishSlot()
}

// statsHandler serves GET /sim/stats: ground truth the harness compares with
// what the router claims it did. Real engines expose the equivalent through
// their own metrics; the harness has a separate probe for those.
func (k *kvSim) statsHandler(nodeName string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		k.mu.Lock()
		blocks := k.lru.Len()
		k.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"node":               nodeName,
			"requests":           k.requests.Load(),
			"prompt_tokens":      k.promptTokens.Load(),
			"cached_tokens":      k.cachedTokens.Load(),
			"running":            k.running.Load(),
			"waiting":            k.waiting.Load(),
			"peak_waiting":       k.peakWaiting.Load(),
			"aborted":            k.aborted.Load(),
			"generated_tokens":   k.generatedTokens.Load(),
			"generated_on_abort": k.generatedOnAbort.Load(),
			"cache_blocks":       blocks,
			"cache_capacity":     k.capBlocks,
		})
	}
}
