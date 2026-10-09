package apicompat

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Non-streaming: ResponsesResponse → ChatCompletionsResponse
// ---------------------------------------------------------------------------

// ResponsesToChatCompletions converts a Responses API response into a Chat
// Completions response. Text output items are concatenated into
// choices[0].message.content, refusal parts into choices[0].message.refusal;
// function_call items become tool_calls.
func ResponsesToChatCompletions(resp *ResponsesResponse, model string) *ChatCompletionsResponse {
	return ResponsesToChatCompletionsWithOptions(resp, model, nil)
}

// ResponsesToChatConversionOptions carries provider-scoped return policies.
// A nil value behaves exactly like the historical conversion.
type ResponsesToChatConversionOptions struct {
	// PreferReasoningContentText reads a reasoning item's full reasoning_text
	// content parts instead of its summary. DeepSeek's native Responses
	// endpoint requires the complete thinking text on replay, and its summary
	// is only a condensed form. Other providers keep the summary behavior.
	PreferReasoningContentText bool
}

// ResponsesToChatCompletionsWithOptions is ResponsesToChatCompletions with
// provider-scoped policies (see ResponsesToChatConversionOptions).
func ResponsesToChatCompletionsWithOptions(resp *ResponsesResponse, model string, opts *ResponsesToChatConversionOptions) *ChatCompletionsResponse {
	id := resp.ID
	if id == "" {
		id = generateChatCmplID()
	}

	out := &ChatCompletionsResponse{
		ID:          id,
		Object:      "chat.completion",
		Created:     time.Now().Unix(),
		Model:       model,
		ServiceTier: resp.ServiceTier,
	}

	var contentText string
	var refusalText string
	var reasoningText string
	var toolCalls []ChatToolCall

	for _, item := range resp.Output {
		switch item.Type {
		case "message":
			for _, part := range item.Content {
				switch {
				case part.Type == "output_text" && part.Text != "":
					contentText += part.Text
				case part.Type == "refusal" && part.Refusal != "":
					refusalText += part.Refusal
				}
			}
		case "function_call":
			toolCalls = append(toolCalls, ChatToolCall{
				ID:   item.CallID,
				Type: "function",
				Function: ChatFunctionCall{
					Name:      item.Name,
					Arguments: item.Arguments,
				},
			})
		case "reasoning":
			text := ""
			if opts != nil && opts.PreferReasoningContentText {
				// DeepSeek's thinking-mode replay needs the full reasoning_text.
				// A condensed summary is not equivalent, so it is never
				// relabeled as the full text. When no content part exists the
				// reasoning_content stays empty rather than fabricating one.
				for _, part := range item.Content {
					if part.Type == "reasoning_text" && part.Text != "" {
						text += part.Text
					}
				}
			} else {
				for _, s := range item.Summary {
					if s.Type == "summary_text" && s.Text != "" {
						text += s.Text
					}
				}
			}
			reasoningText += text
		case "web_search_call":
			// silently consumed — results already incorporated into text output
		}
	}

	msg := ChatMessage{Role: "assistant", Refusal: refusalText}
	if len(toolCalls) > 0 {
		msg.ToolCalls = toolCalls
	}
	if contentText != "" {
		raw, _ := json.Marshal(contentText)
		msg.Content = raw
	}
	if reasoningText != "" {
		msg.ReasoningContent = reasoningText
	}

	finishReason := responsesStatusToChatFinishReason(resp.Status, resp.IncompleteDetails, toolCalls)

	out.Choices = []ChatChoice{{
		Index:        0,
		Message:      msg,
		FinishReason: finishReason,
	}}

	out.Usage = chatUsageFromResponsesUsage(resp.Usage)

	return out
}

func responsesStatusToChatFinishReason(status string, details *ResponsesIncompleteDetails, toolCalls []ChatToolCall) string {
	switch status {
	case "incomplete":
		if details != nil {
			switch details.Reason {
			case "max_output_tokens":
				return "length"
			case "content_filter":
				return "content_filter"
			}
		}
		return "stop"
	case "completed":
		if len(toolCalls) > 0 {
			return "tool_calls"
		}
		return "stop"
	default:
		return "stop"
	}
}

// ---------------------------------------------------------------------------
// Streaming: ResponsesStreamEvent → []ChatCompletionsChunk (stateful converter)
// ---------------------------------------------------------------------------

// ResponsesEventToChatState tracks state for converting a sequence of Responses
// SSE events into Chat Completions SSE chunks.
type ResponsesEventToChatState struct {
	ID                     string
	Model                  string
	Created                int64
	ServiceTier            string // upstream tier observed on response events; echoed on chunks
	SentRole               bool
	SawToolCall            bool
	SawText                bool
	Finalized              bool        // true after finish chunk has been emitted
	NextToolCallIndex      int         // next sequential tool_call index to assign
	OutputIndexToToolIndex map[int]int // Responses output_index → Chat tool_calls index
	OutputIndexToArguments map[int]string
	IncludeUsage           bool
	Usage                  *ChatUsage

	// PreferReasoningText keeps the full reasoning_text as the single source
	// of reasoning_content. A condensed summary is never substituted for the
	// full text, so summary deltas are ignored in this mode (DeepSeek).
	PreferReasoningText bool
	reasoningParts      []*reasoningTextPart
}

// reasoningTextPart tracks one reasoning_text content part so partial deltas,
// terminal done text and full item payloads can be reconciled per part instead
// of through one global "already seen" flag. Identity follows the DeepSeek
// Responses schema: an item id (strong) plus a content index (strong). The
// output index is only used to adopt a part whose item id is not yet known.
type reasoningTextPart struct {
	itemID       string
	outputIndex  int
	contentIndex int

	// sent is the text already emitted downstream, used to compute the missing
	// suffix when a terminal event repeats the part with its full text.
	sent strings.Builder
}

