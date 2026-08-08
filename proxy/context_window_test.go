package proxy

import (
	"strings"
	"testing"
)

// TestGetContextWindowSize verifies models are classified into the correct
// context window. This drives the input-token count that clients use to decide
// when to compact; misclassifying opus-4.8 or opus-5 (1M) as 200K under-reports
// tokens by 5x and prevents timely compaction.
func TestGetContextWindowSize(t *testing.T) {
	cases := []struct {
		model string
		want  int
	}{
		{"claude-opus-4.8", 1_000_000},
		{"claude-opus-4-8", 1_000_000},
		{"claude-opus-4.7", 1_000_000},
		{"claude-opus-4.6", 1_000_000},
		{"claude-sonnet-4.6", 1_000_000},
		{"claude-opus-4.8-thinking", 1_000_000},
		{"CLAUDE-OPUS-4.8", 1_000_000},
		{"claude-opus-5", 1_000_000},
		{"claude-opus-5-thinking", 1_000_000},
		{"CLAUDE-OPUS-5", 1_000_000},
		{"claude-opus-5.1", 1_000_000},
		{"claude-opus-5-1", 1_000_000},
		{"claude-sonnet-5", 1_000_000},
		{"claude-haiku-5", 1_000_000},
		{"claude-opus-6", 1_000_000},
		{"claude-opus-4.5", 200_000},
		{"claude-sonnet-4.5", 200_000},
		{"claude-sonnet-4", 200_000},
		{"claude-haiku-4.5", 200_000},
		{"claude-3-5-sonnet", 200_000},
		{"unknown-model", 200_000},
	}
	for _, c := range cases {
		if got := getContextWindowSize(c.model); got != c.want {
			t.Errorf("getContextWindowSize(%q) = %d, want %d", c.model, got, c.want)
		}
	}
}

func TestHandlerContextWindowUsesKiroTokenLimitsForGPT56(t *testing.T) {
	h := &Handler{cachedModels: []ModelInfo{
		{ModelId: "gpt-5.6-sol", TokenLimits: &ModelTokenLimits{MaxInputTokens: 128_000, MaxOutputTokens: 32_000}},
		{ModelId: "gpt-5.6-terra", TokenLimits: &ModelTokenLimits{MaxInputTokens: 256_000, MaxOutputTokens: 32_000}},
		{ModelId: "gpt-5.6-luna", TokenLimits: &ModelTokenLimits{MaxInputTokens: 64_000, MaxOutputTokens: 16_000}},
	}}
	for model, want := range map[string]int{
		"gpt-5.6-sol":   128_000,
		"gpt-5.6-terra": 256_000,
		"gpt-5.6-luna":  64_000,
	} {
		if got := h.getContextWindowSize(model); got != want {
			t.Fatalf("getContextWindowSize(%q) = %d, want %d", model, got, want)
		}
	}
}

func TestTruncatePayloadForModelUsesTokenDerivedBudget(t *testing.T) {
	req := &OpenAIRequest{
		Model: "gpt-5.6-sol",
		Messages: []OpenAIMessage{
			{Role: "system", Content: strings.Repeat("system ", 20_000)},
			{Role: "user", Content: strings.Repeat("history ", 20_000)},
			{Role: "assistant", Content: strings.Repeat("answer ", 20_000)},
			{Role: "user", Content: "current request"},
		},
	}
	payload := OpenAIToKiro(req, false)
	truncatePayloadForModel(payload, 10_000)
	if got, wantMax := payloadByteSize(payload), 30_000; got > wantMax {
		t.Fatalf("payload size %d exceeds model-derived limit %d", got, wantMax)
	}
	if got := payload.ConversationState.CurrentMessage.UserInputMessage.Content; got != "current request" {
		t.Fatalf("expected current request to be preserved, got %q", got)
	}
}
