package proxy

import (
	"context"
	"encoding/json"
	"io"
	"kiro-go/config"
	accountpool "kiro-go/pool"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestResponsesParseStringInput(t *testing.T) {
	raw := json.RawMessage(`"hello world"`)
	msgs, err := parseResponsesInput(raw)
	if err != nil {
		t.Fatalf("parse string input: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	if msgs[0].Role != "user" {
		t.Fatalf("expected user role, got %q", msgs[0].Role)
	}
	if got, _ := msgs[0].Content.(string); got != "hello world" {
		t.Fatalf("expected hello world, got %v", msgs[0].Content)
	}
}

func TestResponsesParseArrayInput(t *testing.T) {
	raw := json.RawMessage(`[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"first"}]},
		{"type":"input_text","text":"loose part"},
		{"type":"function_call_output","call_id":"call_1","output":"42"}
	]`)
	msgs, err := parseResponsesInput(raw)
	if err != nil {
		t.Fatalf("parse array input: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("expected 3 messages, got %d (msgs=%+v)", len(msgs), msgs)
	}
	if msgs[0].Role != "user" {
		t.Fatalf("expected first message user, got %q", msgs[0].Role)
	}
	if got, _ := msgs[0].Content.(string); got != "first" {
		t.Fatalf("expected first text, got %v", msgs[0].Content)
	}
	if msgs[2].Role != "tool" || msgs[2].ToolCallID != "call_1" {
		t.Fatalf("expected tool result with call_id call_1, got %+v", msgs[2])
	}
	if got, _ := msgs[2].Content.(string); got != "42" {
		t.Fatalf("expected tool output 42, got %v", msgs[2].Content)
	}
}

func TestResponsesParsePreservesDeveloperRole(t *testing.T) {
	raw := json.RawMessage(`[{
		"type":"message",
		"role":"developer",
		"content":[{"type":"input_text","text":"# Collaboration Mode: Plan"}]
	}]`)
	msgs, err := parseResponsesInput(raw)
	if err != nil {
		t.Fatalf("parse developer input: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Role != "developer" {
		t.Fatalf("expected one developer message, got %#v", msgs)
	}
	if got := extractOpenAIMessageText(msgs[0].Content); got != "# Collaboration Mode: Plan" {
		t.Fatalf("developer content mismatch: %q", got)
	}
}

func TestResponsesStoreAndLoad(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}

	resp := &ResponsesObject{
		ID:        "resp_unit_test_001",
		Object:    "response",
		CreatedAt: time.Now().Unix(),
		Status:    "completed",
		Model:     "claude-sonnet-4.5",
		Output: []ResponseOutputItem{{
			ID:   "msg_x",
			Type: "message",
			Role: "assistant",
			Content: []ResponseContentPart{{
				Type: "output_text",
				Text: "stored hello",
			}},
		}},
		StoredInput: json.RawMessage(`"hi"`),
	}

	if err := saveResponse(resp); err != nil {
		t.Fatalf("save: %v", err)
	}

	loaded, err := loadResponse(resp.ID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded.ID != resp.ID || loaded.Model != resp.Model {
		t.Fatalf("loaded mismatch: %+v", loaded)
	}
	if len(loaded.Output) != 1 || loaded.Output[0].Content[0].Text != "stored hello" {
		t.Fatalf("loaded output mismatch: %+v", loaded.Output)
	}
	if string(loaded.StoredInput) != `"hi"` {
		t.Fatalf("stored input mismatch: %s", string(loaded.StoredInput))
	}

	if _, err := loadResponse("does_not_exist"); err == nil {
		t.Fatalf("expected load error for missing id")
	}
}

func TestResponsesNonStreamPreservesUpstreamIncompleteStatus(t *testing.T) {
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
			"content": "partial answer",
		}))
		_, _ = w.Write(awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{
			"stopReason": "MAX_TOKENS",
		}))
	}))
	defer server.Close()
	defer swapKiroEndpointsForTest(t, server)()

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"claude-sonnet-4.5",
		"input":"hello"
	}`))
	rec := httptest.NewRecorder()
	h.handleOpenAIResponses(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var response ResponsesObject
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v body=%s", err, rec.Body.String())
	}
	if response.Status != "incomplete" {
		t.Fatalf("status=%q, want incomplete", response.Status)
	}
	if response.IncompleteDetails == nil || response.IncompleteDetails.Reason != "max_output_tokens" {
		t.Fatalf("incomplete_details=%#v, want max_output_tokens", response.IncompleteDetails)
	}
}

func TestMapResponsesCompletion(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reason string
		status string
		detail string
	}{
		{name: "max tokens", reason: "MAX_TOKENS", status: "incomplete", detail: "max_output_tokens"},
		{name: "max output tokens alias", reason: "max_output_tokens", status: "incomplete", detail: "max_output_tokens"},
		{name: "length alias", reason: "length", status: "incomplete", detail: "max_output_tokens"},
		{name: "context limit", reason: "CONTEXT_WINDOW_EXCEEDED", status: "incomplete", detail: "max_output_tokens"},
		{name: "model context limit alias", reason: "model_context_window_exceeded", status: "incomplete", detail: "max_output_tokens"},
		{name: "content filter", reason: "CONTENT_FILTERED", status: "incomplete", detail: "content_filter"},
		{name: "refusal alias", reason: "refusal", status: "incomplete", detail: "content_filter"},
		{name: "guardrail alias", reason: "guardrail_intervened", status: "incomplete", detail: "content_filter"},
		// END_TURN is the literal the Kiro IDE bundle compares against.
		{name: "end turn", reason: "END_TURN", status: "completed"},
		// Absent metadataEvent: reported as completed here on purpose. Detecting
		// that as truncation is the stream integrity layer's job, not this map's.
		{name: "empty reports completed", reason: "", status: "completed"},
		{name: "normal completion", reason: "COMPLETE", status: "completed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, detail := mapResponsesCompletion(tc.reason)
			if status != tc.status || detail != tc.detail {
				t.Fatalf("mapResponsesCompletion(%q) = (%q, %q), want (%q, %q)", tc.reason, status, detail, tc.status, tc.detail)
			}
		})
	}
}

func TestResponsesPreviousResponseIDExpands(t *testing.T) {
	prev := &ResponsesObject{
		ID:          "resp_prev",
		StoredInput: json.RawMessage(`"earlier user"`),
		Output: []ResponseOutputItem{
			{
				Type: "message",
				Role: "assistant",
				Content: []ResponseContentPart{{
					Type: "output_text",
					Text: "earlier assistant reply",
				}},
			},
			{
				Type:      "function_call",
				CallID:    "call_prev",
				Name:      "lookup",
				Arguments: `{"q":"x"}`,
			},
		},
	}

	expanded := expandPreviousResponseHistory(prev)
	if len(expanded) != 3 {
		t.Fatalf("expected 3 messages from history, got %d (%+v)", len(expanded), expanded)
	}
	if expanded[0].Role != "user" {
		t.Fatalf("expected first message to be user, got %+v", expanded[0])
	}
	if expanded[1].Role != "assistant" {
		t.Fatalf("expected second message to be assistant, got %+v", expanded[1])
	}
	if expanded[2].Role != "assistant" || len(expanded[2].ToolCalls) != 1 {
		t.Fatalf("expected third to be assistant with tool_calls, got %+v", expanded[2])
	}
	if expanded[2].ToolCalls[0].ID != "call_prev" {
		t.Fatalf("expected tool call id call_prev, got %+v", expanded[2].ToolCalls[0])
	}
}

// A → B → C: when expanding history starting from C, all of A's and B's
// inputs/outputs must appear before C's. Previously only C's direct parent
// (B) was emitted, dropping A entirely.
func TestResponsesPreviousResponseIDExpandsFullChain(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}

	a := &ResponsesObject{
		ID:           "resp_a",
		Object:       "response",
		Status:       "completed",
		Model:        "claude-sonnet-4.5",
		StoredInput:  json.RawMessage(`"turn A user"`),
		StoredAt:     time.Now().Unix(),
		Instructions: "be terse",
		Output: []ResponseOutputItem{{
			Type: "message", Role: "assistant",
			Content: []ResponseContentPart{{Type: "output_text", Text: "turn A assistant"}},
		}},
	}
	b := &ResponsesObject{
		ID:                 "resp_b",
		Object:             "response",
		Status:             "completed",
		Model:              "claude-sonnet-4.5",
		StoredInput:        json.RawMessage(`"turn B user"`),
		StoredAt:           time.Now().Unix(),
		PreviousResponseID: a.ID,
		Output: []ResponseOutputItem{{
			Type: "message", Role: "assistant",
			Content: []ResponseContentPart{{Type: "output_text", Text: "turn B assistant"}},
		}},
	}
	if err := saveResponse(a); err != nil {
		t.Fatalf("save a: %v", err)
	}
	if err := saveResponse(b); err != nil {
		t.Fatalf("save b: %v", err)
	}

	expanded := expandPreviousResponseHistory(b)

	var transcript []string
	for _, m := range expanded {
		role := m.Role
		text, _ := m.Content.(string)
		transcript = append(transcript, role+":"+text)
	}
	got := strings.Join(transcript, "|")
	want := "system:be terse|user:turn A user|assistant:turn A assistant|user:turn B user|assistant:turn B assistant"
	if got != want {
		t.Fatalf("chain order mismatch:\n got=%s\nwant=%s", got, want)
	}
}

// New instructions sent on a continuation request must take effect, even when
// previous_response_id is set. The bug: the old code only attached
// req.Instructions when previous_response_id was empty, silently dropping
// updated system prompts on follow-up turns.
func TestResponsesContinuationKeepsNewInstructions(t *testing.T) {
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()

	prev := &ResponsesObject{
		ID:          "resp_for_continuation",
		Object:      "response",
		Status:      "completed",
		Model:       "claude-sonnet-4.5",
		StoredInput: json.RawMessage(`"first user message"`),
		StoredAt:    time.Now().Unix(),
		Output: []ResponseOutputItem{{
			Type: "message", Role: "assistant",
			Content: []ResponseContentPart{{Type: "output_text", Text: "first reply"}},
		}},
	}
	if err := saveResponse(prev); err != nil {
		t.Fatalf("save prev: %v", err)
	}

	var capturedSystem string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		capturedSystem = string(body)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
			"content": "second reply",
		}))
		_, _ = w.Write(awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{
			"stopReason": "end_turn",
		}))
	}))
	defer server.Close()
	defer swapKiroEndpointsForTest(t, server)()

	body := strings.NewReader(`{
		"model":"claude-sonnet-4.5",
		"input":"second user turn",
		"previous_response_id":"resp_for_continuation",
		"instructions":"speak only French",
		"store":false
	}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", body)
	rec := httptest.NewRecorder()
	h.handleOpenAIResponses(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(capturedSystem, "speak only French") {
		t.Fatalf("expected new instructions to reach upstream, payload=%s", capturedSystem)
	}
}

