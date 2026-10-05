package service

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/tidwall/gjson"
)

const (
	// antigravityCompactionOpaquePrefix 标识本网关为 Antigravity Gemini 兼容
	// 分支生成的压缩 opaque。它只是可供本分支回放的本地封装（base64url 摘要），
	// 不是任何上游的原生密文，也不能在其他平台或账号链之间迁移。
	antigravityCompactionOpaquePrefix = "sub2api.antigravity.compaction.v1."

	// antigravityGeminiContinuationInstruction 是末尾为 model 文本时补入的
	// 续写指令。Antigravity 拒绝以 model turn 结尾的请求；补一条非空 user
	// 指令保留全部历史并要求续写，不重写、也不丢弃任何既有 parts。
	antigravityGeminiContinuationInstruction = "Continue from the previous assistant turn and complete the pending task. Do not repeat the previous assistant message verbatim."
)

// antigravityCompatRequestError 表示应在客户端 400 中回传的兼容请求错误。
type antigravityCompatRequestError struct {
	message string
}

func (e *antigravityCompatRequestError) Error() string {
	if e == nil {
		return ""
	}
	return e.message
}

func newAntigravityCompatRequestError(message string) error {
	return &antigravityCompatRequestError{message: message}
}

// prepareAntigravityGeminiResponsesCompatRequest 只在最终映射到 Gemini 的
// Antigravity Responses 兼容请求上启用压缩准备，避免影响 Antigravity Claude。
func prepareAntigravityGeminiResponsesCompatRequest(request *antigravityCompatRequest, mappedModel string) error {
	if request == nil || request.protocol != antigravityCompatResponses {
		return nil
	}
	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(mappedModel)), "gemini-") {
		return nil
	}

	prepared, err := prepareAntigravityGeminiResponsesInput(request.originalBody)
	if err != nil {
		return err
	}
	if !prepared.changed {
		return nil
	}

	var responsesRequest apicompat.ResponsesRequest
	if err := json.Unmarshal(prepared.body, &responsesRequest); err != nil {
		return newAntigravityCompatRequestError("Failed to normalize compaction input")
	}
	claudeRequest, err := apicompat.ResponsesToAnthropicRequest(&responsesRequest)
	if err != nil {
		return newAntigravityCompatRequestError("Failed to normalize compaction input")
	}
	claudeBody, err := json.Marshal(claudeRequest)
	if err != nil {
		return err
	}

	request.claudeBody = claudeBody
	request.compact = prepared.compact
	return nil
}

type antigravityGeminiResponsesPreparation struct {
	body    []byte
	changed bool
	compact bool
}

// prepareAntigravityGeminiResponsesInput 在不改动共享 Responses 转换器的前提下
// 预处理原生压缩输入：
//   - 本分支生成的 compaction opaque 还原为 <conversation_summary> user 消息；
//   - 原生 compaction_trigger 被移除，并追加既有摘要指令（grokCompactSummaryPrompt）；
//   - 其他来源或损坏的 compaction 输入显式报错，绝不静默丢弃上下文。
//
// 返回的 body 只在确有改写时变化，普通请求保持字节原样。
func prepareAntigravityGeminiResponsesInput(body []byte) (antigravityGeminiResponsesPreparation, error) {
	prep := antigravityGeminiResponsesPreparation{body: body}
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return prep, nil
	}

	// 复用已有的 compaction_trigger 识别 helper，避免与本文件各自维护一套判定。
	compact := HasCompactionTriggerInInput(body)
	hasCompactionItem := false
	input.ForEach(func(_, item gjson.Result) bool {
		if !item.IsObject() {
			return true
		}
		if isOpenAICompactionType(strings.TrimSpace(item.Get("type").String())) {
			hasCompactionItem = true
			return false
		}
		return true
	})
	if !compact && !hasCompactionItem {
		return prep, nil
	}

	var payload map[string]any
	if err := decodeOpenAIJSONUseNumber(body, &payload); err != nil {
		return prep, newAntigravityCompatRequestError("Failed to parse compaction input")
	}
	items, ok := payload["input"].([]any)
	if !ok {
		return prep, nil
	}

	converted := make([]any, 0, len(items)+1)
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			converted = append(converted, raw)
			continue
		}
		switch strings.TrimSpace(stringValue(item["type"])) {
		case "compaction_trigger":
			compact = true
			continue
		case "compaction", "compaction_summary":
			summary, err := decodeAntigravityCompactionOpaque(stringValue(item["encrypted_content"]))
			if err != nil {
				return prep, err
			}
			converted = append(converted, antigravityConversationSummaryInputItem(summary))
			continue
		}
		converted = append(converted, raw)
	}
	if compact {
		// 复用 Grok 压缩桥的既有摘要指令，保持压缩意图真实、可回放。
		converted = append(converted, antigravitySummaryPromptInputItem())
	}

	payload["input"] = converted
	encoded, err := marshalOpenAIUpstreamJSON(payload)
	if err != nil {
		return prep, newAntigravityCompatRequestError("Failed to normalize compaction input")
	}
	prep.body = encoded
	prep.changed = true
	prep.compact = compact
	return prep, nil
}

