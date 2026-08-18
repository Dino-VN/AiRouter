package proxy

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// Request parsing
// ---------------------------------------------------------------------------

type chatRequestBody struct {
	Model    string            `json:"model"`
	Messages []chatMessageBody `json:"messages"`
	Tools    []struct {
		Type     string `json:"type"`
		Function struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Parameters  json.RawMessage `json:"parameters"`
		} `json:"function"`
	} `json:"tools"`
	ToolChoice    json.RawMessage `json:"tool_choice"`
	Temperature   *float64        `json:"temperature"`
	TopP          *float64        `json:"top_p"`
	MaxTokens     *int            `json:"max_tokens"`
	MaxCompletion *int            `json:"max_completion_tokens"`
	Stop          json.RawMessage `json:"stop"`
	Stream        bool            `json:"stream"`
	StreamOptions *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
	ReasoningEffort string `json:"reasoning_effort"`
	ResponseFormat  *struct {
		Type       string `json:"type"`
		JSONSchema *struct {
			Name   string          `json:"name"`
			Schema json.RawMessage `json:"schema"`
		} `json:"json_schema"`
	} `json:"response_format"`
	ParallelToolCalls *bool `json:"parallel_tool_calls"`
}

type chatMessageBody struct {
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content"`
	Name      string          `json:"name"`
	ToolCalls []struct {
		ID       string `json:"id"`
		Type     string `json:"type"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	} `json:"tool_calls"`
	ToolCallID       string `json:"tool_call_id"`
	ReasoningContent string `json:"reasoning_content"`
}

// parseOpenAIChat converts a /v1/chat/completions body to canonical form.
func parseOpenAIChat(raw []byte) (*Request, error) {
	var body chatRequestBody
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, newAPIError(http.StatusBadRequest, "invalid_request_error",
			"could not parse request body: "+err.Error())
	}
	if strings.TrimSpace(body.Model) == "" {
		return nil, newAPIError(http.StatusBadRequest, "invalid_request_error", "model is required")
	}

	req := &Request{
		ClientFormat:    FormatOpenAIChat,
		Model:           normalizeModel(body.Model),
		Stream:          body.Stream,
		Temperature:     body.Temperature,
		TopP:            body.TopP,
		MaxOutputTokens: firstNonNilInt(body.MaxCompletion, body.MaxTokens),
		ParallelToolUse: body.ParallelToolCalls,
		Raw:             raw,
	}
	req.Stop = parseStopSequences(body.Stop)

	if body.ReasoningEffort != "" {
		req.Reasoning = Reasoning{Effort: body.ReasoningEffort, Enabled: true, IncludeThoughts: true}
	}
	if body.ResponseFormat != nil {
		switch body.ResponseFormat.Type {
		case "json_object":
			req.ResponseMIMEType = "application/json"
		case "json_schema":
			req.ResponseMIMEType = "application/json"
			if body.ResponseFormat.JSONSchema != nil {
				req.ResponseSchema = body.ResponseFormat.JSONSchema.Schema
			}
		}
	}

	for _, tool := range body.Tools {
		if tool.Function.Name == "" {
			continue
		}
		req.Tools = append(req.Tools, Tool{
			Name:        tool.Function.Name,
			Description: tool.Function.Description,
			Parameters:  tool.Function.Parameters,
		})
	}
	req.ToolChoice = parseOpenAIToolChoice(body.ToolChoice)

	for _, msg := range body.Messages {
		parts, err := parseChatContent(msg.Content)
		if err != nil {
			return nil, err
		}

		switch strings.ToLower(msg.Role) {
		case "system", "developer":
			req.System = append(req.System, parts...)
			continue
		case "tool", "function":
			name := msg.Name
			text := partsText(parts)
			req.Messages = append(req.Messages, Message{Role: RoleTool, Parts: []Part{{
				Type:       PartToolResult,
				ToolCallID: msg.ToolCallID,
				ToolName:   name,
				Text:       text,
			}}})
			continue
		case "assistant":
			assistant := Message{Role: RoleAssistant}
			if msg.ReasoningContent != "" {
				assistant.Parts = append(assistant.Parts,
					Part{Type: PartThinking, Text: msg.ReasoningContent})
			}
			assistant.Parts = append(assistant.Parts, parts...)
			for _, call := range msg.ToolCalls {
				assistant.Parts = append(assistant.Parts, Part{
					Type:       PartToolCall,
					ToolCallID: call.ID,
					ToolName:   call.Function.Name,
					Arguments:  call.Function.Arguments,
				})
			}
			if len(assistant.Parts) > 0 {
				req.Messages = append(req.Messages, assistant)
			}
			continue
		default:
			if len(parts) > 0 {
				req.Messages = append(req.Messages, Message{Role: RoleUser, Parts: parts})
			}
		}
	}

	if len(req.Messages) == 0 && len(req.System) == 0 {
		return nil, newAPIError(http.StatusBadRequest, "invalid_request_error", "messages must not be empty")
	}
	return req, nil
}

// parseChatContent handles both the string and the array form of `content`.
func parseChatContent(raw json.RawMessage) ([]Part, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}

	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		if text == "" {
			return nil, nil
		}
		return []Part{{Type: PartText, Text: text}}, nil
	}

	var items []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL *struct {
			URL string `json:"url"`
		} `json:"image_url"`
		InputAudio json.RawMessage `json:"input_audio"`
	}
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, newAPIError(http.StatusBadRequest, "invalid_request_error",
			"unsupported message content: "+err.Error())
	}

	var parts []Part
	for _, item := range items {
		switch item.Type {
		case "text", "input_text", "output_text":
			if item.Text != "" {
				parts = append(parts, Part{Type: PartText, Text: item.Text})
			}
		case "image_url", "input_image":
			if item.ImageURL == nil || item.ImageURL.URL == "" {
				continue
			}
			parts = append(parts, imagePartFromURL(item.ImageURL.URL))
		default:
			// Unknown parts (audio, file) are dropped rather than failing the
			// request: neither upstream accepts them.
		}
	}
	return parts, nil
}

// imagePartFromURL turns a data: URL or a remote URL into an image part.
func imagePartFromURL(url string) Part {
	if strings.HasPrefix(url, "data:") {
		if mime, data, ok := splitDataURL(url); ok {
			return Part{Type: PartImage, MimeType: mime, Data: data}
		}
	}
	return Part{Type: PartImage, URL: url}
}

// splitDataURL parses "data:image/png;base64,AAAA".
func splitDataURL(url string) (mime, data string, ok bool) {
	rest, found := strings.CutPrefix(url, "data:")
	if !found {
		return "", "", false
	}
	head, payload, found := strings.Cut(rest, ",")
	if !found {
		return "", "", false
	}
	mime = strings.TrimSuffix(head, ";base64")
	if mime == "" {
		mime = "image/png"
	}
	// Validate lazily: an unparsable payload is better dropped than forwarded.
	if _, err := base64.StdEncoding.DecodeString(payload); err != nil {
		return "", "", false
	}
	return mime, payload, true
}

func parseOpenAIToolChoice(raw json.RawMessage) ToolChoice {
	if len(raw) == 0 {
		return ToolChoice{}
	}
	var mode string
	if err := json.Unmarshal(raw, &mode); err == nil {
		switch mode {
		case "auto", "none", "required":
			return ToolChoice{Mode: mode}
		}
		return ToolChoice{}
	}
	var object struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &object); err == nil && object.Function.Name != "" {
		return ToolChoice{Mode: "tool", Name: object.Function.Name}
	}
	return ToolChoice{}
}

// ---------------------------------------------------------------------------
// Response rendering
// ---------------------------------------------------------------------------

// chatRenderer renders canonical events as OpenAI chat completions.
type chatRenderer struct {
	w      http.ResponseWriter
	sse    *sseWriter
	stream bool

	id      string
	created int64
	model   string

	sentRole bool
	text     strings.Builder
	thinking strings.Builder
	// calls holds pointers because each entry owns a strings.Builder, and a
	// Builder that has been written to must never be copied — which is exactly
	// what append does when the slice grows.
	calls   []*renderedToolCall
	finish  string
	usage   Usage
	started bool
}

// renderedToolCall accumulates a streamed tool call.
type renderedToolCall struct {
	ID        string
	Name      string
	Arguments strings.Builder
	Index     int
	announced bool
}

func newChatRenderer(w http.ResponseWriter, stream bool) *chatRenderer {
	return &chatRenderer{
		w:       w,
		stream:  stream,
		id:      "chatcmpl-" + strings.ReplaceAll(uuid.NewString(), "-", ""),
		created: time.Now().Unix(),
	}
}

func (r *chatRenderer) Begin(model string) {
	r.model = model
	if r.stream {
		r.sse = newSSEWriter(r.w)
		r.sse.WriteHeader(http.StatusOK)
	}
}

func (r *chatRenderer) Handle(ev Event) error {
	if ev.Model != "" {
		r.model = ev.Model
	}

	switch ev.Type {
	case EventStart:
		r.started = true
	case EventText:
		if ev.Text == "" {
			return nil
		}
		r.text.WriteString(ev.Text)
		if r.stream {
			r.emitDelta(map[string]any{"content": ev.Text})
		}
	case EventThinking:
		if ev.Text == "" {
			return nil
		}
		r.thinking.WriteString(ev.Text)
		if r.stream {
			r.emitDelta(map[string]any{"reasoning_content": ev.Text})
		}
	case EventToolCallStart:
		call := &renderedToolCall{ID: ev.ToolCallID, Name: ev.ToolName, Index: len(r.calls)}
		if call.ID == "" {
			call.ID = "call_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:24]
		}
		r.calls = append(r.calls, call)
		if r.stream {
			r.emitToolDelta(len(r.calls)-1, call.ID, call.Name, "")
			r.calls[len(r.calls)-1].announced = true
		}
	case EventToolCallDelta:
		idx := r.toolIndex(ev)
		if idx < 0 {
			return nil
		}
		r.calls[idx].Arguments.WriteString(ev.Arguments)
		if r.stream {
			r.emitToolDelta(idx, "", "", ev.Arguments)
		}
	case EventToolCallDone:
		idx := r.toolIndex(ev)
		if idx < 0 {
			return nil
		}
		// Providers that deliver whole calls at once put everything here.
		if r.calls[idx].Arguments.Len() == 0 && ev.Arguments != "" {
			r.calls[idx].Arguments.WriteString(ev.Arguments)
			if r.stream {
				r.emitToolDelta(idx, "", "", ev.Arguments)
			}
		}
		if r.finish == "" {
			r.finish = FinishToolCalls
		}
	case EventUsage:
		if ev.Usage != nil {
			r.usage = *ev.Usage
		}
	case EventDone:
		if ev.Usage != nil {
			r.usage = *ev.Usage
		}
		if ev.FinishReason != "" {
			r.finish = ev.FinishReason
		}
	}
	return nil
}

// toolIndex resolves which accumulated call an event refers to.
func (r *chatRenderer) toolIndex(ev Event) int {
	if len(r.calls) == 0 {
		// Some providers emit deltas without a start event.
		if ev.Type != EventToolCallStart {
			r.calls = append(r.calls, &renderedToolCall{
				ID:    firstNonEmpty(ev.ToolCallID, "call_"+strings.ReplaceAll(uuid.NewString(), "-", "")[:24]),
				Name:  ev.ToolName,
				Index: 0,
			})
			if r.stream {
				r.emitToolDelta(0, r.calls[0].ID, r.calls[0].Name, "")
				r.calls[0].announced = true
			}
			return 0
		}
		return -1
	}
	if ev.ToolCallID != "" {
		for i := range r.calls {
			if r.calls[i].ID == ev.ToolCallID {
				return i
			}
		}
	}
	return len(r.calls) - 1
}

func (r *chatRenderer) emitDelta(delta map[string]any) {
	if !r.sentRole {
		delta["role"] = "assistant"
		r.sentRole = true
	}
	r.emitChunk(map[string]any{
		"index": 0,
		"delta": delta,
	}, nil)
}

func (r *chatRenderer) emitToolDelta(index int, id, name, arguments string) {
	call := map[string]any{"index": index}
	if id != "" {
		call["id"] = id
		call["type"] = "function"
	}
	fn := map[string]any{}
	if name != "" {
		fn["name"] = name
	}
	if arguments != "" {
		fn["arguments"] = arguments
	}
	if len(fn) > 0 {
		call["function"] = fn
	}

	delta := map[string]any{"tool_calls": []any{call}}
	if !r.sentRole {
		delta["role"] = "assistant"
		r.sentRole = true
	}
	r.emitChunk(map[string]any{"index": 0, "delta": delta}, nil)
}

func (r *chatRenderer) emitChunk(choice map[string]any, usage any) {
	payload := map[string]any{
		"id":      r.id,
		"object":  "chat.completion.chunk",
		"created": r.created,
		"model":   r.model,
		"choices": []any{choice},
	}
	if usage != nil {
		payload["usage"] = usage
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return
	}
	r.sse.Data(string(encoded))
}

func (r *chatRenderer) Finish() error {
	if r.finish == "" {
		r.finish = FinishStop
	}

	if r.stream {
		r.emitChunk(map[string]any{
			"index":         0,
			"delta":         map[string]any{},
			"finish_reason": r.finish,
		}, nil)
		// A final usage-only chunk matches what OpenAI sends when
		// stream_options.include_usage is set; clients that do not expect it
		// ignore the empty choices array.
		r.emitChunk(map[string]any{"index": 0, "delta": map[string]any{}}, r.usagePayload())
		r.sse.Data("[DONE]")
		return r.sse.Err()
	}

	message := map[string]any{"role": "assistant"}
	if r.text.Len() > 0 {
		message["content"] = r.text.String()
	} else {
		message["content"] = nil
	}
	if r.thinking.Len() > 0 {
		message["reasoning_content"] = r.thinking.String()
	}
	if len(r.calls) > 0 {
		calls := make([]any, 0, len(r.calls))
		for i := range r.calls {
			calls = append(calls, map[string]any{
				"id":   r.calls[i].ID,
				"type": "function",
				"function": map[string]any{
					"name":      r.calls[i].Name,
					"arguments": defaultArguments(r.calls[i].Arguments.String()),
				},
			})
		}
		message["tool_calls"] = calls
	}

	payload := map[string]any{
		"id":      r.id,
		"object":  "chat.completion",
		"created": r.created,
		"model":   r.model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       message,
			"finish_reason": r.finish,
		}},
		"usage": r.usagePayload(),
	}
	return writeJSON(r.w, http.StatusOK, payload)
}

func (r *chatRenderer) usagePayload() map[string]any {
	usage := map[string]any{
		"prompt_tokens":     r.usage.PromptTokens,
		"completion_tokens": r.usage.CompletionTokens,
		"total_tokens":      r.usage.Total(),
	}
	if r.usage.ReasoningTokens > 0 {
		usage["completion_tokens_details"] = map[string]any{
			"reasoning_tokens": r.usage.ReasoningTokens,
		}
	}
	if r.usage.CachedTokens > 0 {
		usage["prompt_tokens_details"] = map[string]any{
			"cached_tokens": r.usage.CachedTokens,
		}
	}
	return usage
}

func (r *chatRenderer) Usage() Usage { return r.usage }

// WriteError renders an error in the OpenAI shape. When the stream has already
// started the error is delivered as a final SSE frame instead of a status code.
func (r *chatRenderer) WriteError(apiErr *APIError) {
	payload := map[string]any{"error": map[string]any{
		"message": apiErr.Message,
		"type":    firstNonEmpty(apiErr.Type, "api_error"),
		"code":    apiErr.Code,
	}}
	if r.stream && r.sse != nil {
		encoded, err := json.Marshal(payload)
		if err == nil {
			r.sse.Data(string(encoded))
		}
		r.sse.Data("[DONE]")
		return
	}
	_ = writeJSON(r.w, apiErr.Status, payload)
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func defaultArguments(s string) string {
	if strings.TrimSpace(s) == "" {
		return "{}"
	}
	return s
}

func partsText(parts []Part) string {
	var b strings.Builder
	for _, part := range parts {
		if part.Type == PartText && part.Text != "" {
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(part.Text)
		}
	}
	return b.String()
}

func parseStopSequences(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var single string
	if err := json.Unmarshal(raw, &single); err == nil {
		if single == "" {
			return nil
		}
		return []string{single}
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err == nil {
		return many
	}
	return nil
}

func firstNonNilInt(values ...*int) *int {
	for _, v := range values {
		if v != nil {
			return v
		}
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func normalizeModel(id string) string {
	id = strings.TrimSpace(id)
	id = strings.TrimPrefix(id, "models/")
	return id
}

func writeJSON(w http.ResponseWriter, status int, payload any) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode response: %w", err)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, err = w.Write(encoded)
	return err
}
