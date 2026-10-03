package apicompat

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

// ResponsesCollaborationError describes a deterministic client payload error.
// Messages contain field locations and guidance, never task content.
type ResponsesCollaborationError struct {
	Code    string
	Param   string
	Message string
}

func (e *ResponsesCollaborationError) Error() string {
	return fmt.Sprintf("%s: %s (%s)", e.Code, e.Message, e.Param)
}

// NormalizeResponsesCollaboration prepares Codex V2 collaboration for plaintext
// transport. Call before namespace flattening or tool aliasing. Other encrypted
// items (including reasoning and compaction) are outside this conversion.
func NormalizeResponsesCollaboration(req map[string]any) (bool, error) {
	lists := responsesCollaborationToolLists(req)
	// Use the existing identity mapping for flat declarations; never infer a
	// namespace by splitting a tool name.
	var namespaces []ResponsesTool
	for _, list := range lists {
		for _, raw := range list {
			tool, ok := raw.(map[string]any)
			if !ok || stringValue(tool["type"]) != "namespace" {
				continue
			}
			namespace := ResponsesTool{Type: "namespace", Name: stringValue(tool["name"])}
			for _, field := range []string{"tools", "children"} {
				children, _ := tool[field].([]any)
				for _, rawChild := range children {
					child, ok := rawChild.(map[string]any)
					if ok {
						namespace.Tools = append(namespace.Tools, ResponsesTool{
							Type: stringValue(child["type"]), Name: stringValue(child["name"]),
						})
					}
				}
			}
			namespaces = append(namespaces, namespace)
		}
	}
	names := NamespaceToolNames(namespaces)
	changed := false
	for _, list := range lists {
		if normalizeResponsesCollaborationTools(list, "", names) {
			changed = true
		}
	}
	input, _ := req["input"].([]any)
	for i, raw := range input {
		item, ok := raw.(map[string]any)
		if !ok || stringValue(item["type"]) != "agent_message" {
			continue
		}
		param := fmt.Sprintf("input[%d].content", i)
		text, err := responsesCollaborationMessageText(item["content"], param)
		if err != nil {
			return false, err
		}
		// Preserve explicit routing metadata without granting system/developer
		// authority to the sender or changing the order of content parts.
		var envelope strings.Builder
		for _, field := range []struct{ key, label string }{
			{"author", "Sender"}, {"recipient", "Recipient"},
		} {
			value := stringValue(item[field.key])
			if value == "" && item[field.key] != nil {
				if encoded, err := json.Marshal(item[field.key]); err == nil {
					value = string(encoded)
				}
			}
			if value != "" &&
				!strings.HasPrefix(text, field.label+":") &&
				!strings.Contains(text, "\n"+field.label+":") {
				fmt.Fprintf(&envelope, "%s: %s\n", field.label, value)
			}
		}
		input[i] = map[string]any{
			"type": "message", "role": "user",
			"content": []any{map[string]any{
				"type": "input_text", "text": envelope.String() + text,
			}},
		}
		changed = true
	}
	return changed, nil
}

func responsesCollaborationToolLists(req map[string]any) [][]any {
	var lists [][]any
	if tools, ok := req["tools"].([]any); ok {
		lists = append(lists, tools)
	}
	input, _ := req["input"].([]any)
	for _, raw := range input {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		typ := stringValue(item["type"])
		if typ == "additional_tools" || (typ == "tool_search_output" && usableResponsesToolSearchOutput(item)) {
			if tools, ok := item["tools"].([]any); ok {
				lists = append(lists, tools)
			}
		}
		if tools, ok := item["additional_tools"].([]any); ok {
			lists = append(lists, tools)
		}
	}
	return lists
}