// matchReasoningTextPart resolves an incoming reasoning event to an existing
// part using explicit identity only:
//   - the content index must always be equal; a different content index never
//     reuses another part,
//   - a non-empty item id only matches the same id, or adopts a part whose id
//     is still unknown at the same output index,
//   - two non-empty, different item ids never match, even when indices are
//     omitted (int defaults must not alias distinct items),
//   - an event without an item id falls back to the output index, which is
//     unique per item.
func matchReasoningTextPart(parts []*reasoningTextPart, itemID string, outputIndex, contentIndex int) *reasoningTextPart {
	for _, p := range parts {
		if p.contentIndex != contentIndex {
			continue
		}
		if itemID != "" {
			if p.itemID == itemID {
				return p
			}
			if p.itemID == "" && p.outputIndex == outputIndex {
				return p
			}
			continue
		}
		if p.outputIndex == outputIndex {
			return p
		}
	}
	return nil
}

func reasoningTextPartFor(parts *[]*reasoningTextPart, itemID string, outputIndex, contentIndex int) *reasoningTextPart {
	p := matchReasoningTextPart(*parts, itemID, outputIndex, contentIndex)
	if p == nil {
		p = &reasoningTextPart{itemID: itemID, outputIndex: outputIndex, contentIndex: contentIndex}
		*parts = append(*parts, p)
		return p
	}
	if p.itemID == "" && itemID != "" {
		p.itemID = itemID
	}
	p.outputIndex = outputIndex
	return p
}

// orderedReasoningParts returns the tracked parts sorted by content index so
// output preserves the upstream's part order even if events arrive out of
// order.
func orderedReasoningParts(parts []*bufferedReasoningPart) []*bufferedReasoningPart {
	ordered := make([]*bufferedReasoningPart, len(parts))
	copy(ordered, parts)
	sort.SliceStable(ordered, func(i, j int) bool {
		return ordered[i].contentIndex < ordered[j].contentIndex
	})
	return ordered
}

// emitReasoningTextSuffix emits only the not-yet-sent suffix of a part's full
// text. It never duplicates text already streamed and never fabricates text
// when the payload cannot be reconciled with what was sent.
func emitReasoningTextSuffix(state *ResponsesEventToChatState, p *reasoningTextPart, full string) []ChatCompletionsChunk {
	if p == nil || full == "" {
		return nil
	}
	sent := p.sent.String()
	var suffix string
	switch {
	case sent == "":
		suffix = full
	case strings.HasPrefix(full, sent):
		suffix = full[len(sent):]
	default:
		return nil
	}
	if suffix == "" {
		return nil
	}
	_, _ = p.sent.WriteString(suffix)
	reasoning := suffix
	return []ChatCompletionsChunk{makeChatDeltaChunk(state, ChatDelta{ReasoningContent: &reasoning})}
}

// NewResponsesEventToChatState returns an initialised stream state.
func NewResponsesEventToChatState() *ResponsesEventToChatState {
	return NewResponsesEventToChatStateWithOptions(nil)
}

// ResponsesEventToChatOptions carries provider-scoped streaming policies. A
// nil value behaves exactly like the historical converter.
type ResponsesEventToChatOptions struct {
	// PreferReasoningText maps only the full reasoning_text to
	// reasoning_content; summary deltas are ignored because a condensed
	// summary is not the full thinking text DeepSeek requires on replay.
	PreferReasoningText bool
}

// NewResponsesEventToChatStateWithOptions returns an initialised stream state
// with provider-scoped policies (see ResponsesEventToChatOptions).
func NewResponsesEventToChatStateWithOptions(opts *ResponsesEventToChatOptions) *ResponsesEventToChatState {
	return &ResponsesEventToChatState{
		ID:                     generateChatCmplID(),
		Created:                time.Now().Unix(),
		OutputIndexToToolIndex: make(map[int]int),
		OutputIndexToArguments: make(map[int]string),
		PreferReasoningText:    opts != nil && opts.PreferReasoningText,
	}
}

// ResponsesEventToChatChunks converts a single Responses SSE event into zero
// or more Chat Completions chunks, updating state as it goes.
func ResponsesEventToChatChunks(evt *ResponsesStreamEvent, state *ResponsesEventToChatState) []ChatCompletionsChunk {
	switch evt.Type {
	case "response.created":
		return resToChatHandleCreated(evt, state)
	case "response.output_text.delta":
		return resToChatHandleTextDelta(evt, state)
	case "response.refusal.delta":
		return resToChatHandleRefusalDelta(evt, state)
	case "response.output_item.added":
		return resToChatHandleOutputItemAdded(evt, state)
	case "response.output_item.done":
		if state.PreferReasoningText {
			return resToChatHandleReasoningItemDone(evt, state)
		}
		return nil
	case "response.function_call_arguments.delta",
		// custom/freeform 工具（如新版 apply_patch）的输入增量与 function_call 参数增量同形，
		// 均按 OutputIndex 累加到对应工具调用。
		"response.custom_tool_call_input.delta":
		return resToChatHandleFuncArgsDelta(evt, state)
	case "response.function_call_arguments.done", "response.custom_tool_call_input.done":
		return resToChatHandleFuncArgsDone(evt, state)
	case "response.reasoning_summary_text.delta",
		// 原始推理文本增量（真实 Codex 客户端消费的 reasoning_text.delta），
		// 与 reasoning summary 一样映射为 reasoning_content。
		"response.reasoning_text.delta":
		return resToChatHandleReasoningDelta(evt, state)
	case "response.reasoning_summary_text.done", "response.reasoning_text.done":
		if evt.Type == "response.reasoning_text.done" && state.PreferReasoningText {
			return resToChatHandleReasoningTextDone(evt, state)
		}
		return nil
	case "response.content_part.done":
		// A reasoning_text part can also be delivered as its own content-part
		// event carrying the full text; reconcile it with the same suffix
		// logic used by the other terminal reasoning events.
		if state.PreferReasoningText && evt.Part != nil && evt.Part.Type == "reasoning_text" && evt.Part.Text != "" {
			p := reasoningTextPartFor(&state.reasoningParts, evt.ItemID, evt.OutputIndex, evt.ContentIndex)
			return emitReasoningTextSuffix(state, p, evt.Part.Text)
		}
		return nil
	// response.done 是 Realtime/WS 与项目透传路径使用的终止别名；
	// 普通 Responses HTTP SSE 的公开终止事件仍以 response.completed 为主。
	case "response.completed", "response.done", "response.incomplete", "response.failed":
		return resToChatHandleCompleted(evt, state)
	default:
		return nil
	}
}

