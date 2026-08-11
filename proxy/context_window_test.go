package proxy

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/png"
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

func TestTruncateCurrentMessagePreservesTaskStartAndLatestInstruction(t *testing.T) {
	payload := &KiroPayload{}
	payload.ConversationState.CurrentMessage.UserInputMessage = KiroUserInputMessage{
		ModelID: "gpt-5.6-sol",
		Origin:  "AI_EDITOR",
		Content: "ORIGINAL REQUIREMENT\n" + strings.Repeat("middle ", 20_000) + "\nUSE PLAN MODE",
	}
	truncatePayloadToByteLimit(payload, false, 2_000)
	got := payload.ConversationState.CurrentMessage.UserInputMessage.Content
	if !strings.Contains(got, "ORIGINAL REQUIREMENT") || !strings.Contains(got, "USE PLAN MODE") {
		t.Fatalf("middle truncation lost task boundary instructions: %q", got)
	}
}

func TestTruncatePayloadCompressesCurrentImagesBeforeDroppingTask(t *testing.T) {
	imageData := image.NewNRGBA(image.Rect(0, 0, 700, 700))
	state := uint32(1)
	for y := 0; y < 700; y++ {
		for x := 0; x < 700; x++ {
			state = state*1664525 + 1013904223
			offset := imageData.PixOffset(x, y)
			imageData.Pix[offset+0] = uint8(state >> 24)
			imageData.Pix[offset+1] = uint8(state >> 16)
			imageData.Pix[offset+2] = uint8(state >> 8)
			imageData.Pix[offset+3] = 255
		}
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, imageData); err != nil {
		t.Fatalf("encode test image: %v", err)
	}
	kiroImage := KiroImage{Format: "png"}
	kiroImage.Source.Bytes = base64.StdEncoding.EncodeToString(encoded.Bytes())

	payload := &KiroPayload{}
	payload.ConversationState.CurrentMessage.UserInputMessage = KiroUserInputMessage{
		ModelID: "gpt-5.6-sol",
		Origin:  "AI_EDITOR",
		Content: "ORIGINAL TASK\nUse Plan mode after reading the screenshot.",
		Images:  []KiroImage{kiroImage},
	}
	originalSize := payloadByteSize(payload)
	truncatePayloadToByteLimit(payload, false, 90_000)
	if got := payloadByteSize(payload); got > 90_000 {
		t.Fatalf("compressed payload still exceeds limit: %d", got)
	}
	if payloadByteSize(payload) >= originalSize {
		t.Fatalf("expected image payload to shrink")
	}
	if len(payload.ConversationState.CurrentMessage.UserInputMessage.Images) != 1 {
		t.Fatalf("expected compressible image to be retained")
	}
	content := payload.ConversationState.CurrentMessage.UserInputMessage.Content
	if !strings.Contains(content, "ORIGINAL TASK") || !strings.Contains(content, "Use Plan mode") {
		t.Fatalf("image fitting lost task text: %q", content)
	}
}

func TestTruncatePayloadTrimsCurrentToolResults(t *testing.T) {
	payload := &KiroPayload{}
	payload.ConversationState.History = []KiroHistoryMessage{
		{UserInputMessage: &KiroUserInputMessage{ModelID: "gpt-5.6-sol", Origin: "AI_EDITOR", Content: "system instructions"}},
		{AssistantResponseMessage: &KiroAssistantResponseMessage{Content: "I will follow these instructions."}},
		{UserInputMessage: &KiroUserInputMessage{ModelID: "gpt-5.6-sol", Origin: "AI_EDITOR", Content: "run the tools"}},
		{AssistantResponseMessage: &KiroAssistantResponseMessage{ToolUses: []KiroToolUse{
			{ToolUseID: "call_exec", Name: "exec", Input: map[string]interface{}{"cmd": "large output"}},
			{ToolUseID: "call_collaboration", Name: "collaboration", Input: map[string]interface{}{"task": "large output"}},
		}}},
	}
	payload.ConversationState.CurrentMessage.UserInputMessage = KiroUserInputMessage{
		ModelID: "gpt-5.6-sol",
		Origin:  "AI_EDITOR",
		Content: minimalFallbackUserContent,
		UserInputMessageContext: &UserInputMessageContext{
			ToolResults: []KiroToolResult{
				{ToolUseID: "call_exec", Status: "success", Content: []KiroResultContent{{Text: "EXEC START\n" + strings.Repeat("x", 4_000_000) + "\nEXEC END"}}},
				{ToolUseID: "call_collaboration", Status: "success", Content: []KiroResultContent{{Text: "AGENT START\n" + strings.Repeat("y", 4_000_000) + "\nAGENT END"}}},
			},
		},
	}

	truncatePayloadToByteLimit(payload, true, 120_000)
	if got := payloadByteSize(payload); got > 120_000 {
		t.Fatalf("tool-result payload still exceeds limit: %d", got)
	}
	results := payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext.ToolResults
	if len(results) != 2 || results[0].ToolUseID != "call_exec" || results[1].ToolUseID != "call_collaboration" {
		t.Fatalf("tool-result pairing was not preserved: %#v", results)
	}
	history := payload.ConversationState.History
	if len(history) < 3 || history[len(history)-1].AssistantResponseMessage == nil ||
		len(history[len(history)-1].AssistantResponseMessage.ToolUses) != 2 {
		t.Fatalf("active assistant tool-use turn was dropped: %#v", history)
	}
	for i, result := range results {
		if len(result.Content) != 1 || !strings.Contains(result.Content[0].Text, "Tool output truncated") {
			t.Fatalf("tool result %d was not visibly truncated: %#v", i, result.Content)
		}
	}
	if !strings.Contains(results[0].Content[0].Text, "EXEC START") || !strings.Contains(results[0].Content[0].Text, "EXEC END") {
		t.Fatalf("exec result boundaries were lost")
	}
	if !strings.Contains(results[1].Content[0].Text, "AGENT START") || !strings.Contains(results[1].Content[0].Text, "AGENT END") {
		t.Fatalf("collaboration result boundaries were lost")
	}
}

