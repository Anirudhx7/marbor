package main

// runner.go - executes one run: starts the arm's router (or dispatches
// directly for the prefix-only arm), drives the workload with a fixed number
// of concurrent clients, records every request, then gathers independent
// evidence of what really happened (engine counters, the router's own
// per-request routing explanation).

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type runPlan struct {
	Index    int          `json:"index"`
	Rep      int          `json:"rep"`
	Workload workloadSpec `json:"workload"`
	Arm      armSpec      `json:"arm"`
}

// reqRecord is the raw outcome of one request. Latencies are -1 when the
// quantity could not be observed (for example no token ever arrived).
type reqRecord struct {
	RunIndex     int     `json:"run"`
	Conv         int     `json:"conv"`
	Turn         int     `json:"turn"`
	StartMs      float64 `json:"start_ms"`
	TTFTMs       float64 `json:"ttft_ms"`
	TotalMs      float64 `json:"total_ms"`
	MeanITLMs    float64 `json:"mean_itl_ms"`
	Chunks       int     `json:"chunks"`
	Status       int     `json:"status"`
	Error        string  `json:"error,omitempty"`
	RequestID    string  `json:"request_id,omitempty"`
	Node         string  `json:"node,omitempty"`
	NodeSource   string  `json:"node_source,omitempty"`
	PromptTokens int     `json:"prompt_tokens"`
	CachedTokens int     `json:"cached_tokens"`
	Reason       string  `json:"reason,omitempty"`
	HasPrefix    bool    `json:"has_prefix_match"`
	PrefixRaw    float64 `json:"prefix_match_raw"`
	PrefixValue  float64 `json:"prefix_match_value"`
}

type runResult struct {
	Plan       runPlan          `json:"plan"`
	StartedAt  time.Time        `json:"started_at"`
	DurationMs float64          `json:"duration_ms"`
	Records    []reqRecord      `json:"-"`
	Before     []engineSnapshot `json:"before"`
	After      []engineSnapshot `json:"after"`
	Deltas     []engineDelta    `json:"engine_deltas"`
	Settings   json.RawMessage  `json:"settings,omitempty"`
	ExplainOK  int              `json:"explain_ok"`
	ExplainReq int              `json:"explain_requested"`
	RouterLog  string           `json:"-"`
}

type target struct {
	baseURL string
	apiKey  string
}

// sendChat issues one streaming chat request and measures it.
func sendChat(hc *http.Client, t target, model string, msgs []chatMessage, maxTokens int) reqRecord {
	rec := reqRecord{TTFTMs: -1, MeanITLMs: -1, PromptTokens: -1, CachedTokens: -1}
	body, _ := json.Marshal(map[string]any{
		"model": model, "messages": msgs, "stream": true, "max_tokens": maxTokens,
		"stream_options": map[string]any{"include_usage": true},
	})
	req, err := http.NewRequest(http.MethodPost, t.baseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		rec.Error = err.Error()
		return rec
	}
	req.Header.Set("Content-Type", "application/json")
	if t.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+t.apiKey)
	}
	t0 := time.Now()
	resp, err := hc.Do(req)
	if err != nil {
		rec.Error = "transport: " + err.Error()
		rec.TotalMs = ms(time.Since(t0))
		return rec
	}
	defer resp.Body.Close()
	rec.Status = resp.StatusCode
	rec.RequestID = resp.Header.Get("X-Request-Id")
	if h := resp.Header.Get("X-Node-Name"); h != "" {
		rec.Node, rec.NodeSource = h, "header"
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		rec.Error = fmt.Sprintf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
		rec.TotalMs = ms(time.Since(t0))
		return rec
	}
	var first, last time.Time
	rd := bufio.NewReaderSize(resp.Body, 64*1024)
	for {
		line, err := rd.ReadString('\n')
		if s := strings.TrimSpace(line); strings.HasPrefix(s, "data:") {
			payload := strings.TrimSpace(strings.TrimPrefix(s, "data:"))
			if payload != "[DONE]" && payload != "" {
				var ch struct {
					Choices []struct {
						Delta struct {
							Content string `json:"content"`
						} `json:"delta"`
					} `json:"choices"`
					Usage *struct {
						PromptTokens int `json:"prompt_tokens"`
						Details      *struct {
							Cached int `json:"cached_tokens"`
						} `json:"prompt_tokens_details"`
					} `json:"usage"`
				}
				if json.Unmarshal([]byte(payload), &ch) == nil {
					if len(ch.Choices) > 0 && ch.Choices[0].Delta.Content != "" {
						now := time.Now()
						if first.IsZero() {
							first = now
							rec.TTFTMs = ms(now.Sub(t0))
						}
						last = now
						rec.Chunks++
					}
					if ch.Usage != nil {
						rec.PromptTokens = ch.Usage.PromptTokens
						if ch.Usage.Details != nil {
							rec.CachedTokens = ch.Usage.Details.Cached
						}
					}
				}
			}
		}
		if err != nil {
			if err != io.EOF {
				rec.Error = "stream: " + err.Error()
			}
			break
		}
	}
	rec.TotalMs = ms(time.Since(t0))
	if rec.Chunks > 1 {
		rec.MeanITLMs = ms(last.Sub(first)) / float64(rec.Chunks-1)
	}
	if rec.Chunks == 0 && rec.Error == "" {
		rec.Error = "no content tokens received"
	}
	return rec
}

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

