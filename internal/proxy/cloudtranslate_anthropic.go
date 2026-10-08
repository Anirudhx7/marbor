package proxy

// cloudtranslate_anthropic.go -- translates OpenAI-shaped cloud requests into
// Anthropic's native /v1/messages schema, and translates Anthropic's
// responses back into OpenAI-shaped responses.
//
// Anthropic exposes only /v1/messages (no /v1/chat/completions, no
// /v1/completions, no /v1/embeddings). Rather than reject every non-messages
// request with a 501, anthropicTransport rewrites the outbound request into
// the Messages schema and rewrites the response back into the OpenAI chat-
// completions shape that the rest of the cloud fallback pipeline (including
// the Ollama NDJSON translator in cloudtranslate.go) already understands.
// Composing the two translators is what lets an Ollama-native client
// (/api/chat, /api/generate) reach Anthropic exactly like it reaches any
// other OpenAI-compatible provider.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
)

// anthropicVersion is the API version header Anthropic requires on every
// request. Pinned to a stable, widely supported version.
const anthropicVersion = "2023-06-01"

// anthropicDefaultMaxTokens is used when the incoming request specifies no
// max_tokens/max_completion_tokens - Anthropic rejects requests missing
// max_tokens, unlike OpenAI where it is optional.
const anthropicDefaultMaxTokens = 4096

// anthropicTransport wraps an inner RoundTripper and translates between the
// OpenAI-shaped request/response the rest of the proxy pipeline builds and
// Anthropic's native Messages API. It is inserted between the shared cloud
// transport and (when the original client path is Ollama-native) the
// translatingTransport from cloudtranslate.go, so both /v1/... passthrough
// clients and Ollama-native clients get a correctly shaped response.
type anthropicTransport struct {
	inner  http.RoundTripper
	apiKey string
}

func (t *anthropicTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		var err error
		body, err = io.ReadAll(req.Body)
		req.Body.Close() //nolint:errcheck
		if err != nil {
			return nil, err
		}
	}

	// Ollama clients default to streaming when "stream" is absent; OpenAI
	// clients default to non-streaming. The Director has already rewritten
	// the path, so translatingTransport marks Ollama-origin requests.
	ollamaOrigin := req.Context().Value(ollamaOriginKey{}) != nil
	wantsStream := ollamaOrigin
	var sp struct {
		Stream *bool `json:"stream"`
	}
	if json.Unmarshal(body, &sp) == nil && sp.Stream != nil {
		wantsStream = *sp.Stream
	}
	anthropicBody, err := translateOpenAIRequestToAnthropic(body, wantsStream)
	if err != nil {
		return nil, err
	}

	req.URL.Path = "/v1/messages"
	req.Body = io.NopCloser(bytes.NewReader(anthropicBody))
	req.ContentLength = int64(len(anthropicBody))
	req.Header.Set("Content-Type", "application/json")
	// Anthropic authenticates via x-api-key + anthropic-version, not
	// Authorization: Bearer. The admin token exact-match check is unaffected -
	// this is the outbound cloud-provider credential, unrelated to marbor
	// admin auth.
	req.Header.Del("Authorization")
	req.Header.Set("x-api-key", t.apiKey)
	req.Header.Set("anthropic-version", anthropicVersion)

	resp, err := t.inner.RoundTrip(req)
	if err != nil {
		return nil, err
	}

	ct := resp.Header.Get("Content-Type")
	if strings.Contains(ct, "text/event-stream") {
		resp.Body = translateAnthropicSSEToOpenAI(resp.Body)
		resp.Header.Set("Content-Type", "text/event-stream")
	} else {
		if !capCloudResponse(resp, ollamaOrigin) {
			return resp, nil
		}
		resp.Body = translateAnthropicJSONToOpenAI(resp.Body)
		resp.Header.Set("Content-Type", "application/json")
	}
	// Length changed under translation in both branches.
	resp.ContentLength = -1
	resp.Header.Del("Content-Length")
	return resp, nil
}

// ------------------------------------------------------------------
// Request translation: OpenAI-shape -> Anthropic Messages shape
// ------------------------------------------------------------------

type openAIMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openAIRequest struct {
	Model               string          `json:"model"`
	Messages            []openAIMessage `json:"messages"`
	Prompt              string          `json:"prompt"`
	Stream              bool            `json:"stream"`
	Temperature         *float64        `json:"temperature"`
	MaxTokens           int             `json:"max_tokens"`
	MaxCompletionTokens int             `json:"max_completion_tokens"`
	// Stop mirrors OpenAI's own schema, which accepts either a bare string or
	// a []string - flexStop's UnmarshalJSON normalizes both to []string.
	Stop flexStop `json:"stop"`
	TopP *float64 `json:"top_p"`
}

// flexStop unmarshals OpenAI's polymorphic "stop" field (a JSON string or an
// array of strings) into a single []string representation.
type flexStop []string

func (s *flexStop) UnmarshalJSON(data []byte) error {
	if len(data) == 0 || string(data) == "null" {
		*s = nil
		return nil
	}
	if data[0] == '"' {
		var one string
		if err := json.Unmarshal(data, &one); err != nil {
			return err
		}
		*s = flexStop{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(data, &many); err != nil {
		return err
	}
	*s = flexStop(many)
	return nil
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type anthropicRequest struct {
	Model         string             `json:"model"`
	MaxTokens     int                `json:"max_tokens"`
	System        string             `json:"system,omitempty"`
	Messages      []anthropicMessage `json:"messages"`
	Stream        bool               `json:"stream"`
	Temperature   *float64           `json:"temperature,omitempty"`
	StopSequences []string           `json:"stop_sequences,omitempty"`
	TopP          *float64           `json:"top_p,omitempty"`
}

// clampAnthropicTemperature limits temperature to Anthropic's 0..1 range
// (OpenAI allows up to 2).
func clampAnthropicTemperature(t *float64) *float64 {
	if t == nil {
		return nil
	}
	v := *t
	if v > 1 {
		v = 1
	}
	if v < 0 {
		v = 0
	}
	// Logged on every clamped request (one line per request), not deduplicated.
	if v != *t {
		log.Printf("anthropic: temperature %v is outside 0..1, clamped to %v", *t, v)
	}
	return &v
}

// openAIFinishReason maps an Anthropic stop_reason to the OpenAI
// finish_reason vocabulary. An empty reason stays empty and an unknown one
// becomes "stop". tool_use -> tool_calls is unreachable until tool
// translation exists; it is mapped so the vocabulary is already correct then.
// Unknown values are logged so a new Anthropic stop reason is noticed.
func openAIFinishReason(stop string) string {
	switch stop {
	case "":
		// No stop_reason reported: emit no finish_reason at all rather than
		// inventing one.
		return ""
	case "end_turn", "stop_sequence":
		return "stop"
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	case "refusal":
		return "content_filter"
	default:
		log.Printf("anthropic: unknown stop_reason %q mapped to \"stop\"", stop)
		return "stop"
	}
}

// translateOpenAIRequestToAnthropic converts an OpenAI-shaped chat/completion
// request body into Anthropic's /v1/messages schema. Returns an error rather
// than the body unchanged when it can't be understood - previously an
// unparseable body (most commonly a vision-capable client sending
// multimodal/array message content, which openAIMessage.Content as a plain
// string can never unmarshal) passed through unchanged, silently forwarding
// an OpenAI-shaped body to Anthropic's /v1/messages, which was guaranteed to
// reject it - the caller can now fail fast/fall through to another cloud
// provider instead of spending a real round-trip on a request that could
// never have succeeded.
func translateOpenAIRequestToAnthropic(body []byte, wantsStream bool) ([]byte, error) {
	var in openAIRequest
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, fmt.Errorf("proxy: request body is not a valid OpenAI-shaped chat request for Anthropic translation (e.g. multimodal/array message content is not supported): %w", err)
	}

	out := anthropicRequest{
		Model:         in.Model,
		Stream:        wantsStream,
		Messages:      []anthropicMessage{},
		Temperature:   clampAnthropicTemperature(in.Temperature),
		StopSequences: in.Stop,
		TopP:          in.TopP,
	}

	switch {
	case in.MaxTokens > 0:
		out.MaxTokens = in.MaxTokens
	case in.MaxCompletionTokens > 0:
		out.MaxTokens = in.MaxCompletionTokens
	default:
		out.MaxTokens = anthropicDefaultMaxTokens
	}

	if len(in.Messages) > 0 {
		var system []string
		for _, m := range in.Messages {
			if m.Role == "system" || m.Role == "developer" {
				if m.Content != "" {
					system = append(system, m.Content)
				}
				continue
			}
			out.Messages = append(out.Messages, anthropicMessage{Role: m.Role, Content: m.Content})
		}
		if len(system) > 0 {
			out.System = strings.Join(system, "\n\n")
		}
	} else if in.Prompt != "" {
		// /api/generate and legacy /v1/completions requests carry a bare
		// prompt string instead of a messages array.
		out.Messages = []anthropicMessage{{Role: "user", Content: in.Prompt}}
	}

	b, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("proxy: marshal translated Anthropic request: %w", err)
	}
	return b, nil
}

// ------------------------------------------------------------------
// Response translation: Anthropic Messages shape -> OpenAI shape
// ------------------------------------------------------------------

type anthropicContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type anthropicUsage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}

