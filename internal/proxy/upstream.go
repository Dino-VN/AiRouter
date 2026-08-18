package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"aihub/internal/model"
)

// maxErrorBody bounds how much of a failed upstream response is read back.
const maxErrorBody = 64 << 10

// sendOptions tells an executor how the response will be consumed.
type sendOptions struct {
	// Raw asks for passthrough mode: the provider's own frames reach the client,
	// so the executor should forward the client body (sanitised) instead of
	// re-rendering it from the canonical request.
	Raw bool
}

// executor talks to one provider. It owns the request shape, the response
// decoding and the provider-specific headers; the router owns connection
// selection, retries and accounting.
type executor interface {
	// providerID is the connection type this executor serves.
	providerID() model.Provider
	// passthrough reports whether a client format maps onto the provider protocol
	// closely enough to forward frames verbatim.
	passthrough(format Format) bool
	// send performs the upstream call. Every failure is an *APIError so the router
	// can decide whether to try another connection.
	send(ctx context.Context, conn *model.Connection, req *Request, opts sendOptions) (*upstreamStream, error)
}

// upstreamStream is an accepted provider response being consumed. Executors
// build one by supplying the decoding closures; the frame loop is shared.
type upstreamStream struct {
	// Header is the upstream response header, used for quota scraping.
	Header http.Header
	// Body must be closed by the consumer.
	Body io.ReadCloser
	// providerID labels errors raised while the stream is being consumed.
	providerID model.Provider

	scanner *sseScanner
	// pre holds frames synthesised from a non-SSE response body.
	pre []sseEvent

	// decode converts one provider frame into canonical events.
	decode func(sseEvent) ([]Event, error)
	// trailer produces the closing canonical events once the frames run out.
	trailer func() []Event
	// rewrite adapts a provider frame for passthrough. Returning false drops it.
	rewrite func(sseEvent) (sseEvent, bool)
	// sniff extracts usage from a raw frame, so passthrough still bills tokens.
	sniff func(sseEvent) *Usage
	// aggregate is the object a non-streaming passthrough client receives.
	aggregate json.RawMessage

	usage   Usage
	queue   []Event
	trailed bool
}

// Next returns the next canonical event, or ok=false at end of stream.
func (s *upstreamStream) Next() (Event, bool, error) {
	for {
		if len(s.queue) > 0 {
			ev := s.queue[0]
			s.queue = s.queue[1:]
			return ev, true, nil
		}

		frame, ok, err := s.nextFrame()
		if err != nil {
			return Event{}, false, err
		}
		if !ok {
			if s.trailed {
				return Event{}, false, nil
			}
			s.trailed = true
			if s.trailer != nil {
				s.queue = s.trailer()
			}
			continue
		}
		if s.decode == nil {
			continue
		}
		events, err := s.decode(frame)
		if err != nil {
			return Event{}, false, err
		}
		s.queue = append(s.queue, events...)
	}
}

// NextRaw returns the next provider frame, already adapted for passthrough.
func (s *upstreamStream) NextRaw() (sseEvent, bool, error) {
	for {
		frame, ok, err := s.nextFrame()
		if err != nil || !ok {
			return sseEvent{}, false, err
		}
		if s.sniff != nil {
			if usage := s.sniff(frame); usage != nil {
				s.usage = *usage
			}
		}
		if s.rewrite != nil {
			rewritten, keep := s.rewrite(frame)
			if !keep {
				continue
			}
			return rewritten, true, nil
		}
		return frame, true, nil
	}
}

// observedUsage is the token accounting seen while forwarding raw frames.
func (s *upstreamStream) observedUsage() Usage { return s.usage }

func (s *upstreamStream) nextFrame() (sseEvent, bool, error) {
	if len(s.pre) > 0 {
		frame := s.pre[0]
		s.pre = s.pre[1:]
		return frame, true, nil
	}
	if s.scanner == nil {
		return sseEvent{}, false, nil
	}
	for s.scanner.Scan() {
		frame := s.scanner.Event()
		data := strings.TrimSpace(frame.Data)
		if data == "" || data == "[DONE]" {
			continue
		}
		return frame, true, nil
	}
	return sseEvent{}, false, s.scanner.Err()
}

func (s *upstreamStream) Close() {
	if s.Body != nil {
		// Draining is deliberately skipped: an abandoned stream may be huge, and
		// closing an HTTP/2 body cancels the stream cleanly.
		_ = s.Body.Close()
		s.Body = nil
	}
}

// ---------------------------------------------------------------------------
// Errors
// ---------------------------------------------------------------------------