// FinalizeResponsesChatStream emits a final chunk with finish_reason if the
// stream ended without a proper completion event (e.g. upstream disconnect).
// It is idempotent: if a completion event already emitted the finish chunk,
// this returns nil.
func FinalizeResponsesChatStream(state *ResponsesEventToChatState) []ChatCompletionsChunk {
	if state.Finalized {
		return nil
	}
	state.Finalized = true

	finishReason := "stop"
	if state.SawToolCall {
		finishReason = "tool_calls"
	}

	chunks := emitPreferredReasoningTextFallback(nil, state)
	chunks = append(chunks, makeChatFinishChunk(state, finishReason))

	if state.IncludeUsage && state.Usage != nil {
		chunks = append(chunks, ChatCompletionsChunk{
			ID:          state.ID,
			Object:      "chat.completion.chunk",
			Created:     state.Created,
			Model:       state.Model,
			ServiceTier: state.ServiceTier,
			Choices:     []ChatChunkChoice{},
			Usage:       state.Usage,
		})
	}

	return chunks
}

// ChatChunkToSSE formats a ChatCompletionsChunk as an SSE data line.
func ChatChunkToSSE(chunk ChatCompletionsChunk) (string, error) {
	data, err := json.Marshal(chunk)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("data: %s\n\n", data), nil
}

// --- internal handlers ---

func resToChatHandleCreated(evt *ResponsesStreamEvent, state *ResponsesEventToChatState) []ChatCompletionsChunk {
	if evt.Response != nil {
		if evt.Response.ID != "" {
			state.ID = evt.Response.ID
		}
		if state.Model == "" && evt.Response.Model != "" {
			state.Model = evt.Response.Model
		}
		if evt.Response.ServiceTier != "" {
			state.ServiceTier = evt.Response.ServiceTier
		}
	}
	// Emit the role chunk.
	if state.SentRole {
		return nil
	}
	state.SentRole = true

	role := "assistant"
	return []ChatCompletionsChunk{makeChatDeltaChunk(state, ChatDelta{Role: role})}
}

func resToChatHandleTextDelta(evt *ResponsesStreamEvent, state *ResponsesEventToChatState) []ChatCompletionsChunk {
	if evt.Delta == "" {
		return nil
	}
	state.SawText = true
	content := evt.Delta
	return []ChatCompletionsChunk{makeChatDeltaChunk(state, ChatDelta{Content: &content})}
}

func resToChatHandleRefusalDelta(evt *ResponsesStreamEvent, state *ResponsesEventToChatState) []ChatCompletionsChunk {
	if evt.Delta == "" {
		return nil
	}
	refusal := evt.Delta
	return []ChatCompletionsChunk{makeChatDeltaChunk(state, ChatDelta{Refusal: &refusal})}
}

func resToChatHandleOutputItemAdded(evt *ResponsesStreamEvent, state *ResponsesEventToChatState) []ChatCompletionsChunk {
	// function_call 与 custom_tool_call（custom/freeform 工具）均按工具调用注册，
	// 以便后续 *_input.delta / *_arguments.delta 能映射到正确的工具索引。
	if evt.Item == nil || (evt.Item.Type != "function_call" && evt.Item.Type != "custom_tool_call") {
		return nil
	}

	state.SawToolCall = true
	idx := state.NextToolCallIndex
	state.OutputIndexToToolIndex[evt.OutputIndex] = idx
	state.NextToolCallIndex++

	return []ChatCompletionsChunk{makeChatDeltaChunk(state, ChatDelta{
		ToolCalls: []ChatToolCall{{
			Index: &idx,
			ID:    evt.Item.CallID,
			Type:  "function",
			Function: ChatFunctionCall{
				Name: evt.Item.Name,
			},
		}},
	})}
}

func resToChatHandleFuncArgsDelta(evt *ResponsesStreamEvent, state *ResponsesEventToChatState) []ChatCompletionsChunk {
	if evt.Delta == "" {
		return nil
	}

	idx, ok := state.OutputIndexToToolIndex[evt.OutputIndex]
	if !ok {
		return nil
	}
	state.OutputIndexToArguments[evt.OutputIndex] += evt.Delta

	return []ChatCompletionsChunk{makeChatDeltaChunk(state, ChatDelta{
		ToolCalls: []ChatToolCall{{
			Index: &idx,
			Function: ChatFunctionCall{
				Arguments: evt.Delta,
			},
		}},
	})}
}

func resToChatHandleFuncArgsDone(evt *ResponsesStreamEvent, state *ResponsesEventToChatState) []ChatCompletionsChunk {
	idx, ok := state.OutputIndexToToolIndex[evt.OutputIndex]
	if !ok {
		return nil
	}

	completed := evt.Arguments
	if evt.Type == "response.custom_tool_call_input.done" {
		completed = evt.Input
	}
	current := state.OutputIndexToArguments[evt.OutputIndex]
	if completed == "" || !strings.HasPrefix(completed, current) || completed == current {
		return nil
	}

	remainder := completed[len(current):]
	state.OutputIndexToArguments[evt.OutputIndex] = completed
	return []ChatCompletionsChunk{makeChatDeltaChunk(state, ChatDelta{
		ToolCalls: []ChatToolCall{{
			Index: &idx,
			Function: ChatFunctionCall{
				Arguments: remainder,
			},
		}},
	})}
}