func TestResponsesContinuationRestoresToolsButNotPriorToolChoice(t *testing.T) {
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()

	tool := testOpenAITool("request_user_input", "functions")
	prev := &ResponsesObject{
		ID:                 "resp_with_plan_tools",
		Object:             "response",
		Status:             "completed",
		Model:              "gpt-5.6-sol",
		StoredInput:        json.RawMessage(`"first user message"`),
		StoredTools:        []OpenAITool{tool},
		StoredToolChoice:   json.RawMessage(`{"type":"function","name":"request_user_input","namespace":"functions"}`),
		StoredAt:           time.Now().Unix(),
		PreviousResponseID: "",
		Output: []ResponseOutputItem{{
			Type: "message", Role: "assistant",
			Content: []ResponseContentPart{{Type: "output_text", Text: "first reply"}},
		}},
	}
	if err := saveResponse(prev); err != nil {
		t.Fatalf("save prev: %v", err)
	}

	var captured map[string]interface{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Fatalf("decode upstream payload: %v", err)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
			"content": "second reply",
		}))
		_, _ = w.Write(awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{
			"stopReason": "end_turn",
		}))
	}))
	defer server.Close()
	defer swapKiroEndpointsForTest(t, server)()

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"gpt-5.6-sol",
		"input":"second user turn",
		"previous_response_id":"resp_with_plan_tools",
		"store":false
	}`))
	rec := httptest.NewRecorder()
	h.handleOpenAIResponses(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	raw, _ := json.Marshal(captured)
	upstream := string(raw)
	if !strings.Contains(upstream, `"name":"request_user_input"`) {
		t.Fatalf("expected stored plan tool in upstream payload: %s", upstream)
	}
	if strings.Contains(upstream, "must call") {
		t.Fatalf("a prior response's tool_choice must not constrain the continuation: %s", upstream)
	}
}

func TestResponsesPlanAnswerContinuesUnfinishedPlanning(t *testing.T) {
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()

	prev := &ResponsesObject{
		ID:           "resp_waiting_for_plan_answer",
		Object:       "response",
		Status:       "completed",
		Model:        "gpt-5.6-sol",
		Instructions: "<collaboration_mode># Collaboration Mode: Plan</collaboration_mode>",
		StoredInput:  json.RawMessage(`"Plan the player page redesign."`),
		StoredTools:  []OpenAITool{testOpenAITool("request_user_input", "functions")},
		StoredToolChoice: json.RawMessage(
			`{"type":"function","name":"request_user_input","namespace":"functions"}`,
		),
		StoredAt: time.Now().Unix(),
		Output: []ResponseOutputItem{{
			ID:        "fc_question",
			Type:      "function_call",
			Status:    "completed",
			CallID:    "call_question",
			Name:      "request_user_input",
			Namespace: "functions",
			Arguments: `{"question":"Keep playback mode?"}`,
		}},
	}
	if err := saveResponse(prev); err != nil {
		t.Fatalf("save prev: %v", err)
	}

	var captured map[string]interface{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Fatalf("decode upstream payload: %v", err)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
			"content": "Here is the updated implementation plan.",
		}))
		_, _ = w.Write(awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{
			"stopReason": "end_turn",
		}))
	}))
	defer server.Close()
	defer swapKiroEndpointsForTest(t, server)()

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"gpt-5.6-sol",
		"input":[{"type":"function_call_output","call_id":"call_question","output":"Playback mode is no longer required."}],
		"previous_response_id":"resp_waiting_for_plan_answer",
		"store":false
	}`))
	rec := httptest.NewRecorder()
	h.handleOpenAIResponses(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	raw, _ := json.Marshal(captured)
	upstream := string(raw)
	for _, want := range []string{
		"plan_mode_continuation_guard",
		"Do not stop after merely acknowledging",
		"Playback mode is no longer required.",
	} {
		if !strings.Contains(upstream, want) {
			t.Fatalf("missing %q from Plan answer continuation payload: %s", want, upstream)
		}
	}
	if strings.Contains(upstream, "must call") {
		t.Fatalf("prior named tool_choice leaked into the Plan answer continuation: %s", upstream)
	}
}