type anthropicResponse struct {
	Content    []anthropicContentBlock `json:"content"`
	StopReason string                  `json:"stop_reason"`
	Usage      anthropicUsage          `json:"usage"`
}

// openAIChoice/openAIChatResponse mirror just the fields the rest of the
// pipeline (translateJSONToNDJSON/translateJSONToSingleOllama, or a direct
// /v1/ client) reads back out of a non-streaming chat-completions response.
type openAIChoiceMessage struct {
	Content string `json:"content"`
}

type openAIChoice struct {
	Message      openAIChoiceMessage `json:"message"`
	FinishReason string              `json:"finish_reason,omitempty"`
}

type openAIUsage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
}

type openAIChatResponse struct {
	Object  string         `json:"object"`
	Choices []openAIChoice `json:"choices"`
	Usage   openAIUsage    `json:"usage"`
}

// translateAnthropicJSONToOpenAI reads a single non-streaming Anthropic
// response and emits an OpenAI-shaped chat-completions JSON object.
// Unparseable bodies pass through raw so nothing is silently lost.
func translateAnthropicJSONToOpenAI(src io.ReadCloser) io.ReadCloser {
	defer src.Close()
	// src is the buffered body from capCloudResponse (the single read point);
	// this read cannot fail.
	raw, _ := io.ReadAll(src)

	// Anthropic error responses are a distinct envelope shape
	// ({"type":"error","error":{...}}), not a zero-valued success body -
	// unmarshaling one into anthropicResponse below would silently produce a
	// translated {"message":{"content":""},"done":true}-shaped "success" with
	// the real failure lost. Detect it up front and pass the raw error body
	// through unchanged, same as the unparseable-shape branch just below.
	var envelope struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &envelope); err == nil && envelope.Type == "error" {
		return io.NopCloser(bytes.NewReader(raw))
	}

	var resp anthropicResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return io.NopCloser(bytes.NewReader(raw))
	}

	var text strings.Builder
	for _, block := range resp.Content {
		if block.Type == "text" {
			text.WriteString(block.Text)
		}
	}

	out := openAIChatResponse{
		Object: "chat.completion",
		Choices: []openAIChoice{{
			Message:      openAIChoiceMessage{Content: text.String()},
			FinishReason: openAIFinishReason(resp.StopReason),
		}},
		Usage: openAIUsage{
			PromptTokens:     resp.Usage.InputTokens,
			CompletionTokens: resp.Usage.OutputTokens,
			TotalTokens:      resp.Usage.InputTokens + resp.Usage.OutputTokens,
		},
	}
	b, err := json.Marshal(out)
	if err != nil {
		return io.NopCloser(bytes.NewReader(raw))
	}
	return io.NopCloser(bytes.NewReader(b))
}

