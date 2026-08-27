package proxy

// cloudtranslate.go -- translates OpenAI-format cloud responses back into
// Ollama NDJSON format when the original client request came in on an Ollama-
// native path (/api/chat or /api/generate).
//
// Only activated when the original path starts with "/api/". Requests that
// already used /v1/... are passed through unchanged (R2 preserved: no
// buffering in that path).
//
// Translation is done via a custom http.RoundTripper that wraps the real
// transport. The wrapper intercepts the *http.Response before httputil copies
// it to the client, replaces the Body with a translated reader, and adjusts
// Content-Type so the client sees clean NDJSON (or plain JSON for non-
// streaming).

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// isOllamaPath reports whether path is an Ollama-native API path.
// Any path under /api/ is Ollama-native; /v1/ paths are OpenAI-compat and
// can reach any backend runtime. Cloud fallback for /api/ paths still works
// because translateCloudPath handles the full /api/ space.
func isOllamaPath(path string) bool {
	return strings.HasPrefix(path, "/api/")
}

// isTranslatedOllamaPath reports whether path is one of the four Ollama
// endpoints this file actually translates (chat/generate/embeddings/embed
// response bodies). isOllamaPath alone is too broad: it matches every
// /api/* path, including passthrough endpoints like /api/tags whose
// response body is never rewritten - gating RoundTrip's Content-Type/
// Content-Length rewrite on this instead means an untranslated passthrough
// response keeps its real Content-Type/Content-Length rather than being
// mislabeled application/x-ndjson.
func isTranslatedOllamaPath(path string) bool {
	switch path {
	case "/api/chat", "/api/generate", "/api/embeddings", "/api/embed":
		return true
	default:
		return false
	}
}

// translatingTransport wraps an inner RoundTripper and, for responses to
// Ollama-native requests, replaces the response body with an Ollama NDJSON
// stream translated from the OpenAI SSE (or JSON) response.
type translatingTransport struct {
	inner        http.RoundTripper
	origPath     string // the client's original request path (e.g. /api/chat)
	clientModel  string // model name the client asked for (echoed back in NDJSON)
	clientStream bool   // whether the client requested a streamed response
}

// clientWantsStream reports the Ollama request's stream preference. Ollama
// defaults to streaming when the field is absent, so a missing/!bool value is
// treated as true.
func clientWantsStream(body []byte) bool {
	var b struct {
		Stream *bool `json:"stream"`
	}
	if err := json.Unmarshal(body, &b); err != nil || b.Stream == nil {
		return true
	}
	return *b.Stream
}

func (t *translatingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.inner.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if !isTranslatedOllamaPath(t.origPath) {
		return resp, nil
	}

	ct := resp.Header.Get("Content-Type")
	isSSE := strings.Contains(ct, "text/event-stream")

	// Non-streaming client: Ollama returns a SINGLE JSON object for stream:false,
	// not NDJSON. Emit one object and keep application/json so the client parses
	// it correctly. (Streaming is the common path and is handled below.)
	if !t.clientStream && !isSSE {
		resp.Body = translateJSONToSingleOllama(resp.Body, t.origPath, t.clientModel)
		resp.Header.Set("Content-Type", "application/json")
		resp.ContentLength = -1
		resp.Header.Del("Content-Length")
		return resp, nil
	}

	// Detect streaming from Content-Type. If the cloud did not send SSE but
	// we expected it (body is event-stream), fall through to the JSON path.
	var newBody io.ReadCloser
	if isSSE {
		newBody = translateSSEToNDJSON(resp.Body, t.origPath, t.clientModel)
	} else {
		newBody = translateJSONToNDJSON(resp.Body, t.origPath, t.clientModel)
	}

	resp.Body = newBody
	resp.Header.Set("Content-Type", "application/x-ndjson")
	// Content-Length is unknown after translation - remove it so the client
	// does not truncate the translated stream.
	resp.ContentLength = -1
	resp.Header.Del("Content-Length")
	return resp, nil
}

// ------------------------------------------------------------------
// SSE -> NDJSON translation (streaming path)
// ------------------------------------------------------------------