func setupResponsesTestHandler(t *testing.T) (*Handler, func()) {
	t.Helper()
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{
		ID:          "test-account",
		Enabled:     true,
		AccessToken: "token-test",
		ProfileArn:  "arn:aws:codewhisperer:profile/test",
	}); err != nil {
		t.Fatalf("add account: %v", err)
	}
	if err := config.UpdatePreferredEndpoint("kiro"); err != nil {
		t.Fatalf("set endpoint: %v", err)
	}
	if err := config.UpdateEndpointFallback(false); err != nil {
		t.Fatalf("disable fallback: %v", err)
	}
	p := accountpool.GetPool()
	p.Reload()
	h := &Handler{
		pool:        p,
		promptCache: newPromptCacheTracker(defaultPromptCacheTTL),
	}
	cleanup := func() {}
	return h, cleanup
}

func swapKiroEndpointsForTest(t *testing.T, server *httptest.Server) func() {
	t.Helper()
	oldEndpoints := kiroEndpoints
	kiroEndpoints = []kiroEndpoint{{
		URL:    server.URL,
		Origin: "AI_EDITOR",
		Name:   "test",
	}}
	oldClient := kiroHttpStore.Load()
	kiroHttpStore.Store(&http.Client{Timeout: time.Second, Transport: &http.Transport{}})
	return func() {
		kiroEndpoints = oldEndpoints
		kiroHttpStore.Store(oldClient)
	}
}

