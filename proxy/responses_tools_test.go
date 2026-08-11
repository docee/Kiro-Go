package proxy

import (
	"encoding/json"
	"kiro-go/config"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testOpenAITool(name, namespace string) OpenAITool {
	tool := OpenAITool{Type: "function", Namespace: namespace}
	tool.Function.Name = name
	tool.Function.Description = "test tool"
	tool.Function.Parameters = map[string]interface{}{"type": "object"}
	return tool
}

func TestMergeOpenAIToolsDeduplicatesTopLevelAndAdditionalTools(t *testing.T) {
	first := testOpenAITool("request_user_input", "functions")
	duplicate := testOpenAITool("request_user_input", "functions")
	other := testOpenAITool("exec", "")

	got := mergeOpenAITools([]OpenAITool{first}, []OpenAITool{duplicate, other})
	if len(got) != 2 {
		t.Fatalf("expected 2 unique tools, got %d", len(got))
	}
	if got[0].Function.Name != "request_user_input" || got[1].Function.Name != "exec" {
		t.Fatalf("unexpected merged tools: %#v", openAIToolNames(got))
	}
}

func TestOpenAIToKiroHonorsNamedToolChoice(t *testing.T) {
	req := &OpenAIRequest{
		Model:    "gpt-5.6-sol",
		Messages: []OpenAIMessage{{Role: "user", Content: "ask me"}},
		Tools: []OpenAITool{
			testOpenAITool("exec", ""),
			testOpenAITool("request_user_input", "functions"),
		},
		ToolChoice: map[string]interface{}{
			"type":      "function",
			"name":      "request_user_input",
			"namespace": "functions",
		},
	}

	payload := OpenAIToKiro(req, false)
	ctx := payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext
	if ctx == nil || len(ctx.Tools) != 1 {
		t.Fatalf("expected only the selected tool, got %#v", ctx)
	}
	if got := ctx.Tools[0].ToolSpecification.Name; got != "request_user_input" {
		t.Fatalf("expected request_user_input, got %q", got)
	}
	if len(payload.ConversationState.History) < 2 ||
		!strings.Contains(payload.ConversationState.History[0].UserInputMessage.Content, "must call") {
		t.Fatalf("expected forced-tool instruction in system priming")
	}
}

func TestOpenAIToKiroHonorsNoneToolChoice(t *testing.T) {
	req := &OpenAIRequest{
		Model:      "gpt-5.6-sol",
		Messages:   []OpenAIMessage{{Role: "user", Content: "answer directly"}},
		Tools:      []OpenAITool{testOpenAITool("exec", "")},
		ToolChoice: "none",
	}
	payload := OpenAIToKiro(req, false)
	ctx := payload.ConversationState.CurrentMessage.UserInputMessage.UserInputMessageContext
	if ctx != nil && len(ctx.Tools) != 0 {
		t.Fatalf("expected no tools for tool_choice=none, got %#v", ctx.Tools)
	}
}

func TestOpenAIToKiroForwardsDeveloperInstructions(t *testing.T) {
	req := &OpenAIRequest{
		Model: "gpt-5.6-sol",
		Messages: []OpenAIMessage{
			{Role: "developer", Content: "Always inspect before answering."},
			{Role: "user", Content: "What should change?"},
		},
	}
	payload := OpenAIToKiro(req, false)
	if len(payload.ConversationState.History) < 2 {
		t.Fatalf("expected developer instructions to create system priming")
	}
	got := payload.ConversationState.History[0].UserInputMessage.Content
	if !strings.Contains(got, "Always inspect before answering.") {
		t.Fatalf("developer instructions were dropped: %q", got)
	}
}

func TestOpenAIToKiroReinforcesCodexPlanMode(t *testing.T) {
	req := &OpenAIRequest{
		Model: "gpt-5.6-sol",
		Messages: []OpenAIMessage{
			{Role: "developer", Content: "<collaboration_mode># Collaboration Mode: Plan\nDo not make edits.</collaboration_mode>"},
			{Role: "user", Content: "Implement the feature"},
		},
		Tools: []OpenAITool{
			testOpenAITool("exec", ""),
			testOpenAITool("request_user_input", "functions"),
		},
	}
	payload := OpenAIToKiro(req, false)
	if len(payload.ConversationState.History) < 2 {
		t.Fatalf("expected Plan mode system priming")
	}
	got := payload.ConversationState.History[0].UserInputMessage.Content
	for _, want := range []string{"Collaboration Mode: Plan", "<plan_mode_guard>", "Do not modify files", "response must begin with <proposed_plan>"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q from Plan mode prompt: %q", want, got)
		}
	}
	if !openAIRequestUsesPlanMode(req) {
		t.Fatalf("expected request to be detected as Plan mode")
	}
}

