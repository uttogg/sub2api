package service

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// antigravityCompactStreamAdapter 把上游的 Claude 流静默累积为一份摘要，并在
// 终态一次性写出 Codex remote compaction v2 需要的 exactly one
// response.output_item.done(type=compaction) 与携带同一 item 的 response.completed。
// 摘要为空、被 max_tokens 截断或上游返回工具调用时以 response.failed 终止，
// 绝不伪造压缩成功。
type antigravityCompactStreamAdapter struct {
	c         *gin.Context
	model     string
	createdAt int64
	antigravityCompactSummaryState
	// explicitStop 仅在会话实际看到上游显式停止事件后由 session 注入；
	// processor.Finish() 在提前 EOF 时合成的终止事件不置位。
	explicitStop bool
	failure      error
}

// antigravityCompactSummaryState 累积一份压缩摘要的结构性输入：可见文本、
// 是否出现工具调用、上游原始 finishReason 与 usage。两个适配器（流式/非流式
// 收集）共用同一份累积与判据，避免两处语义漂移。
type antigravityCompactSummaryState struct {
	summary    strings.Builder
	sawToolUse bool
	usage      ClaudeUsage
	// geminiFinishReason 记录上游原始 candidates[0].finishReason。通用转换器会
	// 把任意非 MAX_TOKENS 结束原因映射为 Claude end_turn，因此成功判据必须以
	// 原始值 STOP 为准；一旦出现过非 STOP 值就保留该值，任何非 STOP 都失败。
	geminiFinishReason string
}

func (st *antigravityCompactSummaryState) consume(event *apicompat.AnthropicStreamEvent) {
	if event == nil {
		return
	}
	if event.Message != nil {
		mergeAnthropicUsage(&st.usage, event.Message.Usage)
	}
	if event.ContentBlock != nil {
		switch event.ContentBlock.Type {
		case "text":
			st.summary.WriteString(event.ContentBlock.Text)
		case "tool_use":
			st.sawToolUse = true
		}
	}
	if event.Delta != nil {
		if event.Delta.Type == "text_delta" {
			st.summary.WriteString(event.Delta.Text)
		}
	}
	if event.Usage != nil {
		mergeAnthropicUsage(&st.usage, *event.Usage)
	}
}

func (st *antigravityCompactSummaryState) summaryText() string {
	return strings.TrimSpace(st.summary.String())
}

// observeGeminiFinishReason 由 session 从原始 SSE 行注入。只要观测到过一次非
// STOP 就保留它，避免“先 SAFETY 后 STOP”这类不一致形态被判成功。
func (st *antigravityCompactSummaryState) observeGeminiFinishReason(reason string) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return
	}
	if st.geminiFinishReason == "" || (reason != "STOP" && st.geminiFinishReason == "STOP") {
		st.geminiFinishReason = reason
	}
}

func newAntigravityCompactStreamAdapter(c *gin.Context, model string) *antigravityCompactStreamAdapter {
	return &antigravityCompactStreamAdapter{
		c:         c,
		model:     model,
		createdAt: time.Now().Unix(),
	}
}

func (a *antigravityCompactStreamAdapter) Emit(event *apicompat.AnthropicStreamEvent, _ *antigravityClientWriter) {
	a.antigravityCompactSummaryState.consume(event)
}

func (a *antigravityCompactStreamAdapter) Finalize(writer *antigravityClientWriter) {
	summary := a.summaryText()
	if reason := antigravityCompactFailureReason(summary, a.sawToolUse, a.geminiFinishReason, a.explicitStop); reason != "" {
		a.fail(writer, reason)
		return
	}
	payload, err := buildAntigravityCompactResponseJSON(a.model, summary, a.usage, a.createdAt)
	if err != nil {
		a.fail(writer, "Failed to encode Antigravity compaction response")
		return
	}
	stream, ok := buildOpenAICompactSSEPayload(payload)
	if !ok {
		a.fail(writer, "Failed to encode Antigravity compaction stream")
		return
	}
	// 写入失败即客户端已断开：与既有 Responses/Claude 流一致，由 session 归为
	// client_disconnect 并照常结算 usage，不把不可投递的终态当作上游失败。
	writer.Write(stream)
}

func (a *antigravityCompactStreamAdapter) WriteError(writer *antigravityClientWriter, reason string) {
	message := "Antigravity compaction stream failed before a summary was produced"
	if trimmed := strings.TrimSpace(reason); trimmed != "" {
		message += " (" + trimmed + ")"
	}
	a.fail(writer, message)
}