func resToChatHandleReasoningDelta(evt *ResponsesStreamEvent, state *ResponsesEventToChatState) []ChatCompletionsChunk {
	if evt.Delta == "" {
		return nil
	}
	if state.PreferReasoningText {
		// Keep only the full reasoning_text as reasoning_content. Summary
		// deltas are dropped: a condensed summary is not the full thinking
		// text DeepSeek requires, and must never be relabeled as one.
		if evt.Type == "response.reasoning_summary_text.delta" {
			return nil
		}
		p := reasoningTextPartFor(&state.reasoningParts, evt.ItemID, evt.OutputIndex, evt.ContentIndex)
		_, _ = p.sent.WriteString(evt.Delta)
	}
	reasoning := evt.Delta
	return []ChatCompletionsChunk{makeChatDeltaChunk(state, ChatDelta{ReasoningContent: &reasoning})}
}

// resToChatHandleReasoningTextDone reconciles a reasoning_text.done event with
// any deltas already sent for the same part and emits only the missing suffix.
// An upstream may send partial deltas ("A") followed by the full text ("AB").
func resToChatHandleReasoningTextDone(evt *ResponsesStreamEvent, state *ResponsesEventToChatState) []ChatCompletionsChunk {
	if evt.Text == "" {
		return nil
	}
	p := reasoningTextPartFor(&state.reasoningParts, evt.ItemID, evt.OutputIndex, evt.ContentIndex)
	return emitReasoningTextSuffix(state, p, evt.Text)
}

// resToChatHandleReasoningItemDone reconciles an output_item.done reasoning
// item. Every content part is walked in order and only the missing suffix is
// emitted, so multiple reasoning items and parts are preserved.
func resToChatHandleReasoningItemDone(evt *ResponsesStreamEvent, state *ResponsesEventToChatState) []ChatCompletionsChunk {
	if evt.Item == nil || evt.Item.Type != "reasoning" {
		return nil
	}
	itemID := evt.ItemID
	if itemID == "" {
		itemID = evt.Item.ID
	}
	return emitReasoningTextChunksFromParts(state, itemID, evt.OutputIndex, evt.Item.Content)
}

// emitReasoningTextChunksFromParts emits missing suffixes for every
// reasoning_text content part of one item, in part order. Each part keeps its
// positional content_index so multiple parts of the same item stay separate
// even when only item_id is present.
func emitReasoningTextChunksFromParts(state *ResponsesEventToChatState, itemID string, outputIndex int, parts []ResponsesContentPart) []ChatCompletionsChunk {
	var chunks []ChatCompletionsChunk
	contentIndex := -1
	for _, part := range parts {
		contentIndex++
		if part.Type != "reasoning_text" {
			continue
		}
		if part.Text == "" {
			continue
		}
		p := reasoningTextPartFor(&state.reasoningParts, itemID, outputIndex, contentIndex)
		chunks = append(chunks, emitReasoningTextSuffix(state, p, part.Text)...)
	}
	return chunks
}

// emitPreferredReasoningTextFallback emits missing reasoning_text suffixes for
// a terminal event's response.output. It walks every reasoning item/part in
// order; summary text is never substituted. When the terminal output carries
// no reasoning item at all, already-streamed parts are left untouched (the
// caller keeps whatever was streamed) and nothing is fabricated.
func emitPreferredReasoningTextFallback(evt *ResponsesStreamEvent, state *ResponsesEventToChatState) []ChatCompletionsChunk {
	if !state.PreferReasoningText || evt == nil || evt.Response == nil {
		return nil
	}
	var chunks []ChatCompletionsChunk
	for outputIndex, item := range evt.Response.Output {
		if item.Type != "reasoning" {
			continue
		}
		chunks = append(chunks, emitReasoningTextChunksFromParts(state, item.ID, outputIndex, item.Content)...)
	}
	return chunks
}

func resToChatHandleCompleted(evt *ResponsesStreamEvent, state *ResponsesEventToChatState) []ChatCompletionsChunk {
	state.Finalized = true
	finishReason := "stop"

	if evt.Usage != nil {
		state.Usage = chatUsageFromResponsesUsage(evt.Usage)
	}
	if evt.Response != nil {
		if evt.Response.Usage != nil {
			state.Usage = chatUsageFromResponsesUsage(evt.Response.Usage)
		}
		if evt.Response.ServiceTier != "" {
			state.ServiceTier = evt.Response.ServiceTier
		}

		switch evt.Response.Status {
		case "incomplete":
			if evt.Response.IncompleteDetails != nil {
				switch evt.Response.IncompleteDetails.Reason {
				case "max_output_tokens":
					finishReason = "length"
				case "content_filter":
					finishReason = "content_filter"
				}
			}
		case "completed":
			if state.SawToolCall {
				finishReason = "tool_calls"
			}
		}
	} else if state.SawToolCall {
		finishReason = "tool_calls"
	}

	var chunks []ChatCompletionsChunk
	chunks = append(chunks, emitPreferredReasoningTextFallback(evt, state)...)
	chunks = append(chunks, makeChatFinishChunk(state, finishReason))

	if state.IncludeUsage && state.Usage != nil {
		chunks = append(chunks, ChatCompletionsChunk{
			ID:          state.ID,
			Object:      "chat.completion.chunk",
			Created:     state.Created,
			Model:       state.Model,
			ServiceTier: state.ServiceTier,
			Choices:     []ChatChunkChoice{},
			Usage:       state.Usage,
		})
	}

	return chunks
}

func chatUsageFromResponsesUsage(u *ResponsesUsage) *ChatUsage {
	if u == nil {
		return nil
	}
	usage := &ChatUsage{
		PromptTokens:     u.InputTokens,
		CompletionTokens: u.OutputTokens,
		TotalTokens:      u.InputTokens + u.OutputTokens,
	}
	usage.PromptTokensDetails = promptDetailsFromResponses(u.InputTokensDetails)
	if u.CacheCreationInputTokens > 0 {
		if usage.PromptTokensDetails == nil {
			usage.PromptTokensDetails = &ChatTokenDetails{}
		}
		if usage.PromptTokensDetails.CacheWriteTokens == 0 && usage.PromptTokensDetails.CacheCreationTokens == 0 {
			usage.PromptTokensDetails.CacheCreationTokens = u.CacheCreationInputTokens
		}
	}
	usage.CompletionTokensDetails = completionDetailsFromResponses(u.OutputTokensDetails)
	return usage
}