func TestResponsesNonStreamRoundTrip(t *testing.T) {
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
			"content": "responses non-stream OK",
		}))
		_, _ = w.Write(awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{
			"stopReason": "end_turn",
		}))
	}))
	defer server.Close()
	defer swapKiroEndpointsForTest(t, server)()

	body := strings.NewReader(`{"model":"claude-sonnet-4.5","input":"hi from test"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", body)
	req = req.WithContext(context.Background())
	rec := httptest.NewRecorder()

	h.handleOpenAIResponses(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	var resp ResponsesObject
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body.String())
	}
	if resp.Object != "response" {
		t.Fatalf("expected object=response, got %q", resp.Object)
	}
	if resp.Status != "completed" {
		t.Fatalf("expected status=completed, got %q", resp.Status)
	}
	if len(resp.Output) == 0 {
		t.Fatalf("expected output items, got none")
	}
	if resp.Output[0].Type != "message" || len(resp.Output[0].Content) == 0 {
		t.Fatalf("expected message with content, got %+v", resp.Output[0])
	}
	if resp.Output[0].Content[0].Text != "responses non-stream OK" {
		t.Fatalf("unexpected text: %q", resp.Output[0].Content[0].Text)
	}

	loaded, err := loadResponse(resp.ID)
	if err != nil {
		t.Fatalf("loadResponse: %v", err)
	}
	if loaded.ID != resp.ID {
		t.Fatalf("stored response id mismatch")
	}
}

func TestResponsesStreamPreservesUpstreamIncompleteStatus(t *testing.T) {
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
			"content": "partial answer",
		}))
		_, _ = w.Write(awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{
			"stopReason": "MAX_TOKENS",
		}))
	}))
	defer server.Close()
	defer swapKiroEndpointsForTest(t, server)()

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"claude-sonnet-4.5",
		"input":"hello",
		"stream":true,
		"store":false
	}`))
	rec := httptest.NewRecorder()
	h.handleOpenAIResponses(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"status":"incomplete"`) || !strings.Contains(rec.Body.String(), `"reason":"max_output_tokens"`) {
		t.Fatalf("expected incomplete response.completed event, got %s", rec.Body.String())
	}
}

func TestResponsesStreamSSE(t *testing.T) {
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
			"content": "stream chunk",
		}))
		_, _ = w.Write(awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{
			"stopReason": "end_turn",
		}))
	}))
	defer server.Close()
	defer swapKiroEndpointsForTest(t, server)()

	body := strings.NewReader(`{"model":"claude-sonnet-4.5","input":"stream please","stream":true,"store":false}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", body)
	rec := httptest.NewRecorder()

	h.handleOpenAIResponses(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	bodyBytes, _ := io.ReadAll(rec.Body)
	bodyStr := string(bodyBytes)

	for _, evt := range []string{"event: response.created", "event: response.output_text.delta", "event: response.completed"} {
		if !strings.Contains(bodyStr, evt) {
			t.Fatalf("missing event %q in stream body:\n%s", evt, bodyStr)
		}
	}
	if !strings.Contains(bodyStr, "stream chunk") {
		t.Fatalf("expected stream content delta, got:\n%s", bodyStr)
	}
}

func TestResponsesPlanStreamNormalizesProposedPlanBlock(t *testing.T) {
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		for _, content := range []string{
			"Plan is ready and locked.",
			"<proposed_plan>\r\n# Player redesign\n\n- Update controls\n</proposed_plan>",
		} {
			_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
				"content": content,
			}))
		}
		_, _ = w.Write(awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{
			"stopReason": "end_turn",
		}))
	}))
	defer server.Close()
	defer swapKiroEndpointsForTest(t, server)()

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"claude-sonnet-4.5",
		"instructions":"<collaboration_mode># Collaboration Mode: Plan</collaboration_mode>",
		"input":"plan the player redesign",
		"stream":true,
		"store":false
	}`))
	rec := httptest.NewRecorder()
	h.handleOpenAIResponses(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	var streamedText strings.Builder
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if !strings.HasPrefix(line, "data: ") || line == "data: [DONE]" {
			continue
		}
		var event struct {
			Type  string `json:"type"`
			Delta string `json:"delta"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
			t.Fatalf("decode SSE data line: %v line=%q", err, line)
		}
		if event.Type == "response.output_text.delta" {
			streamedText.WriteString(event.Delta)
		}
	}

	want := "<proposed_plan>\n# Player redesign\n\n- Update controls\n</proposed_plan>"
	if got := streamedText.String(); got != want {
		t.Fatalf("streamed Plan output was not normalized:\ngot:  %q\nwant: %q\nbody:\n%s", got, want, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "Plan is ready and locked.") {
		t.Fatalf("Plan preamble leaked into the client stream:\n%s", rec.Body.String())
	}
}

