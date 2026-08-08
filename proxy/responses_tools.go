package proxy

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

const codexPlanModeReinforcement = `<plan_mode_guard>
You are in Codex Plan mode. Do not modify files, run mutating commands, or implement the requested change. Inspect read-only state as needed, use request_user_input when a user decision is required, and return a concrete proposed implementation plan for the user to review. This restriction remains active until a later developer instruction explicitly changes the collaboration mode.
</plan_mode_guard>`

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

func codexModeFromText(prompt string) codexCollaborationMode {
	lower := strings.ToLower(prompt)
	planAt := strings.LastIndex(lower, "collaboration mode: plan")
	defaultAt := strings.LastIndex(lower, "collaboration mode: default")
	if direct := strings.LastIndex(lower, "<collaboration_mode>plan</collaboration_mode>"); direct > planAt {
		planAt = direct
	}
	if direct := strings.LastIndex(lower, "<collaboration_mode>default</collaboration_mode>"); direct > defaultAt {
		defaultAt = direct
	}
	switch {
	case planAt < 0 && defaultAt < 0:
		return codexModeUnknown
	case defaultAt > planAt:
		return codexModeDefault
	default:
		return codexModePlan
	}
}

func isCodexPlanModePrompt(prompt string) bool {
	return codexModeFromText(prompt) == codexModePlan
}

func resolveOpenAICollaborationMode(messages []OpenAIMessage) (codexCollaborationMode, int) {
	mode := codexModeUnknown
	latestIndex := -1
	for i, message := range messages {
		if message.Role != "system" && message.Role != "developer" {
			continue
		}
		if candidate := codexModeFromText(extractOpenAIMessageText(message.Content)); candidate != codexModeUnknown {
			mode = candidate
			latestIndex = i
		}
	}
	return mode, latestIndex
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