func TestTruncatePayloadRetainsOriginalTaskWhenCurrentImagesForceCompaction(t *testing.T) {
	const task = "Rebuild the Roku player screen from the supplied design references, preserving the existing playback behavior."
	payload := &KiroPayload{TaskAnchor: task}
	payload.ConversationState.History = []KiroHistoryMessage{
		{UserInputMessage: &KiroUserInputMessage{ModelID: "gpt-5.6-sol", Origin: "AI_EDITOR", Content: "system instructions"}},
		{AssistantResponseMessage: &KiroAssistantResponseMessage{Content: "I will follow these instructions."}},
		{UserInputMessage: &KiroUserInputMessage{ModelID: "gpt-5.6-sol", Origin: "AI_EDITOR", Content: task}},
		{AssistantResponseMessage: &KiroAssistantResponseMessage{Content: "I will inspect the reference images."}},
		{UserInputMessage: &KiroUserInputMessage{ModelID: "gpt-5.6-sol", Origin: "AI_EDITOR", Content: strings.Repeat("old tool output ", 20_000)}},
		{AssistantResponseMessage: &KiroAssistantResponseMessage{ToolUses: []KiroToolUse{
			{ToolUseID: "call_view", Name: "view_image", Input: map[string]interface{}{"path": "reference.png"}},
		}}},
	}
	image := KiroImage{Format: "png"}
	image.Source.Bytes = strings.Repeat("A", 2_000_000)
	payload.ConversationState.CurrentMessage.UserInputMessage = KiroUserInputMessage{
		ModelID: "gpt-5.6-sol",
		Origin:  "AI_EDITOR",
		Content: toolResultImagePlaceholder,
		Images:  []KiroImage{image, image, image, image},
		UserInputMessageContext: &UserInputMessageContext{ToolResults: []KiroToolResult{
			{ToolUseID: "call_view", Status: "success", Content: []KiroResultContent{{Text: toolResultImagePlaceholder}}},
		}},
	}

	truncatePayloadToByteLimit(payload, true, 120_000)
	if got := payloadByteSize(payload); got > 120_000 {
		t.Fatalf("image payload still exceeds limit: %d", got)
	}
	current := payload.ConversationState.CurrentMessage.UserInputMessage.Content
	if !strings.Contains(current, task) || !strings.Contains(current, taskAnchorPrefix) {
		t.Fatalf("original task was not retained in current content: %q", current)
	}
	if !payload.TaskAnchorInjected {
		t.Fatalf("expected task anchor injection to be recorded")
	}
	history := payload.ConversationState.History
	last := history[len(history)-1].AssistantResponseMessage
	if last == nil || len(last.ToolUses) != 1 || last.ToolUses[0].ToolUseID != "call_view" {
		t.Fatalf("active image tool turn was not preserved: %#v", history)
	}
	context := payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext
	if context == nil || len(context.ToolResults) != 1 || context.ToolResults[0].ToolUseID != "call_view" {
		t.Fatalf("active image tool result was not preserved: %#v", context)
	}
}
