package service

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	coderws "github.com/coder/websocket"
)

// Account selection precedes this policy: each failover attempt starts with its
// own body, so an enabled account cannot opt other accounts into the conversion.
// HTTP and WS ingress already enforce their respective request size limits.
func normalizeOpenAIResponsesCollaborationBody(body []byte, account *Account) ([]byte, bool, error) {
	if !account.IsOpenAIResponsesCollaborationEnabled() {
		return body, false, nil
	}
	var req map[string]any
	if err := decodeOpenAIJSONUseNumber(body, &req); err != nil {
		return body, false, err
	}
	changed, err := apicompat.NormalizeResponsesCollaboration(req)
	if err != nil || !changed {
		return body, false, err
	}
	normalized, err := marshalOpenAIUpstreamJSON(req)
	if err != nil {
		return body, false, err
	}
	return normalized, true, nil
}

func writeOpenAIResponsesCollaborationWSError(ctx context.Context, conn *coderws.Conn, err error) {
	var clientErr *apicompat.ResponsesCollaborationError
	if !errors.As(err, &clientErr) {
		return
	}
	payload, marshalErr := marshalOpenAIUpstreamJSON(map[string]any{
		"type": "error", "status": http.StatusBadRequest,
		"error": map[string]any{
			"type": "invalid_request_error", "code": clientErr.Code,
			"param": clientErr.Param, "message": clientErr.Message,
		},
	})
	if marshalErr != nil {
		return
	}
	writeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_ = conn.Write(writeCtx, coderws.MessageText, payload)
}