// TestExtractResponsesToolsAdditionalTools covers newer Codex clients
// (v0.146+) that stop sending the top-level "tools" field and instead embed
// every tool -- including Codex's own built-in "exec" custom tool that
// fronts shell/file/git access -- as a developer-role "additional_tools"
// item inside "input". Namespace-wrapped tools (e.g. sub-agent groups) must
// be flattened using their own plain name.
func TestExtractResponsesToolsAdditionalTools(t *testing.T) {
	raw := json.RawMessage(`[
		{
			"type": "additional_tools",
			"role": "developer",
			"tools": [
				{"type": "custom", "name": "exec", "description": "Run JS"},
				{"type": "function", "name": "wait", "description": "Wait", "parameters": {"type": "object", "properties": {"cell_id": {"type": "string"}}}},
				{
					"type": "namespace",
					"name": "collaboration",
					"description": "Sub-agents",
					"tools": [
						{"type": "function", "name": "followup_task", "description": "Follow up", "parameters": {"type": "object", "properties": {"target": {"type": "string"}}}}
					]
				}
			]
		},
		{"type": "message", "role": "user", "content": "hi"}
	]`)

	tools := extractResponsesTools(raw)
	if len(tools) != 3 {
		t.Fatalf("expected 3 flattened tools, got %d (%+v)", len(tools), tools)
	}

	byName := map[string]OpenAITool{}
	for _, tool := range tools {
		byName[tool.Function.Name] = tool
	}

	exec, ok := byName["exec"]
	if !ok || exec.Type != "custom" {
		t.Fatalf("expected custom exec tool, got %+v", byName)
	}
	if _, ok := byName["wait"]; !ok {
		t.Fatalf("expected wait function tool, got %+v", byName)
	}
	followup, ok := byName["followup_task"]
	if !ok || followup.Type != "function" {
		t.Fatalf("expected namespace-flattened followup_task tool with its own plain name, got %+v", byName)
	}
	if followup.Namespace != "collaboration" {
		t.Fatalf("expected followup_task to carry its source namespace for the response round trip, got %q", followup.Namespace)
	}
}