// promptDetailsFromResponses maps Responses-API input_tokens_details into a
// Chat-Completions prompt_tokens_details. Returns nil when nothing would be
// emitted, so upstreams that do not break down prompt usage stay clean.
func promptDetailsFromResponses(src *ResponsesInputTokensDetails) *ChatTokenDetails {
	if src == nil {
		return nil
	}
	if src.CachedTokens == 0 && src.AudioTokens == 0 && src.CacheCreationTokens == 0 && src.CacheWriteTokens == 0 {
		return nil
	}
	return &ChatTokenDetails{
		CachedTokens:        src.CachedTokens,
		AudioTokens:         src.AudioTokens,
		CacheCreationTokens: src.CacheCreationTokens,
		CacheWriteTokens:    src.CacheWriteTokens,
	}
}

// completionDetailsFromResponses maps Responses-API output_tokens_details
// into a Chat-Completions completion_tokens_details. Mirrors the OpenAI
// official CompletionUsage schema: reasoning_tokens, audio_tokens, and
// the predicted-outputs accepted/rejected counts. Returns nil when nothing
// would be emitted so non-reasoning, non-audio responses stay clean.
func completionDetailsFromResponses(src *ResponsesOutputTokensDetails) *ChatTokenDetails {
	if src == nil {
		return nil
	}
	if src.ReasoningTokens == 0 && src.AudioTokens == 0 &&
		src.AcceptedPredictionTokens == 0 && src.RejectedPredictionTokens == 0 {
		return nil
	}
	return &ChatTokenDetails{
		ReasoningTokens:          src.ReasoningTokens,
		AudioTokens:              src.AudioTokens,
		AcceptedPredictionTokens: src.AcceptedPredictionTokens,
		RejectedPredictionTokens: src.RejectedPredictionTokens,
	}
}

func makeChatDeltaChunk(state *ResponsesEventToChatState, delta ChatDelta) ChatCompletionsChunk {
	return ChatCompletionsChunk{
		ID:          state.ID,
		Object:      "chat.completion.chunk",
		Created:     state.Created,
		Model:       state.Model,
		ServiceTier: state.ServiceTier,
		Choices: []ChatChunkChoice{{
			Index:        0,
			Delta:        delta,
			FinishReason: nil,
		}},
	}
}

func makeChatFinishChunk(state *ResponsesEventToChatState, finishReason string) ChatCompletionsChunk {
	empty := ""
	return ChatCompletionsChunk{
		ID:          state.ID,
		Object:      "chat.completion.chunk",
		Created:     state.Created,
		Model:       state.Model,
		ServiceTier: state.ServiceTier,
		Choices: []ChatChunkChoice{{
			Index:        0,
			Delta:        ChatDelta{Content: &empty},
			FinishReason: &finishReason,
		}},
	}
}

// generateChatCmplID returns a "chatcmpl-" prefixed random hex ID.
func generateChatCmplID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "chatcmpl-" + hex.EncodeToString(b)
}

// ---------------------------------------------------------------------------
// BufferedResponseAccumulator: accumulates SSE delta events for non-streaming
// paths where the terminal event may have empty output.
// ---------------------------------------------------------------------------

type bufferedFuncCall struct {
	OutputIndex int
	CallID      string
	Name        string
	Args        strings.Builder
}

// bufferedReasoningPart is one reasoning_text content part of one reasoning
// item. Deltas accumulate separately from the authoritative full payload so a
// terminal event can correct a part without duplicating what was already
// collected, and multiple parts stay independent.
type bufferedReasoningPart struct {
	contentIndex int
	delta        strings.Builder
	full         string
}

// bufferedReasoningItem groups the reasoning parts of a single reasoning item
// (and its summary, which is preserved as a summary but never treated as the
// full thinking text).
type bufferedReasoningItem struct {
	itemID      string
	outputIndex int
	summary     strings.Builder
	parts       []*bufferedReasoningPart
}

// matchBufferedReasoningItem resolves a buffered reasoning item with explicit
// identity only. A non-empty item id matches the same id or adopts an item
// whose id is still unknown at the same output index; two non-empty, different
// ids never match through the index fallback. An event without an item id
// falls back to the output index, which is unique per item.
func matchBufferedReasoningItem(items []*bufferedReasoningItem, itemID string, outputIndex int) *bufferedReasoningItem {
	if itemID != "" {
		for _, it := range items {
			if it.itemID == itemID {
				return it
			}
		}
		for _, it := range items {
			if it.itemID == "" && it.outputIndex == outputIndex {
				return it
			}
		}
		return nil
	}
	for _, it := range items {
		if it.outputIndex == outputIndex {
			return it
		}
	}
	return nil
}

func (a *BufferedResponseAccumulator) reasoningItemFor(itemID string, outputIndex int) *bufferedReasoningItem {
	it := matchBufferedReasoningItem(a.reasoningItems, itemID, outputIndex)
	if it == nil {
		it = &bufferedReasoningItem{itemID: itemID, outputIndex: outputIndex}
		a.reasoningItems = append(a.reasoningItems, it)
		return it
	}
	if it.itemID == "" && itemID != "" {
		it.itemID = itemID
	}
	it.outputIndex = outputIndex
	return it
}

func (it *bufferedReasoningItem) partFor(contentIndex int) *bufferedReasoningPart {
	for _, p := range it.parts {
		if p.contentIndex == contentIndex {
			return p
		}
	}
	p := &bufferedReasoningPart{contentIndex: contentIndex}
	it.parts = append(it.parts, p)
	return p
}