// executeRun runs one plan end to end. The router (if any) is stopped before
// returning, whatever the outcome.
func executeRun(ctx context.Context, e *experiment, p runPlan, outDir string) (*runResult, error) {
	hc := &http.Client{Timeout: e.timeout, Transport: &http.Transport{MaxIdleConnsPerHost: e.Concurrency + 4}}
	res := &runResult{Plan: p, StartedAt: time.Now()}
	probeClient := &http.Client{Timeout: 10 * time.Second}

	var sess *routerSession
	routerDir := filepath.Join(outDir, fmt.Sprintf("run%03d-router", p.Index))
	if p.Arm.Kind == "router" {
		var err error
		sess, err = startRouter(ctx, e, p.Arm, routerDir)
		if err != nil {
			return nil, fmt.Errorf("start router: %w", err)
		}
		defer sess.stop()
		res.Settings = sess.Settings
	}

	plans := buildWorkload(p.Workload, runSeed(e.Seed, p.Rep, p.Workload.Name), runTag(p.Index))
	res.Before = probeAll(probeClient, e.EngineKind, e.Engines)

	var mu sync.Mutex
	conv := make(chan convPlan)
	var wg sync.WaitGroup
	runStart := time.Now()
	for w := 0; w < e.Concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for cp := range conv {
				rng := rand.New(rand.NewSource(runSeed(e.Seed, p.Rep, fmt.Sprintf("%s|think|%d", p.Workload.Name, cp.ID))))
				for t, msgs := range cp.Turns {
					if ctx.Err() != nil {
						return
					}
					tg := target{}
					var node string
					if sess != nil {
						tg = target{sess.ProxyURL, sess.APIKey}
					} else {
						en := pickEngine(headKey(msgs), e.Engines)
						tg, node = target{baseURL: en.URL}, en.Name
					}
					started := time.Now()
					rec := sendChat(hc, tg, e.Model, msgs, e.MaxTokens)
					rec.RunIndex, rec.Conv, rec.Turn = p.Index, cp.ID, t+1
					rec.StartMs = ms(started.Sub(runStart))
					if node != "" {
						rec.Node, rec.NodeSource = node, "hash"
					}
					mu.Lock()
					res.Records = append(res.Records, rec)
					mu.Unlock()
					if t < len(cp.Turns)-1 && e.ThinkMs > 0 {
						time.Sleep(time.Duration(float64(e.ThinkMs) * (0.5 + rng.Float64()) * float64(time.Millisecond)))
					}
				}
			}
		}()
	}
	for _, cp := range plans {
		select {
		case conv <- cp:
		case <-ctx.Done():
		}
	}
	close(conv)
	wg.Wait()
	res.DurationMs = ms(time.Since(runStart))

	res.After = probeAll(probeClient, e.EngineKind, e.Engines)
	res.Deltas = diffSnapshots(res.Before, res.After)
	if sess != nil {
		resolveExplain(sess, res)
		res.RouterLog = tailFile(filepath.Join(routerDir, "router.log"), 4000)
	}
	return res, ctx.Err()
}

// resolveExplain asks the router which node it picked for each request and
// what the prefix term contributed. The router records decisions
// asynchronously, so a missing record is retried briefly.
func resolveExplain(sess *routerSession, res *runResult) {
	hc := &http.Client{Timeout: 10 * time.Second}
	time.Sleep(1500 * time.Millisecond)
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	var mu sync.Mutex
	for i := range res.Records {
		rec := &res.Records[i]
		if rec.RequestID == "" || rec.Status != http.StatusOK {
			continue
		}
		res.ExplainReq++
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			for try := 0; try < 8; try++ {
				code, data, err := sess.admin(hc, http.MethodGet, "/admin/requests/"+rec.RequestID+"/explain", nil)
				if err == nil && code == http.StatusOK {
					var d struct {
						Node       string `json:"node"`
						Reason     string `json:"reason"`
						Components []struct {
							Name  string  `json:"name"`
							Raw   float64 `json:"raw"`
							Value float64 `json:"value"`
						} `json:"components"`
					}
					if json.Unmarshal(data, &d) == nil {
						rec.Node, rec.NodeSource, rec.Reason = d.Node, "explain", d.Reason
						for _, c := range d.Components {
							if c.Name == "prefix_match" {
								rec.HasPrefix, rec.PrefixRaw, rec.PrefixValue = true, c.Raw, c.Value
							}
						}
						mu.Lock()
						res.ExplainOK++
						mu.Unlock()
						return
					}
				}
				time.Sleep(500 * time.Millisecond)
			}
		}()
	}
	wg.Wait()
}

func tailFile(path string, n int) string {
	b, err := readFileTail(path, n)
	if err != nil {
		return ""
	}
	return b
}