func TestResponsesTopLevelNamespaceToolsAreFlattened(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.6-sol",
		"input":"use parallel agents",
		"tools":[{
			"type":"namespace",
			"name":"collaboration",
			"description":"Coordinate sub-agents",
			"tools":[
				{"type":"function","name":"spawn_agent","description":"Spawn","parameters":{"type":"object"}},
				{"type":"function","name":"send_message","description":"Send","parameters":{"type":"object"}},
				{"type":"function","name":"wait_agent","description":"Wait","parameters":{"type":"object"}}
			]
		}]
	}`)

	var req ResponsesRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatalf("decode Responses request: %v", err)
	}
	tools := flattenResponsesTools(req.Tools)
	if len(tools) != 3 {
		t.Fatalf("expected 3 collaboration functions, got %d: %+v", len(tools), tools)
	}
	for i, want := range []string{"spawn_agent", "send_message", "wait_agent"} {
		if tools[i].Function.Name != want {
			t.Fatalf("tool[%d] name = %q, want %q", i, tools[i].Function.Name, want)
		}
		if tools[i].Namespace != "collaboration" {
			t.Fatalf("tool[%d] namespace = %q, want collaboration", i, tools[i].Namespace)
		}
	}

	wrapped := convertOpenAITools(tools)
	if len(wrapped) != 3 {
		t.Fatalf("expected all flattened collaboration tools to reach Kiro, got %d", len(wrapped))
	}
}

func TestResponsesHandlerForwardsTopLevelCollaborationFunctions(t *testing.T) {
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()

	var captured map[string]interface{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Fatalf("decode upstream payload: %v", err)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
			"content": "ready",
		}))
		_, _ = w.Write(awsEventStreamFrame(t, "metadataEvent", map[string]interface{}{
			"stopReason": "end_turn",
		}))
	}))
	defer server.Close()
	defer swapKiroEndpointsForTest(t, server)()

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"gpt-5.6-sol",
		"input":"use parallel agents",
		"store":false,
		"tools":[{
			"type":"namespace",
			"name":"collaboration",
			"tools":[
				{"type":"function","name":"spawn_agent","description":"Spawn","parameters":{"type":"object"}},
				{"type":"function","name":"wait_agent","description":"Wait","parameters":{"type":"object"}}
			]
		}]
	}`))
	rec := httptest.NewRecorder()
	h.handleOpenAIResponses(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	raw, _ := json.Marshal(captured)
	upstream := string(raw)
	for _, want := range []string{`"name":"spawn_agent"`, `"name":"wait_agent"`} {
		if !strings.Contains(upstream, want) {
			t.Fatalf("missing flattened collaboration tool %s from upstream payload: %s", want, upstream)
		}
	}
	if strings.Contains(upstream, `"name":"collaboration"`) {
		t.Fatalf("namespace wrapper leaked to Kiro instead of callable children: %s", upstream)
	}
}