func normalizeResponsesCollaborationTools(tools []any, namespace string, names map[string]NamespacedToolName) bool {
	changed := false
	for _, raw := range tools {
		tool, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		typ := stringValue(tool["type"])
		if typ == "namespace" {
			for _, field := range []string{"tools", "children"} {
				children, _ := tool[field].([]any)
				if normalizeResponsesCollaborationTools(children, stringValue(tool["name"]), names) {
					changed = true
				}
			}
			continue
		}
		if typ != "function" {
			continue
		}
		owner, name := namespace, stringValue(tool["name"])
		if explicit := stringValue(tool["namespace"]); explicit != "" {
			owner = explicit
		}
		if owner == "" {
			if identity, ok := names[name]; ok {
				owner, name = identity.Namespace, identity.Name
			}
		}
		if owner != "collaboration" {
			continue
		}
		switch name {
		case "spawn_agent", "send_message", "followup_task":
			parameters, _ := tool["parameters"].(map[string]any)
			properties, _ := parameters["properties"].(map[string]any)
			message, _ := properties["message"].(map[string]any)
			if _, exists := message["encrypted"]; exists {
				delete(message, "encrypted")
				changed = true
			}
		}
	}
	return changed
}

func responsesCollaborationMessageText(content any, param string) (string, error) {
	invalid := func(location string) error {
		return &ResponsesCollaborationError{
			Code: "codex_agent_message_invalid", Param: location,
			Message: "agent_message requires a non-empty plaintext task and valid text content",
		}
	}
	encrypted := func(location string) error {
		return &ResponsesCollaborationError{
			Code: "codex_agent_message_encrypted_unsupported", Param: location,
			Message: "encrypted agent messages cannot be recovered; start a new parent session and resend a plaintext task",
		}
	}
	validate := func(value any, location string) error {
		text, ok := value.(string)
		if !ok {
			return invalid(location)
		}
		if responsesCollaborationFernetPayload(text) {
			return encrypted(location)
		}
		return nil
	}
	switch value := content.(type) {
	case string:
		if err := validate(value, param); err != nil {
			return "", err
		}
	case []any:
		for i, raw := range value {
			location := fmt.Sprintf("%s[%d]", param, i)
			part, ok := raw.(map[string]any)
			if !ok {
				return "", invalid(location)
			}
			switch stringValue(part["type"]) {
			case "input_text", "text":
				if err := validate(part["text"], location+".text"); err != nil {
					return "", err
				}
			case "encrypted_content":
				if _, opaque := part["encrypted_content"].(map[string]any); opaque {
					return "", encrypted(location + ".encrypted_content")
				}
				if err := validate(part["encrypted_content"], location+".encrypted_content"); err != nil {
					return "", err
				}
			case "opaque":
				return "", encrypted(location)
			default:
				return "", invalid(location)
			}
		}
	default:
		return "", invalid(param)
	}
	raw, err := json.Marshal(content)
	if err != nil {
		return "", invalid(param)
	}
	// Keep the Chat bridge's text ordering contract in one place.
	text := agentMessageText(raw)
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return "", invalid(param)
	}
	if responsesCollaborationFernetPayload(trimmed) {
		return "", encrypted(param)
	}
	if strings.HasPrefix(trimmed, "Message Type:") {
		lines := strings.Split(trimmed, "\n")
		hasPayload := false
		for i, line := range lines {
			line = strings.TrimSpace(line)
			if payload, found := strings.CutPrefix(line, "Payload:"); found {
				payload = strings.TrimSpace(payload + "\n" + strings.Join(lines[i+1:], "\n"))
				if responsesCollaborationFernetPayload(payload) {
					return "", encrypted(param)
				}
				hasPayload = payload != ""
				break
			}
			if line == "" || strings.HasPrefix(line, "Message Type:") ||
				strings.HasPrefix(line, "Task name:") || strings.HasPrefix(line, "Sender:") ||
				strings.HasPrefix(line, "Recipient:") {
				continue
			}
			hasPayload = true
			break
		}
		if !hasPayload {
			return "", invalid(param)
		}
	}
	return text, nil
}

// Recognize only a structurally plausible Fernet token, not arbitrary Base64.
// This check does not authenticate the token or attempt to decrypt it.
func responsesCollaborationFernetPayload(text string) bool {
	token := strings.TrimRight(strings.TrimSpace(text), "=")
	raw, err := base64.RawURLEncoding.DecodeString(token)
	return err == nil && len(raw) >= 73 && raw[0] == 0x80 && (len(raw)-57)%16 == 0
}
