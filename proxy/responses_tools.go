package proxy

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

const codexPlanModeReinforcement = `<plan_mode_guard>
You are in Codex Plan mode. Do not modify files, run mutating commands, or implement the requested change. Inspect read-only state as needed, use request_user_input when a user decision is required, and return a concrete proposed implementation plan for the user to review. A user clarification, correction, or answer supplied while planning is in progress is an intermediate continuation of the same unfinished planning workflow, not permission to end with a brief acknowledgement. Incorporate it and continue planning until you either need another user decision or can present the complete plan. This restriction remains active until a later developer instruction explicitly changes the collaboration mode.
</plan_mode_guard>`

const codexPlanToolResultContinuation = `<plan_mode_continuation_guard>
The latest input is a user clarification, correction, or answer supplied after Plan-mode work had already begun. Incorporate it and continue the workflow in this response. Do not stop after merely acknowledging, restating, accepting, or removing the updated requirement. Continue read-only investigation if needed, ask the next necessary question, or present the complete concrete implementation plan.
</plan_mode_continuation_guard>`

const codexDefaultModeReinforcement = `<default_mode_guard>
The current Codex collaboration mode is Default. Any Plan-mode restriction found in older conversation history is superseded and no longer active. You may modify files and run repository-changing tools when the current user request authorizes that work.
</default_mode_guard>`

type codexCollaborationMode string

const (
	codexModeUnknown codexCollaborationMode = ""
	codexModePlan    codexCollaborationMode = "plan"
	codexModeDefault codexCollaborationMode = "default"
)

var collaborationModeBlockPattern = regexp.MustCompile(`(?is)<collaboration_mode>.*?</collaboration_mode>`)

type codexModeSignal struct {
	mode     codexCollaborationMode
	position int
	strength int
}

var codexModePatterns = []struct {
	mode     codexCollaborationMode
	strength int
	pattern  *regexp.Regexp
}{
	{codexModePlan, 3, regexp.MustCompile(`(?is)<collaboration_mode>\s*(?:#\s*)?collaboration\s+mode\s*:\s*plan\b`)},
	{codexModeDefault, 3, regexp.MustCompile(`(?is)<collaboration_mode>\s*(?:#\s*)?collaboration\s+mode\s*:\s*default\b`)},
	{codexModePlan, 3, regexp.MustCompile(`(?is)<collaboration_mode>\s*plan\s*</collaboration_mode>`)},
	{codexModeDefault, 3, regexp.MustCompile(`(?is)<collaboration_mode>\s*default\s*</collaboration_mode>`)},
	{codexModePlan, 2, regexp.MustCompile(`(?i)\bcollaboration\s+mode\s*:\s*plan\b`)},
	{codexModeDefault, 2, regexp.MustCompile(`(?i)\bcollaboration\s+mode\s*:\s*default\b`)},
	{codexModePlan, 2, regexp.MustCompile(`(?i)\byou\s+are\s+(?:now|currently)\s+in\s+plan\s+mode\b`)},
	{codexModeDefault, 2, regexp.MustCompile(`(?i)\byou\s+are\s+(?:now|currently)\s+in\s+default\s+mode\b`)},
	{codexModePlan, 2, regexp.MustCompile(`(?i)\b(?:the\s+)?current\s+(?:collaboration\s+)?mode\s*(?:is|:)\s*:?[ \t]*plan\b`)},
	{codexModeDefault, 2, regexp.MustCompile(`(?i)\b(?:the\s+)?current\s+(?:collaboration\s+)?mode\s*(?:is|:)\s*:?[ \t]*default\b`)},
	{codexModePlan, 1, regexp.MustCompile(`(?i)\bplan\s+mode\s+is\s+(?:now\s+)?active\b`)},
	{codexModeDefault, 1, regexp.MustCompile(`(?i)\bdefault\s+mode\s+is\s+(?:now\s+)?active\b`)},
}

func strongestCodexModeSignal(prompt string) codexModeSignal {
	best := codexModeSignal{position: -1}
	for _, candidate := range codexModePatterns {
		matches := candidate.pattern.FindAllStringIndex(prompt, -1)
		for _, match := range matches {
			signal := codexModeSignal{mode: candidate.mode, position: match[0], strength: candidate.strength}
			if signal.strength > best.strength ||
				(signal.strength == best.strength && signal.position > best.position) {
				best = signal
			}
		}
	}
	return best
}

func codexModeFromText(prompt string) codexCollaborationMode {
	return strongestCodexModeSignal(prompt).mode
}

func isCodexPlanModePrompt(prompt string) bool {
	return codexModeFromText(prompt) == codexModePlan
}

func resolveOpenAICollaborationMode(messages []OpenAIMessage) (codexCollaborationMode, int) {
	best := codexModeSignal{position: -1}
	latestIndex := -1
	for i, message := range messages {
		if message.Role != "system" && message.Role != "developer" {
			continue
		}
		candidate := strongestCodexModeSignal(extractOpenAIMessageText(message.Content))
		if candidate.mode == codexModeUnknown {
			continue
		}
		if candidate.strength > best.strength ||
			(candidate.strength == best.strength && i >= latestIndex) {
			best = candidate
			latestIndex = i
		}
	}
	return best.mode, latestIndex
}

func stripCodexCollaborationModeBlocks(prompt string) string {
	return strings.TrimSpace(collaborationModeBlockPattern.ReplaceAllString(prompt, ""))
}

func countOpenAIRoles(messages []OpenAIMessage) map[string]int {
	counts := make(map[string]int)
	for _, message := range messages {
		counts[strings.ToLower(strings.TrimSpace(message.Role))]++
	}
	return counts
}