// TestToolNamespaceMapAndFunctionCallOutput ensures a tool flattened out of a
// Responses API namespace (e.g. Codex's "collaboration" spawn_agent group)
// gets its namespace re-attached as a sibling "namespace" field on the
// function_call output item -- Codex's FunctionCall wire format keys
// dispatch on ToolName{name, namespace}, so a response with a bare name and
// no namespace field never matches what Codex registered internally.
func TestToolNamespaceMapAndFunctionCallOutput(t *testing.T) {
	tools := []OpenAITool{{Type: "function", Namespace: "collaboration"}}
	tools[0].Function.Name = "spawn_agent"

	ns := toolNamespaceMap(tools)
	if ns["spawn_agent"] != "collaboration" {
		t.Fatalf("expected spawn_agent namespaced under collaboration, got %+v", ns)
	}

	req := &ResponsesRequest{Tools: tools}
	toolUses := []KiroToolUse{{ToolUseID: "call_1", Name: "spawn_agent", Input: map[string]interface{}{"task_name": "worker", "message": "hi"}}}
	obj := buildResponsesObject("resp_1", "gpt-5.6-terra", "", toolUses, 0, 0, req, "tool_use")

	var fc *ResponseOutputItem
	for i := range obj.Output {
		if obj.Output[i].Type == "function_call" {
			fc = &obj.Output[i]
		}
	}
	if fc == nil {
		t.Fatalf("expected a function_call output item, got %+v", obj.Output)
	}
	if fc.Namespace != "collaboration" {
		t.Fatalf("expected function_call namespace %q, got %q", "collaboration", fc.Namespace)
	}
}

// TestConvertOpenAIToolsCustomType ensures "custom" tools (freeform string
// input, no JSON schema) get a stable single-"input"-field schema so the
// model's tool call can be round-tripped back into a custom_tool_call output
// item deterministically.
func TestConvertOpenAIToolsCustomType(t *testing.T) {
	tools := []OpenAITool{{Type: "custom"}}
	tools[0].Function.Name = "exec"
	tools[0].Function.Description = "Run JS"

	wrapped := convertOpenAITools(tools)
	if len(wrapped) != 1 {
		t.Fatalf("expected 1 wrapped tool, got %d", len(wrapped))
	}
	schema, ok := wrapped[0].ToolSpecification.InputSchema.JSON.(map[string]interface{})
	if !ok {
		t.Fatalf("expected map schema, got %T", wrapped[0].ToolSpecification.InputSchema.JSON)
	}
	props, ok := schema["properties"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected properties map in schema, got %+v", schema)
	}
	if _, ok := props["input"]; !ok {
		t.Fatalf("expected a single 'input' property, got %+v", props)
	}
}

// TestResponsesParseCustomToolCallRoundTrip covers the reverse direction:
// Codex re-submits a prior custom_tool_call/custom_tool_call_output pair on
// the next turn, which must parse into the same assistant tool_calls +
// tool-result shape as ordinary function_call items so Kiro's history
// sanitization can handle both uniformly.
func TestResponsesParseCustomToolCallRoundTrip(t *testing.T) {
	raw := json.RawMessage(`[
		{"type":"message","role":"user","content":"run something"},
		{"type":"custom_tool_call","call_id":"call_1","name":"exec","input":"console.log(1)"},
		{"type":"custom_tool_call_output","call_id":"call_1","output":"1"}
	]`)

	msgs, err := parseResponsesInput(raw)
	if err != nil {
		t.Fatalf("parse custom tool call: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("expected 3 messages, got %d (%+v)", len(msgs), msgs)
	}
	if msgs[1].Role != "assistant" || len(msgs[1].ToolCalls) != 1 {
		t.Fatalf("expected assistant tool call message, got %+v", msgs[1])
	}
	tc := msgs[1].ToolCalls[0]
	if tc.Function.Name != "exec" {
		t.Fatalf("expected exec tool call, got %q", tc.Function.Name)
	}
	var args map[string]string
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		t.Fatalf("expected JSON-wrapped arguments, got %q: %v", tc.Function.Arguments, err)
	}
	if args["input"] != "console.log(1)" {
		t.Fatalf("expected wrapped raw input, got %+v", args)
	}
	if msgs[2].Role != "tool" || msgs[2].ToolCallID != "call_1" || msgs[2].Content != "1" {
		t.Fatalf("expected tool result message, got %+v", msgs[2])
	}
}