func TestNormalizeCodexProposedPlanDropsTextOutsideBlock(t *testing.T) {
	raw := "Plan is ready. <proposed_plan>\r\n# Title\n\n- Change\n</proposed_plan> trailing note"
	got, ok := normalizeCodexProposedPlan(raw)
	if !ok {
		t.Fatal("expected proposed plan block to be detected")
	}
	want := "<proposed_plan>\n# Title\n\n- Change\n</proposed_plan>"
	if got != want {
		t.Fatalf("normalized plan mismatch:\ngot:  %q\nwant: %q", got, want)
	}
}

func TestNormalizeCodexProposedPlanLeavesOrdinaryTextAlone(t *testing.T) {
	raw := "I still need one product decision."
	got, ok := normalizeCodexProposedPlan(raw)
	if ok || got != raw {
		t.Fatalf("ordinary Plan-mode text changed: got=%q detected=%t", got, ok)
	}
}

func TestOpenAIToKiroContinuesPlanAfterRequestUserInputAnswer(t *testing.T) {
	req := &OpenAIRequest{
		Model: "gpt-5.6-sol",
		Messages: []OpenAIMessage{
			{Role: "developer", Content: "<collaboration_mode># Collaboration Mode: Plan</collaboration_mode>"},
			{Role: "user", Content: "Plan the player page redesign."},
			{
				Role: "assistant",
				ToolCalls: []ToolCall{{
					ID: "call_question",
					Function: struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					}{Name: "request_user_input", Arguments: `{"question":"Keep playback mode?"}`},
				}},
			},
			{Role: "tool", ToolCallID: "call_question", Content: "Playback mode is no longer required."},
		},
		Tools: []OpenAITool{testOpenAITool("request_user_input", "functions")},
	}

	if !openAIPlanModeContinuesAfterUserInput(req.Messages) {
		t.Fatal("expected request_user_input answer to be recognized as an unfinished Plan-mode continuation")
	}
	payload := OpenAIToKiro(req, false)
	if len(payload.ConversationState.History) < 2 || payload.ConversationState.History[0].UserInputMessage == nil {
		t.Fatalf("expected Plan-mode system priming, got %#v", payload.ConversationState.History)
	}
	priming := payload.ConversationState.History[0].UserInputMessage.Content
	for _, want := range []string{"<plan_mode_continuation_guard>", "Do not stop after merely acknowledging", "present the complete concrete implementation plan"} {
		if !strings.Contains(priming, want) {
			t.Fatalf("missing %q from Plan continuation prompt: %q", want, priming)
		}
	}
}

func TestOpenAIToKiroContinuesPlanAfterMidWorkflowUserCorrection(t *testing.T) {
	req := &OpenAIRequest{
		Model: "gpt-5.6-sol",
		Messages: []OpenAIMessage{
			{Role: "developer", Content: "<collaboration_mode># Collaboration Mode: Plan</collaboration_mode>"},
			{Role: "user", Content: "Plan the player page redesign."},
			{Role: "assistant", Content: "I am reviewing the current player structure and controls."},
			{Role: "user", Content: "Playback mode is no longer required."},
		},
	}

	if !openAIPlanModeContinuesAfterUserInput(req.Messages) {
		t.Fatal("expected a user correction after Plan-mode work began to continue the unfinished workflow")
	}
	payload := OpenAIToKiro(req, false)
	priming := payload.ConversationState.History[0].UserInputMessage.Content
	for _, want := range []string{"<plan_mode_continuation_guard>", "user clarification, correction, or answer", "continue the workflow"} {
		if !strings.Contains(priming, want) {
			t.Fatalf("missing %q from steered Plan continuation prompt: %q", want, priming)
		}
	}
}