func openAIRequestUsesPlanMode(req *OpenAIRequest) bool {
	if req == nil {
		return false
	}
	mode, _ := resolveOpenAICollaborationMode(req.Messages)
	return mode == codexModePlan
}

func openAIPlanModeContinuesAfterUserInput(messages []OpenAIMessage) bool {
	last := len(messages) - 1
	for last >= 0 && (messages[last].Role == "system" || messages[last].Role == "developer") {
		last--
	}
	if last < 0 {
		return false
	}
	if messages[last].Role == "user" {
		for i := last - 1; i >= 0; i-- {
			if messages[i].Role == "assistant" {
				return true
			}
		}
		return false
	}
	if messages[last].Role != "tool" {
		return false
	}

	callID := strings.TrimSpace(messages[last].ToolCallID)
	if callID == "" {
		return false
	}
	for i := last - 1; i >= 0; i-- {
		if messages[i].Role != "assistant" {
			continue
		}
		for _, call := range messages[i].ToolCalls {
			if call.ID == callID {
				return strings.EqualFold(strings.TrimSpace(call.Function.Name), "request_user_input")
			}
		}
	}
	return false
}

func openAIInstructionDiagnostics(messages []OpenAIMessage) string {
	entries := make([]string, 0)
	for i, message := range messages {
		role := strings.ToLower(strings.TrimSpace(message.Role))
		if role != "system" && role != "developer" {
			continue
		}
		content := extractOpenAIMessageText(message.Content)
		hash := sha256.Sum256([]byte(content))
		signal := strongestCodexModeSignal(content)
		entries = append(entries, fmt.Sprintf("%d:%s:%d:%x:%s/%d",
			i, role, len(content), hash[:4], signal.mode, signal.strength))
	}
	return strings.Join(entries, ",")
}

// mergeOpenAITools combines tool declarations while keeping the first
// declaration for each Responses identity. Codex may send the same tool both
// at the top level and inside an additional_tools input item.
func mergeOpenAITools(groups ...[]OpenAITool) []OpenAITool {
	var merged []OpenAITool
	seen := make(map[string]bool)
	for _, tools := range groups {
		for _, tool := range tools {
			name := strings.TrimSpace(tool.Function.Name)
			if name == "" {
				continue
			}
			key := strings.ToLower(strings.TrimSpace(tool.Namespace)) + "\x00" +
				strings.ToLower(strings.TrimSpace(tool.Type)) + "\x00" + strings.ToLower(name)
			if seen[key] {
				continue
			}
			seen[key] = true
			merged = append(merged, tool)
		}
	}
	return merged
}

func containsSystemInstruction(messages []OpenAIMessage, instruction string) bool {
	want := strings.TrimSpace(instruction)
	if want == "" {
		return true
	}
	for _, message := range messages {
		if message.Role == "system" && strings.TrimSpace(extractOpenAIMessageText(message.Content)) == want {
			return true
		}
	}
	return false
}

func decodeToolChoice(raw json.RawMessage) interface{} {
	if len(raw) == 0 {
		return nil
	}
	var choice interface{}
	if err := json.Unmarshal(raw, &choice); err != nil {
		return nil
	}
	return choice
}

func openAIToolNames(tools []OpenAITool) []string {
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		name := tool.Function.Name
		if tool.Namespace != "" {
			name = tool.Namespace + "." + name
		}
		names = append(names, name)
	}
	return names
}

func summarizeToolChoice(choice interface{}) string {
	switch value := choice.(type) {
	case nil:
		return "auto"
	case string:
		return value
	case map[string]interface{}:
		kind, _ := value["type"].(string)
		name, _ := value["name"].(string)
		if function, ok := value["function"].(map[string]interface{}); ok && name == "" {
			name, _ = function["name"].(string)
		}
		if namespace, _ := value["namespace"].(string); namespace != "" {
			name = namespace + "." + name
		}
		if name != "" {
			return kind + ":" + name
		}
		return kind
	default:
		return fmt.Sprint(value)
	}
}

// applyOpenAIToolChoice emulates OpenAI tool_choice on Kiro, whose request
// schema has no native tool-choice field. "none" removes tools; a named choice
// exposes only that tool; required/named choices also receive a short system
// instruction so the model does not answer in prose instead of calling it.
func applyOpenAIToolChoice(tools []OpenAITool, choice interface{}) ([]OpenAITool, string) {
	if choice == nil {
		return tools, ""
	}
	if mode, ok := choice.(string); ok {
		switch strings.ToLower(strings.TrimSpace(mode)) {
		case "none":
			return nil, ""
		case "required", "any":
			if len(tools) == 0 {
				return tools, ""
			}
			return tools, "You must call one of the available tools before completing this response. Do not answer with prose instead of the tool call."
		default:
			return tools, ""
		}
	}

	obj, ok := choice.(map[string]interface{})
	if !ok {
		return tools, ""
	}
	name, _ := obj["name"].(string)
	if function, ok := obj["function"].(map[string]interface{}); ok && name == "" {
		name, _ = function["name"].(string)
	}
	namespace, _ := obj["namespace"].(string)
	name = strings.TrimSpace(name)
	if name == "" {
		return tools, ""
	}

	selected := make([]OpenAITool, 0, 1)
	for _, tool := range tools {
		if tool.Function.Name == name && (namespace == "" || tool.Namespace == namespace) {
			selected = append(selected, tool)
		}
	}
	if len(selected) == 0 {
		return tools, ""
	}
	displayName := name
	if namespace != "" {
		displayName = namespace + "." + name
	}
	return selected, fmt.Sprintf("You must call the available tool %q before completing this response. Do not answer with prose instead of the tool call.", displayName)
}