// translateSSEToNDJSON returns a ReadCloser that reads the OpenAI SSE stream
// from src and emits Ollama NDJSON lines. Each content delta becomes one
// {"model":...,"message":{"role":"assistant","content":"..."},"done":false}
// line (for /api/chat) or {"model":...,"response":"...","done":false} (for
// /api/generate). A final {"done":true,...} line is appended with usage when
// available. The pipe is unbuffered on the write side: each line is written
// and the scanner advances only when the reader consumes it, so R2 (no
// buffering) is preserved.
func translateSSEToNDJSON(src io.ReadCloser, origPath, clientModel string) io.ReadCloser {
	pr, pw := io.Pipe()

	go func() {
		defer src.Close()
		defer pw.Close()

		scanner := bufio.NewScanner(src)
		// Default Scanner caps a line at 64KB; a large SSE data: frame (big delta
		// or a usage/reasoning block) would exceed it and silently truncate the
		// translated stream. Allow up to 4MB per line.
		scanner.Buffer(make([]byte, 0, 64*1024), 4<<20)

		var (
			completionTokens int64
			promptTokens     int64
			sawDone          bool
		)

		for scanner.Scan() {
			raw := scanner.Text()

			// SSE lines look like "data: {...}" or "data: [DONE]".
			if !strings.HasPrefix(raw, "data: ") {
				continue
			}
			payload := strings.TrimPrefix(raw, "data: ")
			if payload == "[DONE]" {
				sawDone = true
				break
			}

			// Parse the OpenAI chunk.
			var chunk struct {
				Choices []struct {
					Delta struct {
						Content string `json:"content"`
					} `json:"delta"`
				} `json:"choices"`
				Usage *struct {
					CompletionTokens int64 `json:"completion_tokens"`
					PromptTokens     int64 `json:"prompt_tokens"`
					TotalTokens      int64 `json:"total_tokens"`
				} `json:"usage"`
			}
			if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
				continue
			}

			// Capture usage when present (some providers send it on every chunk,
			// others only on the last one).
			if chunk.Usage != nil {
				if chunk.Usage.CompletionTokens > 0 {
					completionTokens = chunk.Usage.CompletionTokens
				}
				if chunk.Usage.PromptTokens > 0 {
					promptTokens = chunk.Usage.PromptTokens
				}
			}

			// Emit a NDJSON line for every content delta.
			for _, choice := range chunk.Choices {
				content := choice.Delta.Content
				// Emit even empty strings to keep done:false heartbeats working;
				// but skip truly nil deltas (role-only chunks from some providers).
				var line []byte
				if origPath == "/api/chat" {
					line = buildChatNDJSON(clientModel, content, false, 0, 0)
				} else {
					line = buildGenerateNDJSON(clientModel, content, false, 0, 0)
				}
				if _, err := pw.Write(append(line, '\n')); err != nil {
					return
				}
			}
		}

		// A genuine [DONE] sentinel means the upstream finished normally.
		// Anything else (scanner error, or the loop exiting because the
		// connection dropped before [DONE] arrived) is a truncated stream -
		// report it as an error instead of fabricating a done:true success
		// line that would hide the truncation from the client.
		if err := scanner.Err(); err != nil || !sawDone {
			if err == nil {
				err = io.ErrUnexpectedEOF
			}
			pw.Write(append(buildErrorNDJSON("upstream stream ended unexpectedly"), '\n')) //nolint:errcheck -- CloseWithError below reports it
			pw.CloseWithError(err)
			return
		}

		// Final done:true line.
		var finalLine []byte
		if origPath == "/api/chat" {
			finalLine = buildChatNDJSON(clientModel, "", true, completionTokens, promptTokens)
		} else {
			finalLine = buildGenerateNDJSON(clientModel, "", true, completionTokens, promptTokens)
		}
		pw.Write(append(finalLine, '\n')) //nolint:errcheck -- pipe close handles error
	}()

	return pr
}

// ------------------------------------------------------------------
// Non-streaming JSON -> NDJSON translation
// ------------------------------------------------------------------