func TestOpenAIToKiroLatestDefaultModeSupersedesPlanHistory(t *testing.T) {
	req := &OpenAIRequest{
		Model: "gpt-5.6-sol",
		Messages: []OpenAIMessage{
			{Role: "developer", Content: "old rules\n<collaboration_mode># Collaboration Mode: Plan\nDo not make edits.</collaboration_mode>"},
			{Role: "user", Content: "first request"},
			{Role: "assistant", Content: "Here is the plan."},
			{Role: "developer", Content: "current rules\n<collaboration_mode># Collaboration Mode: Default\nPlan mode is no longer active.</collaboration_mode>"},
			{Role: "user", Content: "The plan is approved. Implement it."},
		},
		Tools: []OpenAITool{testOpenAITool("apply_patch", "")},
	}

	if openAIRequestUsesPlanMode(req) {
		t.Fatalf("latest Default mode must supersede Plan history")
	}
	payload := OpenAIToKiro(req, false)
	if len(payload.ConversationState.History) < 2 {
		t.Fatalf("expected collaboration instructions in system priming")
	}
	got := payload.ConversationState.History[0].UserInputMessage.Content
	if strings.Contains(got, "<plan_mode_guard>") || strings.Contains(got, "Collaboration Mode: Plan") {
		t.Fatalf("stale Plan mode remained active: %q", got)
	}
	for _, want := range []string{"old rules", "Collaboration Mode: Default", "<default_mode_guard>", "may modify files"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q from Default-mode prompt: %q", want, got)
		}
	}
}

func TestOpenAIToKiroLatestPlanModeSupersedesDefaultHistory(t *testing.T) {
	req := &OpenAIRequest{
		Model: "gpt-5.6-sol",
		Messages: []OpenAIMessage{
			{Role: "developer", Content: "<collaboration_mode># Collaboration Mode: Default</collaboration_mode>"},
			{Role: "developer", Content: "<collaboration_mode># Collaboration Mode: Plan</collaboration_mode>"},
			{Role: "user", Content: "Plan this change"},
		},
	}
	if !openAIRequestUsesPlanMode(req) {
		t.Fatalf("latest Plan mode must supersede Default history")
	}
	payload := OpenAIToKiro(req, false)
	got := payload.ConversationState.History[0].UserInputMessage.Content
	if !strings.Contains(got, "<plan_mode_guard>") || strings.Contains(got, "Collaboration Mode: Default") {
		t.Fatalf("latest Plan mode was not isolated: %q", got)
	}
}

func TestCodexModeFromTextRecognizesCurrentCodexPhrases(t *testing.T) {
	for _, tc := range []struct {
		text string
		want codexCollaborationMode
	}{
		{"You are now in Plan mode.", codexModePlan},
		{"The current collaboration mode is Plan.", codexModePlan},
		{"Plan mode is active for this task.", codexModePlan},
		{"You are currently in default mode.", codexModeDefault},
		{"The current mode is: Default", codexModeDefault},
	} {
		if got := codexModeFromText(tc.text); got != tc.want {
			t.Fatalf("codexModeFromText(%q) = %q, want %q", tc.text, got, tc.want)
		}
	}
}

func TestResolveModeIgnoresLaterGenericModeDocumentation(t *testing.T) {
	messages := []OpenAIMessage{
		{Role: "developer", Content: "<collaboration_mode># Collaboration Mode: Plan\nPlan safely.</collaboration_mode>"},
		{Role: "developer", Content: "Known mode names are Default and Plan. User requests do not change modes."},
	}
	mode, index := resolveOpenAICollaborationMode(messages)
	if mode != codexModePlan || index != 0 {
		t.Fatalf("expected explicit Plan marker at index 0, got mode=%q index=%d", mode, index)
	}
}