// apiErrorFromResponse maps a non-2xx upstream reply onto a client-facing error.
// The upstream message is preserved because it usually explains the real cause
// (an unsupported model, a rate limit window, a revoked token).
func apiErrorFromResponse(provider model.Provider, status int, header http.Header, body []byte) *APIError {
	message, code, errType := parseUpstreamError(body)
	if message == "" {
		message = strings.TrimSpace(string(body))
	}
	if message == "" {
		message = http.StatusText(status)
	}

	apiErr := &APIError{
		Status:    clientStatusFor(status),
		Type:      firstNonEmpty(errType, typeForStatus(status)),
		Code:      code,
		Message:   fmt.Sprintf("%s: %s", provider, truncate(message, 2000)),
		Upstream:  status,
		Retryable: retryableStatus(status),
	}
	if status == http.StatusTooManyRequests {
		if retry := header.Get("Retry-After"); retry != "" {
			apiErr.Message += " (retry-after " + retry + ")"
		}
	}
	return apiErr
}

// parseUpstreamError digs the human-readable message out of the several error
// envelopes the two providers use.
func parseUpstreamError(body []byte) (message, code, errType string) {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" || (!strings.HasPrefix(trimmed, "{") && !strings.HasPrefix(trimmed, "[")) {
		return "", "", ""
	}

	// Google APIs answer errors as an array of one object on streaming endpoints.
	if strings.HasPrefix(trimmed, "[") {
		var many []json.RawMessage
		if err := json.Unmarshal([]byte(trimmed), &many); err == nil && len(many) > 0 {
			return parseUpstreamError(many[0])
		}
		return "", "", ""
	}

	var envelope struct {
		Error json.RawMessage `json:"error"`
		// Codex sometimes answers {"detail": "..."}.
		Detail  json.RawMessage `json:"detail"`
		Message string          `json:"message"`
	}
	if err := json.Unmarshal([]byte(trimmed), &envelope); err != nil {
		return "", "", ""
	}

	if len(envelope.Error) > 0 {
		var text string
		if err := json.Unmarshal(envelope.Error, &text); err == nil {
			return text, "", ""
		}
		var object struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    any    `json:"code"`
			Status  string `json:"status"`
		}
		if err := json.Unmarshal(envelope.Error, &object); err == nil {
			return object.Message, stringifyCode(object.Code, object.Status), object.Type
		}
	}
	if len(envelope.Detail) > 0 {
		var text string
		if err := json.Unmarshal(envelope.Detail, &text); err == nil {
			return text, "", ""
		}
		return string(envelope.Detail), "", ""
	}
	return envelope.Message, "", ""
}

func stringifyCode(code any, status string) string {
	switch value := code.(type) {
	case string:
		if value != "" {
			return value
		}
	case float64:
		if value != 0 {
			return fmt.Sprintf("%.0f", value)
		}
	}
	return status
}

// clientStatusFor hides upstream authentication problems behind 502: the client
// key was valid, it is this server's stored credential that failed.
func clientStatusFor(status int) int {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return http.StatusBadGateway
	case http.StatusBadRequest, http.StatusNotFound, http.StatusRequestEntityTooLarge,
		http.StatusTooManyRequests, http.StatusUnprocessableEntity:
		return status
	}
	if status >= 500 {
		return http.StatusBadGateway
	}
	return status
}

func typeForStatus(status int) string {
	switch {
	case status == http.StatusTooManyRequests:
		return "rate_limit_error"
	case status == http.StatusBadRequest || status == http.StatusUnprocessableEntity:
		return "invalid_request_error"
	case status >= 500:
		return "api_error"
	default:
		return "api_error"
	}
}

// retryableStatus reports whether another connection is worth trying.
func retryableStatus(status int) bool {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests,
		http.StatusRequestTimeout, http.StatusConflict:
		return true
	}
	return status >= 500
}

// asAPIError converts a transport-level failure into a retryable *APIError.
func asAPIError(provider model.Provider, err error) *APIError {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr
	}
	if errors.Is(err, context.Canceled) {
		return &APIError{
			Status:  499,
			Type:    "request_cancelled",
			Message: "client closed the request",
		}
	}
	status := http.StatusBadGateway
	if errors.Is(err, context.DeadlineExceeded) {
		status = http.StatusGatewayTimeout
	}
	return &APIError{
		Status:    status,
		Type:      "api_error",
		Message:   fmt.Sprintf("%s: %s", provider, err.Error()),
		Retryable: true,
	}
}

// readErrorBody reads a bounded amount of a failed response.
func readErrorBody(body io.Reader) []byte {
	data, _ := io.ReadAll(io.LimitReader(body, maxErrorBody))
	return data
}

func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "…"
}

// ---------------------------------------------------------------------------
// JSON schema sanitising
// ---------------------------------------------------------------------------

// geminiSchemaKeys is the OpenAPI subset the Gemini and Antigravity endpoints
// accept. Anything else makes them reject the whole request, so unknown keywords
// from Anthropic or OpenAI tool schemas are dropped rather than forwarded.
var geminiSchemaKeys = map[string]bool{
	"type": true, "format": true, "title": true, "description": true,
	"nullable": true, "enum": true, "items": true, "properties": true,
	"required": true, "minimum": true, "maximum": true, "minItems": true,
	"maxItems": true, "minLength": true, "maxLength": true, "pattern": true,
	"anyOf": true, "default": true, "example": true, "propertyOrdering": true,
	"minProperties": true, "maxProperties": true,
}