func (a *antigravityCompactStreamAdapter) setExplicitUpstreamStop(explicit bool) {
	a.explicitStop = explicit
}

// finalError 供 session 在写出终态后把压缩失败交回 handler，使失败不会被
// 误记为成功的压缩回合。终态已经写出，调用方不会追加第二个终态。
func (a *antigravityCompactStreamAdapter) finalError() error {
	return a.failure
}

func (a *antigravityCompactStreamAdapter) fail(writer *antigravityClientWriter, reason string) {
	if a.failure != nil {
		return
	}
	a.failure = errors.New(reason)
	writeAntigravityCompactStreamFailure(a.c, writer, reason)
}

func (s *AntigravityGatewayService) handleAntigravityCompactStreaming(
	c *gin.Context,
	resp *http.Response,
	startTime time.Time,
	originalModel string,
) (*antigravityStreamResult, error) {
	return s.handleAntigravityCompatStream(
		c,
		resp,
		startTime,
		originalModel,
		newAntigravityCompactStreamAdapter(c, originalModel),
		"antigravity compaction stream",
	)
}

func (s *AntigravityGatewayService) handleAntigravityCompactNonStreaming(
	c *gin.Context,
	resp *http.Response,
	startTime time.Time,
	originalModel string,
) (*antigravityStreamResult, error) {
	// 非流式压缩同样走上游 SSE，但由静默 collector 复用 compat session 的超时与
	// 显式停止跟踪：collectClaudeStreamResponse 会把提前 EOF 合成为 end_turn，
	// 不能用于判定压缩是否真正完整。collector 不向客户端写出任何字节，成功时由
	// 本函数统一写 JSON。
	adapter := newAntigravityCompactCollectAdapter(originalModel)
	result, err := s.handleAntigravityCompatStreamWithKeepaliveInterval(
		c,
		resp,
		startTime,
		originalModel,
		adapter,
		"antigravity compaction collect",
		antigravityCompactCollectPreContentWait,
		antigravityCompactCollectPreContentWait,
	)
	if err != nil {
		var failoverErr *UpstreamFailoverError
		if errors.As(err, &failoverErr) {
			return nil, err
		}
		if adapter.failure != nil {
			// 错误响应照常写出；已消费的 usage 通过部分 result 交回 handler 入账，
			// 与 compat.go/handler 的既有部分计费规范一致（failover 仍为 nil）。
			return result, s.writeAntigravityCompatError(c, http.StatusBadGateway, "upstream_error", adapter.failure.Error())
		}
		return result, s.mapAntigravityCompatCollectionError(c, err)
	}
	if adapter.failure != nil {
		return result, s.writeAntigravityCompatError(c, http.StatusBadGateway, "upstream_error", adapter.failure.Error())
	}
	if len(adapter.payload) == 0 {
		return result, s.writeAntigravityCompatError(c, http.StatusBadGateway, "upstream_error", "Antigravity compaction produced no response")
	}
	c.Data(http.StatusOK, "application/json; charset=utf-8", adapter.payload)
	return result, nil
}

// antigravityCompactCollectPreContentWait 是静默 collector 收到首个语义事件前
// 的绝对上限；上游静默由既有 StreamDataIntervalTimeout 更早兜底，而 collector
// 通过 suppressWriterKeepalive 结构性关闭所有 writer 心跳，注释行不会把非流式
// JSON 响应提前提交成 SSE。
const antigravityCompactCollectPreContentWait = time.Hour

// antigravityCompactCollectAdapter 静默累积一次压缩流，不向客户端写字节。
// 成功时 payload 为最终 JSON；失败时 failure 携带具体原因，由调用方统一映射
// 为 JSON 错误。
type antigravityCompactCollectAdapter struct {
	model     string
	createdAt int64
	antigravityCompactSummaryState
	explicitStop bool
	payload      []byte
	failure      error
}

func newAntigravityCompactCollectAdapter(model string) *antigravityCompactCollectAdapter {
	return &antigravityCompactCollectAdapter{model: model, createdAt: time.Now().Unix()}
}

func (a *antigravityCompactCollectAdapter) Emit(event *apicompat.AnthropicStreamEvent, _ *antigravityClientWriter) {
	a.antigravityCompactSummaryState.consume(event)
}