// translateJSONToNDJSON reads a single OpenAI non-streaming response and
// emits two Ollama NDJSON lines: one with content and done:false, and a
// second final line with done:true. Using two lines mirrors real Ollama
// behavior and means the token-count tail scan in statusRecorder will see the
// final line and pick up eval_count.
func translateJSONToNDJSON(src io.ReadCloser, origPath, clientModel string) io.ReadCloser {
	defer src.Close()

	raw, err := io.ReadAll(src)
	if err != nil {
		// Return an error line so the client sees something.
		errLine := `{"error":"failed to read cloud response"}` + "\n"
		return io.NopCloser(strings.NewReader(errLine))
	}

	// Embeddings responses have no choices/streaming shape at all - Ollama's
	// native /api/embeddings and /api/embed are each a single flat object,
	// not NDJSON lines. Detect and translate before the chat/generate
	// choices parsing below.
	if line, ok := translateEmbeddingResponse(raw, origPath, clientModel); ok {
		return io.NopCloser(bytes.NewReader(line))
	}

	var resp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			Text string `json:"text"` // completions endpoint
		} `json:"choices"`
		Usage struct {
			CompletionTokens int64 `json:"completion_tokens"`
			PromptTokens     int64 `json:"prompt_tokens"`
			TotalTokens      int64 `json:"total_tokens"`
		} `json:"usage"`
	}

	var buf bytes.Buffer
	if err := json.Unmarshal(raw, &resp); err == nil && len(resp.Choices) > 0 {
		content := resp.Choices[0].Message.Content
		if content == "" {
			content = resp.Choices[0].Text
		}
		var contentLine, finalLine []byte
		if origPath == "/api/chat" {
			contentLine = buildChatNDJSON(clientModel, content, false, 0, 0)
			finalLine = buildChatNDJSON(clientModel, "", true,
				resp.Usage.CompletionTokens, resp.Usage.PromptTokens)
		} else {
			contentLine = buildGenerateNDJSON(clientModel, content, false, 0, 0)
			finalLine = buildGenerateNDJSON(clientModel, "", true,
				resp.Usage.CompletionTokens, resp.Usage.PromptTokens)
		}
		buf.Write(append(contentLine, '\n'))
		buf.Write(append(finalLine, '\n'))
	} else {
		// Could not parse - pass raw through so nothing is silently lost.
		buf.Write(raw)
	}

	return io.NopCloser(&buf)
}