// setFull records an authoritative full text for the part, preferring the more
// complete real payload. A shorter prefix never replaces a longer text, and
// unrelated text is ignored rather than overwriting what was already trusted.
func (p *bufferedReasoningPart) setFull(text string) {
	if text == "" {
		return
	}
	switch {
	case p.full == "":
		p.full = text
	case strings.HasPrefix(text, p.full):
		p.full = text
	case strings.HasPrefix(p.full, text):
		// existing text is at least as complete
	default:
		// Cannot reconcile; keep the trusted payload.
	}
}

// effectiveText returns the part's authoritative full text when a terminal
// payload supplied one, otherwise the accumulated deltas.
func (p *bufferedReasoningPart) effectiveText() string {
	if p.full != "" {
		return p.full
	}
	return p.delta.String()
}

// BufferedResponseAccumulator collects content from Responses SSE delta events
// so that non-streaming handlers can reconstruct output when the terminal event
// (response.completed / response.done) carries an empty output array.
type BufferedResponseAccumulator struct {
	text                 strings.Builder
	refusal              strings.Builder
	reasoning            strings.Builder
	reasoningSummary     strings.Builder
	preferReasoningText  bool
	reasoningItems       []*bufferedReasoningItem
	funcCalls            []bufferedFuncCall
	outputIndexToFuncIdx map[int]int
}

// NewBufferedResponseAccumulator returns an initialised accumulator.
func NewBufferedResponseAccumulator() *BufferedResponseAccumulator {
	return NewBufferedResponseAccumulatorWithOptions(nil)
}

// BufferedResponseAccumulatorOptions carries provider-scoped accumulation
// policies. A nil value behaves exactly like the historical accumulator.
type BufferedResponseAccumulatorOptions struct {
	// PreferReasoningText keeps the full reasoning_text stream separate from
	// the condensed summary and rebuilds the reasoning item as
	// content=[{type:"reasoning_text",...}] instead of a summary part. This
	// preserves DeepSeek's required thinking-mode history and avoids emitting
	// both the summary and the full text.
	PreferReasoningText bool
}

// NewBufferedResponseAccumulatorWithOptions returns an initialised accumulator
// with provider-scoped policies (see BufferedResponseAccumulatorOptions).
func NewBufferedResponseAccumulatorWithOptions(opts *BufferedResponseAccumulatorOptions) *BufferedResponseAccumulator {
	return &BufferedResponseAccumulator{
		outputIndexToFuncIdx: make(map[int]int),
		preferReasoningText:  opts != nil && opts.PreferReasoningText,
	}
}

// ProcessEvent inspects a single Responses SSE event and accumulates any
// content it carries. Only delta events that contribute to the final output
// are handled; all other event types are silently ignored.
func (a *BufferedResponseAccumulator) ProcessEvent(event *ResponsesStreamEvent) {
	switch event.Type {
	case "response.output_text.delta":
		if event.Delta != "" {
			_, _ = a.text.WriteString(event.Delta)
		}
	case "response.refusal.delta":
		if event.Delta != "" {
			_, _ = a.refusal.WriteString(event.Delta)
		}
	case "response.output_item.added":
		if event.Item != nil && event.Item.Type == "reasoning" && a.preferReasoningText {
			a.reasoningItemFor(event.Item.ID, event.OutputIndex)
			break
		}
		if event.Item != nil && (event.Item.Type == "function_call" || event.Item.Type == "custom_tool_call") {
			idx := len(a.funcCalls)
			a.outputIndexToFuncIdx[event.OutputIndex] = idx
			a.funcCalls = append(a.funcCalls, bufferedFuncCall{
				OutputIndex: event.OutputIndex,
				CallID:      event.Item.CallID,
				Name:        event.Item.Name,
			})
		}
	case "response.function_call_arguments.delta", "response.custom_tool_call_input.delta":
		if event.Delta != "" {
			if idx, ok := a.outputIndexToFuncIdx[event.OutputIndex]; ok {
				_, _ = a.funcCalls[idx].Args.WriteString(event.Delta)
			}
		}
	case "response.function_call_arguments.done", "response.custom_tool_call_input.done":
		completed := event.Arguments
		if event.Type == "response.custom_tool_call_input.done" {
			completed = event.Input
		}
		if completed != "" {
			if idx, ok := a.outputIndexToFuncIdx[event.OutputIndex]; ok {
				a.funcCalls[idx].Args.Reset()
				_, _ = a.funcCalls[idx].Args.WriteString(completed)
			}
		}
	case "response.reasoning_summary_text.delta":
		if event.Delta != "" {
			_, _ = a.reasoningSummary.WriteString(event.Delta)
			if !a.preferReasoningText {
				_, _ = a.reasoning.WriteString(event.Delta)
			} else {
				it := a.reasoningItemFor(event.ItemID, event.OutputIndex)
				_, _ = it.summary.WriteString(event.Delta)
			}
		}
	case "response.reasoning_text.delta":
		if event.Delta != "" {
			if !a.preferReasoningText {
				_, _ = a.reasoning.WriteString(event.Delta)
			} else {
				it := a.reasoningItemFor(event.ItemID, event.OutputIndex)
				p := it.partFor(event.ContentIndex)
				_, _ = p.delta.WriteString(event.Delta)
			}
		}
	case "response.reasoning_text.done":
		// The done payload is authoritative for that part: it may be the only
		// source (no deltas) or the full text of a partially streamed part.
		if a.preferReasoningText && event.Text != "" {
			it := a.reasoningItemFor(event.ItemID, event.OutputIndex)
			p := it.partFor(event.ContentIndex)
			p.setFull(event.Text)
		}
	case "response.output_item.done":
		// Capture full reasoning payloads (and summaries) delivered only in
		// the terminal item, keeping per-item/per-part identity.
		if a.preferReasoningText && event.Item != nil && event.Item.Type == "reasoning" {
			itemID := event.ItemID
			if itemID == "" {
				itemID = event.Item.ID
			}
			it := a.reasoningItemFor(itemID, event.OutputIndex)
			for _, s := range event.Item.Summary {
				if s.Type == "summary_text" && s.Text != "" {
					_, _ = it.summary.WriteString(s.Text)
				}
			}
			contentIndex := -1
			for _, part := range event.Item.Content {
				contentIndex++
				if part.Type != "reasoning_text" || part.Text == "" {
					continue
				}
				p := it.partFor(contentIndex)
				p.setFull(part.Text)
			}
		}
	case "response.content_part.done":
		// A single reasoning_text part can also arrive as its own content part.
		if a.preferReasoningText && event.Part != nil && event.Part.Type == "reasoning_text" && event.Part.Text != "" {
			it := a.reasoningItemFor(event.ItemID, event.OutputIndex)
			p := it.partFor(event.ContentIndex)
			p.setFull(event.Part.Text)
		}
	}
}