func (a *antigravityCompactCollectAdapter) Finalize(_ *antigravityClientWriter) {
	summary := a.summaryText()
	if reason := antigravityCompactFailureReason(summary, a.sawToolUse, a.geminiFinishReason, a.explicitStop); reason != "" {
		a.failure = errors.New(reason)
		return
	}
	payload, err := buildAntigravityCompactResponseJSON(a.model, summary, a.usage, a.createdAt)
	if err != nil {
		a.failure = errors.New("Failed to encode Antigravity compaction response")
		return
	}
	a.payload = payload
}

func (a *antigravityCompactCollectAdapter) WriteError(_ *antigravityClientWriter, reason string) {
	message := "Antigravity compaction stream failed before a summary was produced"
	if trimmed := strings.TrimSpace(reason); trimmed != "" {
		message += " (" + trimmed + ")"
	}
	if a.failure == nil {
		a.failure = errors.New(message)
	}
}

func (a *antigravityCompactCollectAdapter) setExplicitUpstreamStop(explicit bool) {
	a.explicitStop = explicit
}

func (a *antigravityCompactCollectAdapter) suppressWriterKeepalive() bool {
	return true
}

func (a *antigravityCompactCollectAdapter) finalError() error {
	return a.failure
}

// antigravityCompactFailureReason 是流式与非流式共用的压缩成功判据：必须有
// 非空摘要、上游原始 finishReason 恰为 STOP、且流上出现过显式停止事件。
func antigravityCompactFailureReason(summary string, sawToolUse bool, geminiFinishReason string, explicitStop bool) string {
	finishReason := strings.TrimSpace(geminiFinishReason)
	switch {
	case sawToolUse:
		return "Antigravity compaction returned a tool call instead of a summary"
	case finishReason == "MAX_TOKENS":
		return "Antigravity compaction summary was truncated"
	case finishReason == "":
		return "Antigravity compaction did not report an upstream finish reason"
	case finishReason != "STOP":
		return "Antigravity compaction ended with upstream finish reason " + finishReason
	case strings.TrimSpace(summary) == "":
		return "Antigravity compaction produced an empty summary"
	case !explicitStop:
		return "Antigravity compaction stream ended before an explicit stop reason"
	default:
		return ""
	}
}

// buildAntigravityCompactResponseJSON 组装单 item 压缩响应。encrypted_content
// 是本地可回放封装且被后续请求单独识别；summary 仅作冗余展示，回放不依赖它。
func buildAntigravityCompactResponseJSON(model, summary string, usage ClaudeUsage, createdAt int64) ([]byte, error) {
	if createdAt <= 0 {
		createdAt = time.Now().Unix()
	}
	inputTokens := usage.InputTokens + usage.CacheReadInputTokens + usage.CacheCreationInputTokens
	response := map[string]any{
		"id":         "resp_" + strings.ReplaceAll(uuid.NewString(), "-", ""),
		"object":     "response",
		"created_at": createdAt,
		"model":      model,
		"status":     "completed",
		"output": []any{map[string]any{
			"id":                "cmp_" + strings.ReplaceAll(uuid.NewString(), "-", ""),
			"type":              "compaction",
			"status":            "completed",
			"encrypted_content": encodeAntigravityCompactionOpaque(summary),
			"summary": []any{map[string]any{
				"type": "summary_text",
				"text": summary,
			}},
		}},
		"usage": map[string]any{
			"input_tokens":  inputTokens,
			"output_tokens": usage.OutputTokens,
			"total_tokens":  inputTokens + usage.OutputTokens,
		},
	}
	return marshalOpenAIUpstreamJSON(response)
}

// writeAntigravityCompactStreamFailure 通过客户端 writer 写出 response.failed
// 终止事件（Codex 对压缩流只认这个终止形态），并记录流内错误。
func writeAntigravityCompactStreamFailure(c *gin.Context, writer *antigravityClientWriter, message string) {
	status := http.StatusBadGateway
	if c != nil {
		MarkResponseCommitted(c)
		MarkOpsStreamError(c, "upstream_error", message, status)
	}
	body, err := json.Marshal(map[string]any{
		"type":            "response.failed",
		"sequence_number": 0,
		"response": map[string]any{
			"id":         "resp_" + strings.ReplaceAll(uuid.NewString(), "-", ""),
			"object":     "response",
			"created_at": time.Now().Unix(),
			"status":     "failed",
			"output":     []any{},
			"error": map[string]any{
				"code":    "upstream_error",
				"message": message,
			},
		},
	})
	if err != nil || writer == nil {
		return
	}
	frame := make([]byte, 0, len(body)+32)
	frame = append(frame, "event: response.failed\ndata: "...)
	frame = append(frame, body...)
	frame = append(frame, '\n', '\n')
	writer.Write(frame)
}
