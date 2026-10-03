package service

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"go.uber.org/zap"
)

// Adapted from MACOS-DO/sub4api's request locale rewrite. Only parse field
// boundaries: fields such as subagents may contain opaque, unescaped text.
var openAIEnvironmentFieldStart = regexp.MustCompile(`^<([A-Za-z_][A-Za-z0-9_.:-]*)(?:[ \t\r\n]+(?:[^<>"']|"[^"]*"|'[^']*')*)?[ \t\r\n]*/?>`)

func findOpenAIEnvironmentFieldEnd(text, name string) (start, end int) {
	prefix := "</" + name
	for offset := 0; offset < len(text); {
		index := strings.Index(text[offset:], prefix)
		if index < 0 {
			break
		}
		start = offset + index
		offset = start + len(prefix)
		rest := strings.TrimLeft(text[offset:], " \t\r\n")
		if strings.HasPrefix(rest, ">") {
			return start, len(text) - len(rest) + 1
		}
	}
	return -1, -1
}

func rewriteOpenAIRequestEnvironment(text, timezone, currentDate string) string {
	trimmed := strings.TrimSpace(text)
	root := openAIEnvironmentFieldStart.FindStringSubmatchIndex(trimmed)
	if root == nil || trimmed[root[2]:root[3]] != "environment_context" {
		return text
	}
	rootCloseStart, rootCloseEnd := findOpenAIEnvironmentFieldEnd(trimmed[root[1]:], "environment_context")
	if strings.HasSuffix(trimmed[:root[1]], "/>") || rootCloseEnd != len(trimmed)-root[1] {
		return text
	}
	content := trimmed[root[1] : root[1]+rootCloseStart]
	patches := make([]struct {
		start, end int
		value      string
	}, 0, 2)
	hasTimezone, hasDate := false, false
	for offset := 0; offset < len(content); {
		rest := strings.TrimLeftFunc(content[offset:], unicode.IsSpace)
		offset = len(content) - len(rest)
		if rest == "" {
			break
		}
		field := openAIEnvironmentFieldStart.FindStringSubmatchIndex(rest)
		if field == nil {
			return text
		}
		name := rest[field[2]:field[3]]
		if name == "environment_context" {
			return text
		}
		selfClosing := strings.HasSuffix(rest[:field[1]], "/>")
		if (name == "timezone" && (hasTimezone || selfClosing)) || (name == "current_date" && (hasDate || selfClosing)) {
			return text
		}
		offset += field[1]
		if selfClosing {
			continue
		}
		end, closeEnd := findOpenAIEnvironmentFieldEnd(content[offset:], name)
		if end < 0 {
			return text
		}
		if name == "timezone" || name == "current_date" {
			value := content[offset : offset+end]
			if strings.TrimSpace(value) == "" || strings.Contains(value, "<") {
				return text
			}
			target := timezone
			if name == "timezone" {
				hasTimezone = true
			} else {
				hasDate = true
				target = currentDate
			}
			patches = append(patches, struct {
				start, end int
				value      string
			}{
				start: offset + len(value) - len(strings.TrimLeftFunc(value, unicode.IsSpace)),
				end:   offset + len(strings.TrimRightFunc(value, unicode.IsSpace)),
				value: target,
			})
		}
		offset += closeEnd
	}
	// Never inject fields or change a date without an explicit timezone field.
	if !hasTimezone {
		return text
	}
	base := len(text) - len(strings.TrimLeftFunc(text, unicode.IsSpace)) + root[1]
	for i := len(patches) - 1; i >= 0; i-- {
		patch := patches[i]
		text = text[:base+patch.start] + patch.value + text[base+patch.end:]
	}
	return text
}

func normalizeOpenAIRequestLocale(ctx context.Context, account *Account, body []byte, transport string) []byte {
	if account == nil || !account.IsOpenAIOAuth() || !gjson.ValidBytes(body) {
		return body
	}
	payload := gjson.ParseBytes(body)
	if !payload.IsObject() || hasDuplicateJSONObjectKeys(payload) {
		return body
	}
	timezone := account.OpenAIRequestTimezone()
	location, err := time.LoadLocation(timezone)
	if err != nil {
		return body
	}
	currentDate := time.Now().In(location).Format("2006-01-02")
	original := body
	replaced := 0
	patchFailed := false
	patchText := func(path, before, after string) {
		if before == after || patchFailed {
			return
		}
		updated, err := sjson.SetBytes(body, path, after)
		if err != nil {
			patchFailed = true
			return
		}
		body = updated
		replaced++
	}
	rewriteText := func(path string, text gjson.Result) {
		if text.Type == gjson.String {
			before := text.String()
			patchText(path, before, rewriteOpenAIRequestEnvironment(before, timezone, currentDate))
		}
	}
	input := payload.Get("input")
	if input.IsArray() {
		for inputIndex, item := range input.Array() {
			if !item.IsObject() || hasDuplicateJSONObjectKeys(item) || item.Get("role").String() != "user" {
				continue
			}
			if kind := item.Get("type").String(); kind != "" && kind != "message" {
				continue
			}
			content := item.Get("content")
			path := "input." + strconv.Itoa(inputIndex) + ".content"
			if content.Type == gjson.String {
				rewriteText(path, content)
			} else if content.IsArray() {
				for contentIndex, part := range content.Array() {
					if part.IsObject() && !hasDuplicateJSONObjectKeys(part) && part.Get("type").String() == "input_text" {
						rewriteText(path+"."+strconv.Itoa(contentIndex)+".text", part.Get("text"))
					}
				}
			}
		}
	}
	tools := payload.Get("tools")
	if tools.IsArray() {
		for index, tool := range tools.Array() {
			if !tool.IsObject() || hasDuplicateJSONObjectKeys(tool) {
				continue
			}
			kind := tool.Get("type").String()
			if kind != "web_search" && !strings.HasPrefix(kind, "web_search_") {
				continue
			}
			userLocation := tool.Get("user_location")
			if !userLocation.IsObject() || hasDuplicateJSONObjectKeys(userLocation) {
				continue
			}
			if value := userLocation.Get("timezone"); value.Type == gjson.String {
				patchText("tools."+strconv.Itoa(index)+".user_location.timezone", value.String(), timezone)
			}
		}
	}
	if patchFailed {
		return original
	}
	if replaced > 0 {
		// Do not log client text or previous timezone values.
		logger.FromContext(ctx).Debug("openai request timezone normalization",
			zap.Int64("account_id", account.ID), zap.String("transport", transport),
			zap.String("target_timezone", timezone), zap.Int("replaced_count", replaced))
	}
	return body
}
