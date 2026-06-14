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

// isOllamaPath reports whether path is an Ollama-native API path that needs
// response translation on cloud fallback.
func isOllamaPath(path string) bool {
	return path == "/api/chat" || path == "/api/generate"
}

// translatingTransport wraps an inner RoundTripper and, for responses to
// Ollama-native requests, replaces the response body with an Ollama NDJSON
// stream translated from the OpenAI SSE (or JSON) response.
type translatingTransport struct {
	inner       http.RoundTripper
	origPath    string // the client's original request path (e.g. /api/chat)
	clientModel string // model name the client asked for (echoed back in NDJSON)
}

func (t *translatingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.inner.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if !isOllamaPath(t.origPath) {
		return resp, nil
	}

	ct := resp.Header.Get("Content-Type")
	isSSE := strings.Contains(ct, "text/event-stream")

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

		var (
			completionTokens int64
			promptTokens     int64
		)

		for scanner.Scan() {
			raw := scanner.Text()

			// SSE lines look like "data: {...}" or "data: [DONE]".
			if !strings.HasPrefix(raw, "data: ") {
				continue
			}
			payload := strings.TrimPrefix(raw, "data: ")
			if payload == "[DONE]" {
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