// geminiFormats lists the format values Gemini understands, per type.
var geminiFormats = map[string]map[string]bool{
	"string":  {"enum": true, "date-time": true},
	"integer": {"int32": true, "int64": true},
	"number":  {"float": true, "double": true},
}

// sanitizeGeminiSchema rewrites a JSON Schema into the dialect the Google
// endpoints accept. It returns nil when nothing usable is left.
func sanitizeGeminiSchema(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil
	}
	cleaned := sanitizeSchemaNode(decoded)
	if cleaned == nil {
		return nil
	}
	encoded, err := json.Marshal(cleaned)
	if err != nil {
		return nil
	}
	return encoded
}

func sanitizeSchemaNode(node any) any {
	object, ok := node.(map[string]any)
	if !ok {
		// Arrays of schemas (anyOf members) and scalars pass through.
		if list, isList := node.([]any); isList {
			out := make([]any, 0, len(list))
			for _, item := range list {
				if cleaned := sanitizeSchemaNode(item); cleaned != nil {
					out = append(out, cleaned)
				}
			}
			return out
		}
		return node
	}

	out := map[string]any{}

	// allOf has no equivalent: flatten its members into the parent so at least
	// the constraints that do translate survive.
	if members, ok := object["allOf"].([]any); ok {
		for _, member := range members {
			if merged, isObject := sanitizeSchemaNode(member).(map[string]any); isObject {
				for key, value := range merged {
					if _, exists := out[key]; !exists {
						out[key] = value
					}
				}
			}
		}
	}
	// oneOf is spelled anyOf.
	if members, ok := object["oneOf"]; ok {
		if _, exists := object["anyOf"]; !exists {
			object["anyOf"] = members
		}
	}
	// const is a single-value enum.
	if value, ok := object["const"]; ok {
		if _, exists := object["enum"]; !exists {
			object["enum"] = []any{value}
		}
	}

	for key, value := range object {
		if !geminiSchemaKeys[key] {
			continue
		}
		switch key {
		case "type":
			resolved, nullable := normalizeSchemaType(value)
			if resolved != "" {
				out["type"] = resolved
			}
			if nullable {
				out["nullable"] = true
			}
		case "properties":
			properties, isObject := value.(map[string]any)
			if !isObject {
				continue
			}
			cleaned := map[string]any{}
			for name, sub := range properties {
				if sanitized := sanitizeSchemaNode(sub); sanitized != nil {
					cleaned[name] = sanitized
				}
			}
			if len(cleaned) > 0 {
				out["properties"] = cleaned
			}
		case "items", "anyOf":
			if sanitized := sanitizeSchemaNode(value); sanitized != nil {
				out[key] = sanitized
			}
		case "enum":
			// Gemini only accepts string enums.
			list, isList := value.([]any)
			if !isList {
				continue
			}
			values := make([]any, 0, len(list))
			for _, item := range list {
				values = append(values, fmt.Sprintf("%v", item))
			}
			if len(values) > 0 {
				out["enum"] = values
			}
		default:
			out[key] = value
		}
	}

	// A format the endpoint does not know is a hard error, so unknown ones go.
	if format, ok := out["format"].(string); ok {
		declared, _ := out["type"].(string)
		if allowed := geminiFormats[declared]; !allowed[format] {
			delete(out, "format")
		}
	}
	// An object with no properties is rejected; describing it as a free-form
	// object is the closest legal equivalent.
	if declared, _ := out["type"].(string); declared == "object" {
		if properties, ok := out["properties"].(map[string]any); !ok || len(properties) == 0 {
			delete(out, "properties")
			delete(out, "required")
		}
	}
	// Drop required entries that no longer have a property.
	if required, ok := out["required"].([]any); ok {
		properties, _ := out["properties"].(map[string]any)
		kept := make([]any, 0, len(required))
		for _, name := range required {
			key, isString := name.(string)
			if !isString {
				continue
			}
			if _, exists := properties[key]; exists {
				kept = append(kept, key)
			}
		}
		if len(kept) == 0 {
			delete(out, "required")
		} else {
			out["required"] = kept
		}
	}
	// Enums carry their own type in Gemini's dialect.
	if _, ok := out["enum"]; ok {
		if _, hasType := out["type"]; !hasType {
			out["type"] = "string"
		}
	}

	if len(out) == 0 {
		return nil
	}
	return out
}

// normalizeSchemaType collapses JSON Schema's union types ("string" or null)
// into a single type plus the nullable flag.
func normalizeSchemaType(value any) (string, bool) {
	switch typed := value.(type) {
	case string:
		return strings.ToLower(typed), false
	case []any:
		var (
			resolved string
			nullable bool
		)
		for _, item := range typed {
			name, isString := item.(string)
			if !isString {
				continue
			}
			if strings.EqualFold(name, "null") {
				nullable = true
				continue
			}
			if resolved == "" {
				resolved = strings.ToLower(name)
			}
		}
		return resolved, nullable
	default:
		return "", false
	}
}
