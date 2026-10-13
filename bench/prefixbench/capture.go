package main

// capture.go - saves raw engine responses so the usage and cache fields a
// real engine returns can be inspected before anything is built on them. Run
// it once against the real runtime on the GPU host: it sends one non-stream
// request, one stream request without usage options and one with
// stream_options.include_usage, then stores each raw body together with the
// engine's metrics page.

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

func cmdCapture(args []string) error {
	fs := flag.NewFlagSet("capture", flag.ContinueOnError)
	url := fs.String("url", "", "engine base URL, e.g. http://127.0.0.1:8000 (required)")
	model := fs.String("model", "", "model id (required)")
	out := fs.String("out", "", "output directory (required)")
	metrics := fs.String("metrics-path", "/metrics", "metrics path to snapshot before and after")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *url == "" || *model == "" || *out == "" {
		return fmt.Errorf("capture needs --url, --model and --out")
	}
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}
	hc := &http.Client{Timeout: 120 * time.Second}
	msgs := []chatMessage{{Role: "user", Content: "Say the word ready."}}

	save := func(name string, data []byte) error {
		return os.WriteFile(filepath.Join(*out, name), data, 0o644)
	}
	snapshot := func(name string) {
		if b, err := fetchBytes(hc, *url+*metrics); err == nil || len(b) > 0 {
			_ = save(name, b)
		}
	}
	post := func(name string, body map[string]any) error {
		body["model"], body["messages"], body["max_tokens"] = *model, msgs, 16
		raw, _ := json.Marshal(body)
		resp, err := hc.Post(*url+"/v1/chat/completions", "application/json", bytes.NewReader(raw))
		if err != nil {
			return save(name+".error.txt", []byte(err.Error()))
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		hdr, _ := json.MarshalIndent(resp.Header, "", "  ")
		if err := save(name+".headers.json", hdr); err != nil {
			return err
		}
		return save(fmt.Sprintf("%s.status%d.txt", name, resp.StatusCode), data)
	}

	snapshot("metrics.before.txt")
	cases := []struct {
		name string
		body map[string]any
	}{
		{"nonstream", map[string]any{"stream": false}},
		{"stream-plain", map[string]any{"stream": true}},
		{"stream-usage", map[string]any{"stream": true, "stream_options": map[string]any{"include_usage": true}}},
		// A repeated request makes a prefix-cache hit visible in usage and metrics.
		{"nonstream-repeat", map[string]any{"stream": false}},
	}
	for _, c := range cases {
		if err := post(c.name, c.body); err != nil {
			return err
		}
		fmt.Println("captured", c.name)
	}
	snapshot("metrics.after.txt")
	fmt.Println("saved to", *out)
	return nil
}