func TestInstructionDiagnosticsDoesNotIncludePromptContent(t *testing.T) {
	messages := []OpenAIMessage{{Role: "developer", Content: "SECRET You are now in Plan mode."}}
	diagnostic := openAIInstructionDiagnostics(messages)
	if strings.Contains(diagnostic, "SECRET") {
		t.Fatalf("diagnostic leaked prompt content: %q", diagnostic)
	}
	if !strings.Contains(diagnostic, ":plan/2") {
		t.Fatalf("diagnostic omitted detected mode: %q", diagnostic)
	}
}

func TestOpenAIToKiroCoalescesConsecutiveUserMessages(t *testing.T) {
	image := KiroImage{Format: "png"}
	image.Source.Bytes = "aGVsbG8="
	req := &OpenAIRequest{
		Model: "gpt-5.6-sol",
		Messages: []OpenAIMessage{
			{Role: "user", Content: []interface{}{
				map[string]interface{}{"type": "input_text", "text": "full original requirements"},
				map[string]interface{}{"type": "input_image", "image_url": "data:image/png;base64," + image.Source.Bytes},
			}},
			{Role: "user", Content: "use Plan mode"},
		},
	}
	payload := OpenAIToKiro(req, false)
	current := payload.ConversationState.CurrentMessage.UserInputMessage
	if !strings.Contains(current.Content, "full original requirements") || !strings.Contains(current.Content, "use Plan mode") {
		t.Fatalf("consecutive user messages were not coalesced: %q", current.Content)
	}
	if len(current.Images) != 1 {
		t.Fatalf("expected original image on current task, got %d", len(current.Images))
	}
	if len(payload.ConversationState.History) != 0 {
		t.Fatalf("consecutive user messages must not become trimmable history: %#v", payload.ConversationState.History)
	}
}

func TestInitialOpenAITaskAnchorUsesLatestUserBeforeWorkStarts(t *testing.T) {
	messages := []OpenAIMessage{
		{Role: "user", Content: "workspace instructions"},
		{Role: "user", Content: "actual player redesign request"},
		{Role: "assistant", Content: "I will inspect the repository."},
		{Role: "tool", Content: "tool output"},
	}
	if got := initialOpenAITaskAnchor(messages); got != "actual player redesign request" {
		t.Fatalf("task anchor = %q", got)
	}
}

func TestStoredResponseRoundTripsToolsAndToolChoice(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	choice := json.RawMessage(`{"type":"function","name":"request_user_input"}`)
	original := &ResponsesObject{
		ID:               "resp_tools_roundtrip",
		Object:           "response",
		StoredAt:         time.Now().Unix(),
		StoredTools:      []OpenAITool{testOpenAITool("request_user_input", "functions")},
		StoredToolChoice: choice,
	}
	if err := saveResponse(original); err != nil {
		t.Fatalf("saveResponse: %v", err)
	}
	loaded, err := loadResponse(original.ID)
	if err != nil {
		t.Fatalf("loadResponse: %v", err)
	}
	if len(loaded.StoredTools) != 1 || loaded.StoredTools[0].Namespace != "functions" {
		t.Fatalf("stored tools were not preserved: %#v", loaded.StoredTools)
	}
	var gotChoice, wantChoice interface{}
	if err := json.Unmarshal(loaded.StoredToolChoice, &gotChoice); err != nil {
		t.Fatalf("decode loaded tool choice: %v", err)
	}
	if err := json.Unmarshal(choice, &wantChoice); err != nil {
		t.Fatalf("decode wanted tool choice: %v", err)
	}
	gotJSON, _ := json.Marshal(gotChoice)
	wantJSON, _ := json.Marshal(wantChoice)
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("tool choice mismatch: got %s want %s", gotJSON, wantJSON)
	}
}