func antigravitySummaryPromptInputItem() map[string]any {
	return map[string]any{
		"type": "message",
		"role": "user",
		"content": []any{map[string]any{
			"type": "input_text",
			"text": grokCompactSummaryPrompt,
		}},
	}
}

func antigravityConversationSummaryInputItem(summary string) map[string]any {
	return map[string]any{
		"type": "message",
		"role": "user",
		"content": []any{map[string]any{
			"type": "input_text",
			"text": "<conversation_summary>\n" + summary + "\n</conversation_summary>",
		}},
	}
}

// encodeAntigravityCompactionOpaque 把摘要包装成本分支可回放的本地 opaque。
func encodeAntigravityCompactionOpaque(summary string) string {
	return antigravityCompactionOpaquePrefix + base64.RawURLEncoding.EncodeToString([]byte(strings.TrimSpace(summary)))
}

// decodeAntigravityCompactionOpaque 只接受本分支生成的格式；未知来源、损坏或
// 空摘要一律显式拒绝，避免把无法恢复的上下文当成功继续。
func decodeAntigravityCompactionOpaque(value string) (string, error) {
	trimmed := strings.TrimSpace(value)
	if !strings.HasPrefix(trimmed, antigravityCompactionOpaquePrefix) {
		return "", newAntigravityCompatRequestError("compaction input item is not a local Antigravity summary and cannot be replayed")
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(trimmed, antigravityCompactionOpaquePrefix))
	if err != nil || !utf8.Valid(raw) {
		return "", newAntigravityCompatRequestError("compaction input item carries a damaged local summary and cannot be replayed")
	}
	summary := strings.TrimSpace(string(raw))
	if summary == "" {
		return "", newAntigravityCompatRequestError("compaction input item carries an empty local summary and cannot be replayed")
	}
	return summary, nil
}

// antigravityGeminiTailInfo 只承载结构性诊断字段，不含任何提示词或工具输出。
type antigravityGeminiTailInfo struct {
	Messages  int
	LastRole  string
	LastParts string
	Repaired  bool
}

// repairAntigravityGeminiContentsTail 在转换与清理之后、wrap 之前检查最终有效
// contents：有效 user 结尾原样保留；末尾为 model 且含 functionCall 时显式拒绝
// （不把未回传的调用伪装成普通续写）；末尾为纯可见文本 model（允许附着的合法
// thoughtSignature）时保留全部历史并补一条非空 user 续写指令；model 尾含
// thought-only/inlineData/code/functionResponse 或任何未知 part 时显式拒绝，
// 不丢弃任何内容。函数只追加尾部条目，不触碰既有 parts。
func repairAntigravityGeminiContentsTail(body []byte) ([]byte, antigravityGeminiTailInfo, error) {
	var info antigravityGeminiTailInfo
	var payload map[string]any
	if err := decodeOpenAIJSONUseNumber(body, &payload); err != nil {
		return body, info, err
	}
	contents, ok := payload["contents"].([]any)
	if !ok {
		return body, info, nil
	}
	info.Messages = len(contents)

	last := -1
	for index, raw := range contents {
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		parts, ok := entry["parts"].([]any)
		if !ok || len(parts) == 0 {
			continue
		}
		last = index
	}
	if last < 0 {
		return body, info, nil
	}

	entry, _ := contents[last].(map[string]any)
	info.LastRole = strings.TrimSpace(stringValue(entry["role"]))
	parts, _ := entry["parts"].([]any)
	info.LastParts = antigravityGeminiPartKinds(parts)
	if info.LastRole != "model" {
		return body, info, nil
	}
	switch classifyAntigravityGeminiModelTail(parts) {
	case antigravityGeminiTailFunctionCall:
		return body, info, newAntigravityCompatRequestError("The conversation ends with an unanswered tool call; Antigravity Gemini cannot continue it")
	case antigravityGeminiTailPlainText:
		// 继续追加续写指令。
	default:
		return body, info, newAntigravityCompatRequestError("The conversation ends with a model turn that is not plain text; Antigravity Gemini cannot safely continue it")
	}

	// 补入续写指令；既有 parts 原样保留。`last` 之后只可能是缺失/空 parts 的
	// 尾部条目（不含任何文本、工具结果或用户内容，且会被上游拒收），仅剔除它们。
	repaired := make([]any, 0, last+2)
	repaired = append(repaired, contents[:last+1]...)
	repaired = append(repaired, map[string]any{
		"role":  "user",
		"parts": []any{map[string]any{"text": antigravityGeminiContinuationInstruction}},
	})
	payload["contents"] = repaired
	encoded, err := marshalOpenAIUpstreamJSON(payload)
	if err != nil {
		return body, info, err
	}
	info.Repaired = true
	return encoded, info, nil
}