func TestResponsesParseCustomToolOutputPreservesImageBlocks(t *testing.T) {
	raw := json.RawMessage(`[
		{"type":"message","role":"user","content":"inspect the design"},
		{"type":"custom_tool_call","call_id":"call_view","name":"view_image","input":"reference.png"},
		{"type":"custom_tool_call_output","call_id":"call_view","output":[
			{"type":"input_text","text":"Viewed the reference image."},
			{"type":"input_image","image_url":"data:image/png;base64,aGVsbG8="}
		]}
	]`)

	msgs, err := parseResponsesInput(raw)
	if err != nil {
		t.Fatalf("parse custom image tool output: %v", err)
	}
	if len(msgs) != 3 || msgs[2].Role != "tool" {
		t.Fatalf("expected tool result message, got %+v", msgs)
	}
	parts, ok := msgs[2].Content.([]interface{})
	if !ok || len(parts) != 2 {
		t.Fatalf("expected structured output parts, got %#v", msgs[2].Content)
	}

	req := &OpenAIRequest{Model: "gpt-5.6-sol", Messages: msgs}
	payload := OpenAIToKiro(req, false)
	current := payload.ConversationState.CurrentMessage.UserInputMessage
	if len(current.Images) != 1 || current.Images[0].Source.Bytes != "aGVsbG8=" {
		t.Fatalf("custom tool image was not converted to a Kiro image: %#v", current.Images)
	}
	if current.UserInputMessageContext == nil || len(current.UserInputMessageContext.ToolResults) != 1 {
		t.Fatalf("expected matching structured tool result: %#v", current.UserInputMessageContext)
	}
	text := current.UserInputMessageContext.ToolResults[0].Content[0].Text
	if text != "Viewed the reference image." {
		t.Fatalf("tool result text = %q", text)
	}
	if strings.Contains(text, "data:image") || strings.Contains(text, "aGVsbG8=") {
		t.Fatalf("image data leaked into tool-result text: %q", text)
	}
}

// TestResponsesParseAgentMessage covers Codex's multi-agent v2 delivery of a
// spawned sub-agent's initial task (and inter-agent messages) as an
// "agent_message" item rather than a plain "message". Without this parsing
// case the sub-agent silently receives no task at all -- it still runs, but
// answers as if starting a conversation with no instructions.
func TestResponsesParseAgentMessage(t *testing.T) {
	raw := json.RawMessage(`[
		{
			"type": "agent_message",
			"author": "/root",
			"recipient": "/root/worker",
			"content": [{"type": "input_text", "text": "reply with exactly: PONG"}]
		}
	]`)

	msgs, err := parseResponsesInput(raw)
	if err != nil {
		t.Fatalf("parse agent_message: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d (%+v)", len(msgs), msgs)
	}
	if msgs[0].Role != "user" {
		t.Fatalf("expected user role, got %q", msgs[0].Role)
	}
	if got, _ := msgs[0].Content.(string); got != "reply with exactly: PONG" {
		t.Fatalf("expected task text, got %v", msgs[0].Content)
	}
}

// TestResponsesParseAgentMessageReadsEncryptedContentField ensures the
// "encrypted_content" part's value is read as plain text, not skipped as
// opaque ciphertext. Codex tags multi-agent payload content this way
// unconditionally, but against a non-ChatGPT-backend provider (no
// encryption session exists) the field genuinely carries the task text
// as-is -- confirmed live against a real Codex CLI session, where a spawned
// sub-agent's "Message Type: NEW_TASK" agent_message arrived as
// {"type":"input_text","text":"Message Type: NEW_TASK\n...\nPayload:\n"}
// followed by {"type":"encrypted_content","encrypted_content":"<the actual
// plaintext task>"}.
func TestResponsesParseAgentMessageReadsEncryptedContentField(t *testing.T) {
	raw := json.RawMessage(`[
		{
			"type": "agent_message",
			"author": "/root",
			"recipient": "/root/worker",
			"content": [
				{"type": "input_text", "text": "Message Type: NEW_TASK\nTask name: /root/worker\nSender: /root\nPayload:\n"},
				{"type": "encrypted_content", "encrypted_content": "reply with exactly: PONG"}
			]
		}
	]`)

	msgs, err := parseResponsesInput(raw)
	if err != nil {
		t.Fatalf("parse agent_message: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d (%+v)", len(msgs), msgs)
	}
	got, _ := msgs[0].Content.(string)
	if !strings.Contains(got, "reply with exactly: PONG") {
		t.Fatalf("expected the task payload to be readable, got %v", got)
	}
}
