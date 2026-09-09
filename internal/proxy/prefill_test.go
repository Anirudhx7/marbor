package proxy

import "testing"

func TestPromptEvalDurationMsOllamaNDJSON(t *testing.T) {
	body := `{"model":"llama3","response":"hi","done":false}
{"model":"llama3","done":true,"prompt_eval_duration":9000000,"eval_duration":120000000}
`
	if got := recorderWith(body).promptEvalDurationMs(); got != 9 {
		t.Errorf("promptEvalDurationMs = %d, want 9 (9000000ns -> 9ms)", got)
	}
}

func TestPromptEvalDurationMsSSEFraming(t *testing.T) {
	body := "data: {\"choices\":[]}\n\ndata: {\"prompt_eval_duration\":14000000}\n\ndata: [DONE]\n"
	if got := recorderWith(body).promptEvalDurationMs(); got != 14 {
		t.Errorf("promptEvalDurationMs = %d, want 14 (final SSE chunk)", got)
	}
}

func TestPromptEvalDurationMsMissingReturnsZero(t *testing.T) {
	if got := recorderWith(`{"id":"chatcmpl-1","choices":[],"usage":{"total_tokens":30}}`).promptEvalDurationMs(); got != 0 {
		t.Errorf("promptEvalDurationMs = %d, want 0 for a non-Ollama (OpenAI usage) response", got)
	}
}

func TestPromptEvalDurationMsZeroDurationReturnsZero(t *testing.T) {
	if got := recorderWith(`{"done":true,"prompt_eval_duration":0,"eval_duration":5000000}`).promptEvalDurationMs(); got != 0 {
		t.Errorf("promptEvalDurationMs = %d, want 0 when prompt_eval_duration is genuinely 0 (e.g. a fully cached prompt)", got)
	}
}