// translateJSONToSingleOllama reads a single OpenAI non-streaming response and
// emits ONE Ollama JSON object (done:true, full content + usage) - what an
// Ollama client expects when it requested stream:false. Unparseable bodies are
// passed through raw so nothing is silently lost.
func translateJSONToSingleOllama(src io.ReadCloser, origPath, clientModel string) io.ReadCloser {
	defer src.Close()
	raw, err := io.ReadAll(src)
	if err != nil {
		return io.NopCloser(strings.NewReader(`{"error":"failed to read cloud response"}` + "\n"))
	}
	if line, ok := translateEmbeddingResponse(raw, origPath, clientModel); ok {
		return io.NopCloser(bytes.NewReader(line))
	}
	var resp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			Text string `json:"text"`
		} `json:"choices"`
		Usage struct {
			CompletionTokens int64 `json:"completion_tokens"`
			PromptTokens     int64 `json:"prompt_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil || len(resp.Choices) == 0 {
		return io.NopCloser(bytes.NewReader(raw))
	}
	content := resp.Choices[0].Message.Content
	if content == "" {
		content = resp.Choices[0].Text
	}
	var line []byte
	if origPath == "/api/chat" {
		obj := ollamaChatLine{
			Model:           clientModel,
			Message:         &ollamaMessage{Role: "assistant", Content: content},
			Done:            true,
			EvalCount:       resp.Usage.CompletionTokens,
			PromptEvalCount: resp.Usage.PromptTokens,
		}
		line, _ = json.Marshal(obj)
	} else {
		line = buildGenerateNDJSON(clientModel, content, true, resp.Usage.CompletionTokens, resp.Usage.PromptTokens)
	}
	return io.NopCloser(bytes.NewReader(line))
}

// ------------------------------------------------------------------
// NDJSON line builders
// ------------------------------------------------------------------

type ollamaChatLine struct {
	Model           string         `json:"model"`
	Message         *ollamaMessage `json:"message,omitempty"`
	Done            bool           `json:"done"`
	EvalCount       int64          `json:"eval_count,omitempty"`
	PromptEvalCount int64          `json:"prompt_eval_count,omitempty"`
}

type ollamaMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ollamaGenerateLine struct {
	Model           string `json:"model"`
	Response        string `json:"response,omitempty"`
	Done            bool   `json:"done"`
	EvalCount       int64  `json:"eval_count,omitempty"`
	PromptEvalCount int64  `json:"prompt_eval_count,omitempty"`
}

func buildChatNDJSON(model, content string, done bool, evalCount, promptEvalCount int64) []byte {
	line := ollamaChatLine{
		Model:           model,
		Done:            done,
		EvalCount:       evalCount,
		PromptEvalCount: promptEvalCount,
	}
	if !done {
		line.Message = &ollamaMessage{Role: "assistant", Content: content}
	}
	b, _ := json.Marshal(line)
	return b
}

// buildErrorNDJSON builds a distinct NDJSON error line for a truncated
// stream, so a client scanning for "done":true never mistakes it for a
// successful completion.
func buildErrorNDJSON(message string) []byte {
	b, _ := json.Marshal(struct {
		Error string `json:"error"`
	}{Error: message})
	return b
}

func buildGenerateNDJSON(model, response string, done bool, evalCount, promptEvalCount int64) []byte {
	line := ollamaGenerateLine{
		Model:           model,
		Response:        response,
		Done:            done,
		EvalCount:       evalCount,
		PromptEvalCount: promptEvalCount,
	}
	b, _ := json.Marshal(line)
	return b
}

// openAIEmbeddingResponse matches the OpenAI /v1/embeddings response shape:
// {"data":[{"embedding":[...],"index":0},...],"usage":{"total_tokens":N}}.
// No "choices" key - that's what distinguishes it from chat/completions.
type openAIEmbeddingResponse struct {
	Data []struct {
		Embedding []float64 `json:"embedding"`
		Index     int       `json:"index"`
	} `json:"data"`
	Usage struct {
		TotalTokens int64 `json:"total_tokens"`
	} `json:"usage"`
}

// ollamaEmbeddingLine is Ollama's native legacy /api/embeddings shape: a
// single flat object, no "done"/"model" framing. PromptEvalCount is set only
// when the cloud provider actually reported usage - never fabricated.
type ollamaEmbeddingLine struct {
	Embedding       []float64 `json:"embedding"`
	PromptEvalCount int64     `json:"prompt_eval_count,omitempty"`
}

// ollamaEmbedLine is Ollama's native plural /api/embed shape: embeddings is
// an array of arrays even for a single input, matching real Ollama.
type ollamaEmbedLine struct {
	Model           string      `json:"model"`
	Embeddings      [][]float64 `json:"embeddings"`
	PromptEvalCount int64       `json:"prompt_eval_count,omitempty"`
}

func buildEmbeddingJSON(embedding []float64, promptEvalCount int64) []byte {
	b, _ := json.Marshal(ollamaEmbeddingLine{Embedding: embedding, PromptEvalCount: promptEvalCount})
	return b
}

func buildEmbedJSON(clientModel string, embeddings [][]float64, promptEvalCount int64) []byte {
	b, _ := json.Marshal(ollamaEmbedLine{Model: clientModel, Embeddings: embeddings, PromptEvalCount: promptEvalCount})
	return b
}

// translateEmbeddingResponse detects an OpenAI embeddings response shape
// (no "choices" key) for origPath "/api/embeddings" or "/api/embed" and
// translates it to the matching Ollama-native shape. Returns nil, false if
// raw doesn't parse as this shape or origPath isn't an embeddings path, so
// callers fall through to their existing chat/generate parsing unchanged.
func translateEmbeddingResponse(raw []byte, origPath, clientModel string) ([]byte, bool) {
	if origPath != "/api/embeddings" && origPath != "/api/embed" {
		return nil, false
	}
	var emb openAIEmbeddingResponse
	if err := json.Unmarshal(raw, &emb); err != nil || len(emb.Data) == 0 {
		return nil, false
	}
	if origPath == "/api/embeddings" {
		return buildEmbeddingJSON(emb.Data[0].Embedding, emb.Usage.TotalTokens), true
	}
	// /api/embed: place each embedding at its reported Index, not loop
	// position - correctness must not depend on provider response order.
	embeddings := make([][]float64, len(emb.Data))
	for _, d := range emb.Data {
		if d.Index >= 0 && d.Index < len(embeddings) {
			embeddings[d.Index] = d.Embedding
		}
	}
	return buildEmbedJSON(clientModel, embeddings, emb.Usage.TotalTokens), true
}
