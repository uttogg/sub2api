package service

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	coderws "github.com/coder/websocket"
	"github.com/tidwall/gjson"
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

func aliasOpenAIResponsesCollaborationBody(body []byte, account *Account) ([]byte, map[string]string, bool, error) {
	if !account.IsOpenAIResponsesCollaborationEnabled() {
		return body, nil, false, nil
	}
	var req map[string]any
	if err := decodeOpenAIJSONUseNumber(body, &req); err != nil {
		return body, nil, false, err
	}
	changed, err := apicompat.AliasResponsesCollaborationNamespace(req)
	if err != nil {
		return body, nil, false, err
	}
	// Keep namespace aliases distinct from bare function aliases in the
	// existing reverse map, so an unrelated function name cannot be rewritten.
	reverse := map[string]string{"namespace:" + apicompat.ResponsesCollaborationNamespaceAlias: "collaboration"}
	if !changed {
		return body, reverse, false, nil
	}
	normalized, err := marshalOpenAIUpstreamJSON(req)
	if err != nil {
		return body, nil, false, err
	}
	return normalized, reverse, true, nil
}

// The existing WS alias lifecycle owns both turn and session maps, so combine
// collaboration and reserved-name aliases before updating either map.
func aliasOpenAIResponsesToolNamesBodyForAccount(body []byte, account *Account) ([]byte, map[string]string, bool, error) {
	normalized := body
	changed := false
	if account.IsOpenAIResponsesCollaborationEnabled() && gjson.GetBytes(body, "type").String() == "session.update" {
		var err error
		normalized, changed, err = normalizeOpenAIResponsesCollaborationBody(body, account)
		if err != nil {
			return body, nil, false, err
		}
	}
	aliased, reverse, collaborationChanged, err := aliasOpenAIResponsesCollaborationBody(normalized, account)
	if err != nil {
		return body, nil, false, err
	}
	normalized = aliased
	changed = changed || collaborationChanged
	if account.IsOpenAIOAuthLike() {
		aliased, reservedReverse, reservedChanged, err := aliasOpenAIOAuthReservedToolNamesBody(normalized)
		if err != nil {
			return body, nil, false, err
		}
		normalized = aliased
		reverse = mergeCodexToolNameReverseMaps(reverse, reservedReverse)
		changed = changed || reservedChanged
	}
	return normalized, reverse, changed, nil
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