// HasContent reports whether any content has been accumulated.
func (a *BufferedResponseAccumulator) HasContent() bool {
	if a.text.Len() > 0 || a.refusal.Len() > 0 || len(a.funcCalls) > 0 || a.reasoning.Len() > 0 || a.reasoningSummary.Len() > 0 {
		return true
	}
	for _, it := range a.reasoningItems {
		if it.summary.Len() > 0 {
			return true
		}
		for _, p := range it.parts {
			if p.effectiveText() != "" {
				return true
			}
		}
	}
	return false
}

// BuildOutput constructs a []ResponsesOutput from the accumulated delta
// content. The order matches what ResponsesToChatCompletions expects:
// reasoning → message → function_calls.
func (a *BufferedResponseAccumulator) BuildOutput() []ResponsesOutput {
	var out []ResponsesOutput

	if a.preferReasoningText {
		out = append(out, a.buildReasoningItems()...)
	} else if a.reasoning.Len() > 0 {
		out = append(out, ResponsesOutput{
			Type: "reasoning",
			Summary: []ResponsesSummary{{
				Type: "summary_text",
				Text: a.reasoning.String(),
			}},
		})
	}

	if a.text.Len() > 0 || a.refusal.Len() > 0 {
		var content []ResponsesContentPart
		if a.text.Len() > 0 {
			content = append(content, ResponsesContentPart{Type: "output_text", Text: a.text.String()})
		}
		if a.refusal.Len() > 0 {
			content = append(content, ResponsesContentPart{Type: "refusal", Refusal: a.refusal.String()})
		}
		out = append(out, ResponsesOutput{
			Type:    "message",
			Role:    "assistant",
			Content: content,
		})
	}

	for i := range a.funcCalls {
		out = append(out, ResponsesOutput{
			Type:      "function_call",
			CallID:    a.funcCalls[i].CallID,
			Name:      a.funcCalls[i].Name,
			Arguments: a.funcCalls[i].Args.String(),
		})
	}

	return out
}

// buildReasoningItems reconstructs one reasoning item per tracked item, keeping
// each part's real type (reasoning_text) and order. A summary-only item keeps
// its summary as an ordinary summary part; summary text is never promoted to
// the full thinking text.
func (a *BufferedResponseAccumulator) buildReasoningItems() []ResponsesOutput {
	var out []ResponsesOutput
	for _, it := range a.reasoningItems {
		item := ResponsesOutput{Type: "reasoning", ID: it.itemID}
		for _, p := range orderedReasoningParts(it.parts) {
			text := p.effectiveText()
			if text == "" {
				continue
			}
			item.Content = append(item.Content, ResponsesContentPart{Type: "reasoning_text", Text: text})
		}
		if len(item.Content) == 0 && it.summary.Len() > 0 {
			item.Summary = []ResponsesSummary{{Type: "summary_text", Text: it.summary.String()}}
		}
		if len(item.Content) == 0 && len(item.Summary) == 0 {
			continue
		}
		out = append(out, item)
	}
	return out
}

// SupplementResponseOutput fills resp.Output from accumulated stream content
// when the terminal event delivered an empty output array. It also fills empty
// function-call arguments from authoritative argument-done events.
func (a *BufferedResponseAccumulator) SupplementResponseOutput(resp *ResponsesResponse) {
	if resp == nil {
		return
	}
	if len(resp.Output) == 0 {
		if a.HasContent() {
			resp.Output = a.BuildOutput()
		}
		return
	}

	// The terminal event can carry a non-empty output array whose message has
	// no usable text. Trusting it as-is silently drops the text that already
	// streamed: the client gets an empty reply while usage still bills the
	// terminal output_tokens. Refill it from the accumulated deltas; non-empty
	// terminal text stays authoritative.
	if a.text.Len() > 0 && !responsesOutputHasText(resp.Output) {
		if !fillResponsesOutputText(resp.Output, a.text.String()) {
			resp.Output = append(resp.Output, ResponsesOutput{
				Type: "message",
				Role: "assistant",
				Content: []ResponsesContentPart{{
					Type: "output_text",
					Text: a.text.String(),
				}},
			})
		}
	}

	if a.preferReasoningText {
		a.supplementReasoningOutput(resp)
	}

	for outputIndex := range resp.Output {
		item := &resp.Output[outputIndex]
		if item.Type != "function_call" || item.Arguments != "" {
			continue
		}
		for funcIndex := range a.funcCalls {
			call := &a.funcCalls[funcIndex]
			matchesCallID := item.CallID != "" && item.CallID == call.CallID
			if !matchesCallID && call.OutputIndex != outputIndex {
				continue
			}
			if call.Args.Len() > 0 {
				item.Arguments = call.Args.String()
			}
			break
		}
	}
}

