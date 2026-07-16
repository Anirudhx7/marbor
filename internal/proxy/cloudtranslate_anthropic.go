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
	"io"
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

	wantsStream := clientWantsStream(body)
	anthropicBody := translateOpenAIRequestToAnthropic(body, wantsStream)

	req.URL.Path = "/v1/messages"
	req.Body = io.NopCloser(bytes.NewReader(anthropicBody))
	req.ContentLength = int64(len(anthropicBody))
	req.Header.Set("Content-Type", "application/json")
	// Anthropic authenticates via x-api-key + anthropic-version, not
	// Authorization: Bearer. R4 (admin token exact-match) is unaffected -
	// this is the outbound cloud-provider credential, unrelated to mesh
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
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type anthropicRequest struct {
	Model       string             `json:"model"`
	MaxTokens   int                `json:"max_tokens"`
	System      string             `json:"system,omitempty"`
	Messages    []anthropicMessage `json:"messages"`
	Stream      bool               `json:"stream"`
	Temperature *float64           `json:"temperature,omitempty"`
}

// translateOpenAIRequestToAnthropic converts an OpenAI-shaped chat/completion
// request body into Anthropic's /v1/messages schema. Unparseable bodies pass
// through unchanged (Anthropic will reject with a clear error rather than the
// mesh silently mangling something it doesn't understand).
func translateOpenAIRequestToAnthropic(body []byte, wantsStream bool) []byte {
	var in openAIRequest
	if err := json.Unmarshal(body, &in); err != nil {
		return body
	}

	out := anthropicRequest{
		Model:       in.Model,
		Stream:      wantsStream,
		Temperature: in.Temperature,
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
			if m.Role == "system" {
				system = append(system, m.Content)
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
		return body
	}
	return b
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
	raw, err := io.ReadAll(src)
	if err != nil {
		return io.NopCloser(strings.NewReader(`{"error":"failed to read cloud response"}` + "\n"))
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
			FinishReason: resp.StopReason,
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
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"delta"`
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
// already expect. The pipe is unbuffered on the write side (R2: no
// buffering) - each event is translated and written as soon as it is
// scanned.
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

		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			payload := strings.TrimPrefix(line, "data: ")

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
			case "message_stop":
				usageChunk := struct {
					Choices []struct{} `json:"choices"`
					Usage   struct {
						PromptTokens     int64 `json:"prompt_tokens"`
						CompletionTokens int64 `json:"completion_tokens"`
					} `json:"usage"`
				}{}
				usageChunk.Choices = []struct{}{}
				usageChunk.Usage.PromptTokens = inputTokens
				usageChunk.Usage.CompletionTokens = outputTokens
				b, _ := json.Marshal(usageChunk)
				pw.Write([]byte("data: " + string(b) + "\n\n")) //nolint:errcheck
				pw.Write([]byte("data: [DONE]\n\n"))            //nolint:errcheck
				return
			}
		}

		if err := scanner.Err(); err != nil {
			pw.CloseWithError(err)
		}
	}()

	return pr
}