// antigravityGeminiModelTailClass 描述末尾 model 回合的形态。
type antigravityGeminiModelTailClass uint8

const (
	// antigravityGeminiTailUnsupported：不是可安全续写的纯可见文本（含
	// thought-only、inlineData、code、functionResponse、未知 part 或空文本）。
	antigravityGeminiTailUnsupported antigravityGeminiModelTailClass = iota
	// antigravityGeminiTailPlainText：每个 part 均为带非空可见文本的 text part，
	// 允许附着合法的非空 thoughtSignature。
	antigravityGeminiTailPlainText
	// antigravityGeminiTailFunctionCall：含未回传的 functionCall。
	antigravityGeminiTailFunctionCall
)

// classifyAntigravityGeminiModelTail 只在末尾 model 回合严格为"带可见文本的
// text parts（可带 signature）"时允许补续写指令；其余形态一律显式拒绝，绝不
// 猜测、改写或丢弃内容。
func classifyAntigravityGeminiModelTail(parts []any) antigravityGeminiModelTailClass {
	if len(parts) == 0 {
		return antigravityGeminiTailUnsupported
	}
	sawVisibleText := false
	for _, raw := range parts {
		part, ok := raw.(map[string]any)
		if !ok {
			return antigravityGeminiTailUnsupported
		}
		if _, hasCall := part["functionCall"]; hasCall {
			return antigravityGeminiTailFunctionCall
		}
		if thought, ok := part["thought"].(bool); ok && thought {
			return antigravityGeminiTailUnsupported
		}
		for key := range part {
			switch key {
			case "text", "thoughtSignature":
			default:
				return antigravityGeminiTailUnsupported
			}
		}
		if signature, hasSignature := part["thoughtSignature"]; hasSignature {
			text, ok := signature.(string)
			if !ok || strings.TrimSpace(text) == "" {
				return antigravityGeminiTailUnsupported
			}
		}
		text, ok := part["text"].(string)
		if !ok || strings.TrimSpace(text) == "" {
			return antigravityGeminiTailUnsupported
		}
		sawVisibleText = true
	}
	if !sawVisibleText {
		return antigravityGeminiTailUnsupported
	}
	return antigravityGeminiTailPlainText
}

func antigravityGeminiPartKinds(parts []any) string {
	kinds := make([]string, 0, len(parts))
	for _, raw := range parts {
		part, ok := raw.(map[string]any)
		if !ok {
			kinds = append(kinds, "unknown")
			continue
		}
		switch {
		case part["text"] != nil:
			kinds = append(kinds, "text")
		case part["functionCall"] != nil:
			kinds = append(kinds, "functionCall")
		case part["functionResponse"] != nil:
			kinds = append(kinds, "functionResponse")
		case part["inlineData"] != nil:
			kinds = append(kinds, "inlineData")
		case part["executableCode"] != nil:
			kinds = append(kinds, "executableCode")
		case part["codeExecutionResult"] != nil:
			kinds = append(kinds, "codeExecutionResult")
		case part["thoughtSignature"] != nil:
			kinds = append(kinds, "thoughtSignature")
		default:
			kinds = append(kinds, "other")
		}
	}
	return strings.Join(kinds, ",")
}

// applyAntigravityGeminiNoToolCalling 保留工具声明与历史，仅把最终的
// functionCallingConfig.mode 收敛为 NONE，阻断压缩回合产生新的工具调用。
func applyAntigravityGeminiNoToolCalling(body []byte) ([]byte, error) {
	var payload map[string]any
	if err := decodeOpenAIJSONUseNumber(body, &payload); err != nil {
		return body, err
	}
	toolConfig, _ := payload["toolConfig"].(map[string]any)
	if toolConfig == nil {
		toolConfig = map[string]any{}
	}
	callingConfig, _ := toolConfig["functionCallingConfig"].(map[string]any)
	if callingConfig == nil {
		callingConfig = map[string]any{}
	}
	callingConfig["mode"] = "NONE"
	delete(callingConfig, "allowedFunctionNames")
	delete(callingConfig, "allowed_function_names")
	toolConfig["functionCallingConfig"] = callingConfig
	payload["toolConfig"] = toolConfig
	return marshalOpenAIUpstreamJSON(payload)
}

func logAntigravityCompatGeminiTail(info antigravityGeminiTailInfo, compact bool) {
	if !compact && !info.Repaired {
		return
	}
	logger.LegacyPrintf(
		"service.antigravity_gateway",
		"Antigravity Gemini tail check: messages=%d last_role=%s last_parts=%s compact=%t repaired=%t",
		info.Messages,
		info.LastRole,
		info.LastParts,
		compact,
		info.Repaired,
	)
}