// supplementReasoningOutput reconciles accumulated reasoning with the terminal
// output. Existing reasoning items get their missing parts restored by item id
// (or position), each part keeping its own text. Missing reasoning items are
// re-inserted using their tracked original output_index as the order key, so a
// terminal array that dropped an earlier item does not reorder the result
// (rs_A/output0 + rs_B/output1 with terminal [rs_B, message] must stay AB).
// Non-reasoning items keep their relative order and fields.
func (a *BufferedResponseAccumulator) supplementReasoningOutput(resp *ResponsesResponse) {
	if len(a.reasoningItems) == 0 {
		return
	}
	matched := make(map[*bufferedReasoningItem]bool)
	// Assign each tracked reasoning item a stable order rank: the tracked
	// original output_index when the upstream supplied distinct values, with
	// arrival order as the tie-breaker for upstreams that omit output_index.
	// The terminal array may be compressed, so its current index must not be
	// used as the order key.
	orderedTracked := make([]*bufferedReasoningItem, len(a.reasoningItems))
	copy(orderedTracked, a.reasoningItems)
	sort.SliceStable(orderedTracked, func(i, j int) bool {
		return orderedTracked[i].outputIndex < orderedTracked[j].outputIndex
	})
	rank := make(map[*bufferedReasoningItem]int, len(orderedTracked))
	for i, tracked := range orderedTracked {
		rank[tracked] = i
	}
	// terminalKeys[i] is the tracked original output_index of the reasoning
	// item currently at resp.Output[i]; -1 for non-reasoning or uncorrelated
	// items. It is kept aligned with resp.Output as missing items are inserted.
	terminalKeys := make([]int, len(resp.Output))
	for i := range terminalKeys {
		terminalKeys[i] = -1
	}
	for outputIndex := range resp.Output {
		item := &resp.Output[outputIndex]
		if item.Type != "reasoning" {
			continue
		}
		tracked := matchBufferedReasoningItem(a.reasoningItems, item.ID, outputIndex)
		if tracked == nil {
			continue
		}
		matched[tracked] = true
		terminalKeys[outputIndex] = rank[tracked]
		// Reconcile each content position independently: the terminal item may
		// carry only a subset of the streamed parts, or a shorter prefix than
		// the authoritative done/full payload. Rebuild in original index order
		// so parts are neither dropped nor reordered, keeping the more complete
		// real text for each position.
		byIndex := make(map[int]ResponsesContentPart, len(item.Content))
		nextIndex := -1
		for _, part := range item.Content {
			nextIndex++
			byIndex[nextIndex] = part
		}
		for _, p := range tracked.parts {
			full := p.effectiveText()
			if full == "" {
				continue
			}
			existing, ok := byIndex[p.contentIndex]
			switch {
			case !ok:
				byIndex[p.contentIndex] = ResponsesContentPart{Type: "reasoning_text", Text: full}
			case existing.Type != "reasoning_text":
				// Keep an unrelated non-text part untouched.
			case existing.Text == "" || strings.HasPrefix(full, existing.Text):
				existing.Text = full
				byIndex[p.contentIndex] = existing
			default:
				// The terminal text is at least as complete; keep it.
			}
		}
		maxIndex := len(item.Content) - 1
		for _, p := range tracked.parts {
			if p.contentIndex > maxIndex {
				maxIndex = p.contentIndex
			}
		}
		ordered := make([]ResponsesContentPart, 0, maxIndex+1)
		for index := 0; index <= maxIndex; index++ {
			if part, ok := byIndex[index]; ok {
				ordered = append(ordered, part)
			}
		}
		if len(ordered) > 0 {
			item.Content = ordered
		}
	}

	for _, tracked := range orderedTracked {
		if matched[tracked] {
			continue
		}
		rebuilt := ResponsesOutput{Type: "reasoning", ID: tracked.itemID}
		for _, p := range orderedReasoningParts(tracked.parts) {
			if text := p.effectiveText(); text != "" {
				rebuilt.Content = append(rebuilt.Content, ResponsesContentPart{Type: "reasoning_text", Text: text})
			}
		}
		if len(rebuilt.Content) == 0 {
			continue
		}
		// Insert before the first existing reasoning item whose tracked order
		// key is greater; that keeps the original relative order even when the
		// terminal array is compressed.
		insertAt := len(resp.Output)
		for i := range resp.Output {
			if terminalKeys[i] >= 0 && terminalKeys[i] > rank[tracked] {
				insertAt = i
				break
			}
		}
		if insertAt == len(resp.Output) {
			// No later reasoning item: append after the last reasoning item, or
			// before the first message/function_call when none is present.
			lastReasoning := -1
			for i := range resp.Output {
				if resp.Output[i].Type == "reasoning" {
					lastReasoning = i
				}
			}
			if lastReasoning >= 0 {
				insertAt = lastReasoning + 1
			} else {
				for i := range resp.Output {
					if resp.Output[i].Type == "message" || resp.Output[i].Type == "function_call" {
						insertAt = i
						break
					}
				}
			}
		}
		resp.Output = append(resp.Output, ResponsesOutput{})
		copy(resp.Output[insertAt+1:], resp.Output[insertAt:])
		resp.Output[insertAt] = rebuilt
		terminalKeys = append(terminalKeys, -1)
		copy(terminalKeys[insertAt+1:], terminalKeys[insertAt:])
		terminalKeys[insertAt] = rank[tracked]
	}
}

// responsesOutputHasText reports whether the terminal output already carries
// usable message text. Whitespace-only text does not count.
func responsesOutputHasText(output []ResponsesOutput) bool {
	for i := range output {
		if output[i].Type != "message" {
			continue
		}
		for _, part := range output[i].Content {
			if part.Type == "output_text" && strings.TrimSpace(part.Text) != "" {
				return true
			}
		}
	}
	return false
}

// fillResponsesOutputText writes text into the first empty output_text part of
// the first message item, adding an output_text part when that message has
// none. It returns false when the output holds no message item, leaving the
// caller to append one.
func fillResponsesOutputText(output []ResponsesOutput, text string) bool {
	for i := range output {
		if output[i].Type != "message" {
			continue
		}
		for j := range output[i].Content {
			if output[i].Content[j].Type != "output_text" {
				continue
			}
			if strings.TrimSpace(output[i].Content[j].Text) == "" {
				output[i].Content[j].Text = text
				return true
			}
		}
		output[i].Content = append(output[i].Content, ResponsesContentPart{
			Type: "output_text",
			Text: text,
		})
		return true
	}
	return false
}