// anthropicSSEEvent is the minimal shape needed from each Anthropic SSE data
// payload to drive the OpenAI-chunk translation below.
type anthropicSSEEvent struct {
	Type  string `json:"type"`
	Delta struct {
		Type       string `json:"type"`
		Text       string `json:"text"`
		StopReason string `json:"stop_reason"`
	} `json:"delta"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
	Message *struct {
		Usage anthropicUsage `json:"usage"`
	} `json:"message"`
	Usage *struct {
		OutputTokens int64 `json:"output_tokens"`
	} `json:"usage"`
}

// translateAnthropicSSEToOpenAI returns a ReadCloser that reads an Anthropic
// SSE event stream from src and emits OpenAI-shaped SSE chunks
// ("data: {...}\n\n" ... "data: [DONE]\n\n"), the exact shape
// translateSSEToNDJSON (cloudtranslate.go) and any /v1/-passthrough client
// already expect. The pipe is unbuffered on the write side (streaming stays
// streaming: no buffering) - each event is translated and written as soon as
// it is scanned.
func translateAnthropicSSEToOpenAI(src io.ReadCloser) io.ReadCloser {
	pr, pw := io.Pipe()

	go func() {
		defer src.Close()
		defer pw.Close()

		scanner := bufio.NewScanner(src)
		scanner.Buffer(make([]byte, 0, 64*1024), 4<<20)

		var (
			inputTokens  int64
			outputTokens int64
			stopReason   string
		)

		writeChunk := func(content string) error {
			chunk := struct {
				Choices []struct {
					Delta struct {
						Content string `json:"content"`
					} `json:"delta"`
				} `json:"choices"`
			}{}
			chunk.Choices = []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			}{{}}
			chunk.Choices[0].Delta.Content = content
			b, _ := json.Marshal(chunk)
			_, err := pw.Write([]byte("data: " + string(b) + "\n\n"))
			return err
		}

		// closeWithErrorChunk emits an OpenAI-style error chunk, then ends the
		// stream with err so the failure is never mistaken for a clean finish.
		closeWithErrorChunk := func(errType, errMsg string, err error) {
			type errBody struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			}
			b, _ := json.Marshal(struct {
				Error errBody `json:"error"`
			}{Error: errBody{Type: errType, Message: errMsg}})
			logStreamFailure(errType+": "+errMsg, err)
			pw.Write([]byte("data: " + string(b) + "\n\n")) //nolint:errcheck -- CloseWithError below reports it
			pw.CloseWithError(err)
		}

		for scanner.Scan() {
			payload, ok := sseData(scanner.Text())
			if !ok {
				continue
			}

			var evt anthropicSSEEvent
			if err := json.Unmarshal([]byte(payload), &evt); err != nil {
				continue
			}

			switch evt.Type {
			case "message_start":
				if evt.Message != nil {
					inputTokens = evt.Message.Usage.InputTokens
				}
			case "content_block_delta":
				if evt.Delta.Type == "text_delta" && evt.Delta.Text != "" {
					if err := writeChunk(evt.Delta.Text); err != nil {
						return
					}
				}
			case "message_delta":
				if evt.Usage != nil {
					outputTokens = evt.Usage.OutputTokens
				}
				if evt.Delta.StopReason != "" {
					stopReason = evt.Delta.StopReason
				}
			case "error":
				errType, errMsg := "api_error", "upstream stream error"
				if evt.Error != nil {
					if evt.Error.Type != "" {
						errType = evt.Error.Type
					}
					if evt.Error.Message != "" {
						errMsg = evt.Error.Message
					}
				}
				closeWithErrorChunk(errType, errMsg, fmt.Errorf("anthropic stream error: %s: %s", errType, errMsg))
				return
			case "message_stop":
				if stopReason != "" {
					fin, _ := json.Marshal(map[string]any{"choices": []map[string]any{{
						"index":         0,
						"delta":         map[string]any{},
						"finish_reason": openAIFinishReason(stopReason),
					}}})
					if _, err := pw.Write([]byte("data: " + string(fin) + "\n\n")); err != nil {
						return
					}
				}
				usageChunk := struct {
					Choices []struct{} `json:"choices"`
					Usage   struct {
						PromptTokens     int64 `json:"prompt_tokens"`
						CompletionTokens int64 `json:"completion_tokens"`
						TotalTokens      int64 `json:"total_tokens"`
					} `json:"usage"`
				}{}
				usageChunk.Choices = []struct{}{}
				usageChunk.Usage.PromptTokens = inputTokens
				usageChunk.Usage.CompletionTokens = outputTokens
				usageChunk.Usage.TotalTokens = inputTokens + outputTokens
				b, _ := json.Marshal(usageChunk)
				pw.Write([]byte("data: " + string(b) + "\n\n")) //nolint:errcheck
				pw.Write([]byte("data: [DONE]\n\n"))            //nolint:errcheck
				return
			}
		}

		// Reaching here means the stream ended without message_stop: a
		// truncated stream, reported as an error rather than a clean close.
		err := scanner.Err()
		if err == nil {
			err = io.ErrUnexpectedEOF
		}
		closeWithErrorChunk("api_error", streamFailureMessage(err), err)
	}()

	return pr
}
