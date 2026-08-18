package proxy

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// Request parsing
// ---------------------------------------------------------------------------

type responsesRequestBody struct {
	Model           string          `json:"model"`
	Instructions    string          `json:"instructions"`
	Input           json.RawMessage `json:"input"`
	Tools           json.RawMessage `json:"tools"`
	ToolChoice      json.RawMessage `json:"tool_choice"`
	Temperature     *float64        `json:"temperature"`
	TopP            *float64        `json:"top_p"`
	MaxOutputTokens *int            `json:"max_output_tokens"`
	Stream          bool            `json:"stream"`
	Reasoning       *struct {
		Effort  string `json:"effort"`
		Summary string `json:"summary"`
	} `json:"reasoning"`
	Text *struct {
		Format *struct {
			Type   string          `json:"type"`
			Name   string          `json:"name"`
			Schema json.RawMessage `json:"schema"`
		} `json:"format"`
	} `json:"text"`
	ParallelToolCalls *bool `json:"parallel_tool_calls"`
}

// responsesInputItem covers every item shape the Responses API accepts.
type responsesInputItem struct {
	Type    string          `json:"type"`
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`

	// Function calls and their results.
	CallID    string          `json:"call_id"`
	Name      string          `json:"name"`
	Arguments string          `json:"arguments"`
	Output    json.RawMessage `json:"output"`

	// Reasoning items.
	Summary []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"summary"`
	EncryptedContent string `json:"encrypted_content"`
}

// parseOpenAIResponses converts a /v1/responses body to canonical form.
func parseOpenAIResponses(raw []byte) (*Request, error) {
	var body responsesRequestBody
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, newAPIError(http.StatusBadRequest, "invalid_request_error",
			"could not parse request body: "+err.Error())
	}
	if strings.TrimSpace(body.Model) == "" {
		return nil, newAPIError(http.StatusBadRequest, "invalid_request_error", "model is required")
	}

	req := &Request{
		ClientFormat:    FormatOpenAIResponses,
		Model:           normalizeModel(body.Model),
		Stream:          body.Stream,
		Temperature:     body.Temperature,
		TopP:            body.TopP,
		MaxOutputTokens: body.MaxOutputTokens,
		ParallelToolUse: body.ParallelToolCalls,
		Raw:             raw,
	}
	if body.Instructions != "" {
		req.System = append(req.System, Part{Type: PartText, Text: body.Instructions})
	}
	if body.Reasoning != nil {
		req.Reasoning = Reasoning{
			Effort:          body.Reasoning.Effort,
			Enabled:         true,
			IncludeThoughts: body.Reasoning.Summary != "" && body.Reasoning.Summary != "none",
		}
	}
	if body.Text != nil && body.Text.Format != nil {
		switch body.Text.Format.Type {
		case "json_object":
			req.ResponseMIMEType = "application/json"
		case "json_schema":
			req.ResponseMIMEType = "application/json"
			req.ResponseSchema = body.Text.Format.Schema
		}
	}
	req.Tools = parseResponsesTools(body.Tools)
	req.ToolChoice = parseResponsesToolChoice(body.ToolChoice)

	if err := appendResponsesInput(req, body.Input); err != nil {
		return nil, err
	}
	if len(req.Messages) == 0 && len(req.System) == 0 {
		return nil, newAPIError(http.StatusBadRequest, "invalid_request_error", "input must not be empty")
	}
	return req, nil
}

// appendResponsesInput handles the string form and the item-array form of input.
func appendResponsesInput(req *Request, raw json.RawMessage) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}

	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		if text != "" {
			req.Messages = append(req.Messages, Message{
				Role:  RoleUser,
				Parts: []Part{{Type: PartText, Text: text}},
			})
		}
		return nil
	}

	var items []responsesInputItem
	if err := json.Unmarshal(raw, &items); err != nil {
		return newAPIError(http.StatusBadRequest, "invalid_request_error",
			"could not parse input: "+err.Error())
	}

	for _, item := range items {
		switch item.Type {
		case "function_call", "custom_tool_call":
			req.Messages = appendToAssistant(req.Messages, Part{
				Type:       PartToolCall,
				ToolCallID: item.CallID,
				ToolName:   item.Name,
				Arguments:  item.Arguments,
			})
		case "function_call_output", "custom_tool_call_output":
			req.Messages = append(req.Messages, Message{Role: RoleTool, Parts: []Part{{
				Type:       PartToolResult,
				ToolCallID: item.CallID,
				Text:       rawToText(item.Output),
			}}})
		case "reasoning":
			var b strings.Builder
			for _, summary := range item.Summary {
				if summary.Text == "" {
					continue
				}
				if b.Len() > 0 {
					b.WriteString("\n")
				}
				b.WriteString(summary.Text)
			}
			if b.Len() == 0 && item.EncryptedContent == "" {
				continue
			}
			req.Messages = appendToAssistant(req.Messages, Part{
				Type:      PartThinking,
				Text:      b.String(),
				Signature: item.EncryptedContent,
			})
		default:
			// "message" or a bare {role, content} pair.
			parts, err := parseResponsesContent(item.Content)
			if err != nil {
				return err
			}
			if len(parts) == 0 {
				continue
			}
			switch strings.ToLower(item.Role) {
			case "system", "developer":
				req.System = append(req.System, parts...)
			case "assistant":
				req.Messages = append(req.Messages, Message{Role: RoleAssistant, Parts: parts})
			default:
				req.Messages = append(req.Messages, Message{Role: RoleUser, Parts: parts})
			}
		}
	}
	return nil
}

// appendToAssistant attaches a part to the trailing assistant turn, starting one
// when the previous turn belongs to somebody else. The Responses API emits tool
// calls as siblings of the message they belong to.
func appendToAssistant(messages []Message, part Part) []Message {
	if n := len(messages); n > 0 && messages[n-1].Role == RoleAssistant {
		messages[n-1].Parts = append(messages[n-1].Parts, part)
		return messages
	}
	return append(messages, Message{Role: RoleAssistant, Parts: []Part{part}})
}

// parseResponsesContent reads an input/output content array.
func parseResponsesContent(raw json.RawMessage) ([]Part, error) {
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
		ImageURL string `json:"image_url"`
		FileData string `json:"file_data"`
		Filename string `json:"filename"`
	}
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, newAPIError(http.StatusBadRequest, "invalid_request_error",
			"could not parse content: "+err.Error())
	}

	var parts []Part
	for _, item := range items {
		switch item.Type {
		case "input_text", "output_text", "text", "summary_text":
			if item.Text != "" {
				parts = append(parts, Part{Type: PartText, Text: item.Text})
			}
		case "input_image":
			if item.ImageURL != "" {
				parts = append(parts, imagePartFromURL(item.ImageURL))
			}
		}
	}
	return parts, nil
}

func parseResponsesTools(raw json.RawMessage) []Tool {
	if len(raw) == 0 {
		return nil
	}
	var items []struct {
		Type        string          `json:"type"`
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	}
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil
	}
	var tools []Tool
	for _, item := range items {
		if item.Name == "" {
			continue
		}
		tools = append(tools, Tool{
			Name:        item.Name,
			Description: item.Description,
			Parameters:  item.Parameters,
		})
	}
	return tools
}

func parseResponsesToolChoice(raw json.RawMessage) ToolChoice {
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
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &object); err == nil && object.Name != "" {
		return ToolChoice{Mode: "tool", Name: object.Name}
	}
	return ToolChoice{}
}

// rawToText flattens a function_call_output payload, which may be a string, an
// object or an array of content parts.
func rawToText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text
	}
	if parts, err := parseResponsesContent(raw); err == nil && len(parts) > 0 {
		return partsText(parts)
	}
	return string(raw)
}

// ---------------------------------------------------------------------------
// Response rendering
// ---------------------------------------------------------------------------

// responsesItemKind discriminates the output items a response can contain.
type responsesItemKind string

const (
	itemReasoning responsesItemKind = "reasoning"
	itemMessage   responsesItemKind = "message"
	itemFunction  responsesItemKind = "function_call"
)

// responsesItem is one entry of the response's output array.
type responsesItem struct {
	Kind   responsesItemKind
	ID     string
	CallID string
	Name   string
	Index  int
	Body   strings.Builder
}

func (i *responsesItem) payload(complete bool) map[string]any {
	status := "in_progress"
	if complete {
		status = "completed"
	}
	switch i.Kind {
	case itemReasoning:
		summary := []any{}
		if i.Body.Len() > 0 {
			summary = append(summary, map[string]any{
				"type": "summary_text",
				"text": i.Body.String(),
			})
		}
		return map[string]any{
			"id":      i.ID,
			"type":    "reasoning",
			"summary": summary,
		}
	case itemFunction:
		return map[string]any{
			"id":        i.ID,
			"type":      "function_call",
			"call_id":   i.CallID,
			"name":      i.Name,
			"arguments": defaultArguments(i.Body.String()),
			"status":    status,
		}
	default:
		content := []any{}
		if complete || i.Body.Len() > 0 {
			content = append(content, map[string]any{
				"type":        "output_text",
				"text":        i.Body.String(),
				"annotations": []any{},
			})
		}
		return map[string]any{
			"id":      i.ID,
			"type":    "message",
			"role":    "assistant",
			"status":  status,
			"content": content,
		}
	}
}

// responsesRenderer renders canonical events as an OpenAI Responses response.
type responsesRenderer struct {
	w      http.ResponseWriter
	sse    *sseWriter
	stream bool

	id        string
	createdAt int64
	model     string

	sequence int
	items    []*responsesItem
	open     *responsesItem
	usage    Usage
	finish   string
}

func newResponsesRenderer(w http.ResponseWriter, stream bool) *responsesRenderer {
	return &responsesRenderer{
		w:         w,
		stream:    stream,
		id:        "resp_" + strings.ReplaceAll(uuid.NewString(), "-", ""),
		createdAt: time.Now().Unix(),
	}
}

func (r *responsesRenderer) Begin(model string) {
	r.model = model
	if !r.stream {
		return
	}
	r.sse = newSSEWriter(r.w)
	r.sse.WriteHeader(http.StatusOK)
	r.emit("response.created", map[string]any{"response": r.response("in_progress", false)})
	r.emit("response.in_progress", map[string]any{"response": r.response("in_progress", false)})
}

func (r *responsesRenderer) Handle(ev Event) error {
	if ev.Model != "" {
		r.model = ev.Model
	}
	if ev.ResponseID != "" && !r.stream {
		// Keep the upstream id for non-streaming replies; a streaming client has
		// already seen ours in response.created.
		r.id = ev.ResponseID
	}

	switch ev.Type {
	case EventThinking:
		if ev.Text == "" {
			return nil
		}
		item := r.ensureItem(itemReasoning, "", "")
		item.Body.WriteString(ev.Text)
		r.emit("response.reasoning_summary_text.delta", map[string]any{
			"item_id":       item.ID,
			"output_index":  item.Index,
			"summary_index": 0,
			"delta":         ev.Text,
		})
	case EventText:
		if ev.Text == "" {
			return nil
		}
		item := r.ensureItem(itemMessage, "", "")
		item.Body.WriteString(ev.Text)
		r.emit("response.output_text.delta", map[string]any{
			"item_id":       item.ID,
			"output_index":  item.Index,
			"content_index": 0,
			"delta":         ev.Text,
		})
	case EventToolCallStart:
		r.ensureItem(itemFunction, ev.ToolCallID, ev.ToolName)
	case EventToolCallDelta:
		item := r.open
		if item == nil || item.Kind != itemFunction {
			item = r.ensureItem(itemFunction, ev.ToolCallID, ev.ToolName)
		}
		item.Body.WriteString(ev.Arguments)
		r.emit("response.function_call_arguments.delta", map[string]any{
			"item_id":      item.ID,
			"output_index": item.Index,
			"delta":        ev.Arguments,
		})
	case EventToolCallDone:
		item := r.open
		if item == nil || item.Kind != itemFunction {
			item = r.ensureItem(itemFunction, ev.ToolCallID, ev.ToolName)
		}
		if item.Body.Len() == 0 && ev.Arguments != "" {
			item.Body.WriteString(ev.Arguments)
			r.emit("response.function_call_arguments.delta", map[string]any{
				"item_id":      item.ID,
				"output_index": item.Index,
				"delta":        ev.Arguments,
			})
		}
		r.closeOpen()
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

// ensureItem returns the open item of the requested kind, closing a different
// one first and announcing the new item on the stream.
func (r *responsesRenderer) ensureItem(kind responsesItemKind, callID, name string) *responsesItem {
	if r.open != nil && r.open.Kind == kind && kind != itemFunction {
		return r.open
	}
	r.closeOpen()

	item := &responsesItem{
		Kind:   kind,
		CallID: callID,
		Name:   name,
		Index:  len(r.items),
		ID:     responsesItemID(kind),
	}
	if kind == itemFunction && item.CallID == "" {
		item.CallID = "call_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:24]
	}
	r.items = append(r.items, item)
	r.open = item

	r.emit("response.output_item.added", map[string]any{
		"output_index": item.Index,
		"item":         item.payload(false),
	})
	switch kind {
	case itemMessage:
		r.emit("response.content_part.added", map[string]any{
			"item_id":       item.ID,
			"output_index":  item.Index,
			"content_index": 0,
			"part":          map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
		})
	case itemReasoning:
		r.emit("response.reasoning_summary_part.added", map[string]any{
			"item_id":       item.ID,
			"output_index":  item.Index,
			"summary_index": 0,
			"part":          map[string]any{"type": "summary_text", "text": ""},
		})
	}
	return item
}

// closeOpen emits the terminal frames for the item currently being streamed.
func (r *responsesRenderer) closeOpen() {
	item := r.open
	if item == nil {
		return
	}
	r.open = nil

	switch item.Kind {
	case itemMessage:
		r.emit("response.output_text.done", map[string]any{
			"item_id":       item.ID,
			"output_index":  item.Index,
			"content_index": 0,
			"text":          item.Body.String(),
		})
		r.emit("response.content_part.done", map[string]any{
			"item_id":       item.ID,
			"output_index":  item.Index,
			"content_index": 0,
			"part": map[string]any{
				"type": "output_text", "text": item.Body.String(), "annotations": []any{},
			},
		})
	case itemReasoning:
		r.emit("response.reasoning_summary_text.done", map[string]any{
			"item_id":       item.ID,
			"output_index":  item.Index,
			"summary_index": 0,
			"text":          item.Body.String(),
		})
		r.emit("response.reasoning_summary_part.done", map[string]any{
			"item_id":       item.ID,
			"output_index":  item.Index,
			"summary_index": 0,
			"part":          map[string]any{"type": "summary_text", "text": item.Body.String()},
		})
	case itemFunction:
		r.emit("response.function_call_arguments.done", map[string]any{
			"item_id":      item.ID,
			"output_index": item.Index,
			"arguments":    defaultArguments(item.Body.String()),
		})
	}
	r.emit("response.output_item.done", map[string]any{
		"output_index": item.Index,
		"item":         item.payload(true),
	})
}

func (r *responsesRenderer) Finish() error {
	r.closeOpen()
	if r.finish == "" {
		r.finish = FinishStop
	}

	status := "completed"
	if r.finish == FinishLength {
		status = "incomplete"
	}
	response := r.response(status, true)

	if r.stream {
		name := "response.completed"
		if status == "incomplete" {
			name = "response.incomplete"
		}
		r.emit(name, map[string]any{"response": response})
		return r.sse.Err()
	}
	return writeJSON(r.w, http.StatusOK, response)
}

// response assembles the response object at its current state.
func (r *responsesRenderer) response(status string, withUsage bool) map[string]any {
	output := make([]any, 0, len(r.items))
	for _, item := range r.items {
		output = append(output, item.payload(status != "in_progress"))
	}

	response := map[string]any{
		"id":                  r.id,
		"object":              "response",
		"created_at":          r.createdAt,
		"status":              status,
		"model":               r.model,
		"output":              output,
		"parallel_tool_calls": true,
		"store":               false,
	}
	if status == "incomplete" {
		response["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
	}
	if withUsage {
		response["usage"] = map[string]any{
			"input_tokens":  r.usage.PromptTokens,
			"output_tokens": r.usage.CompletionTokens,
			"total_tokens":  r.usage.Total(),
			"input_tokens_details": map[string]any{
				"cached_tokens": r.usage.CachedTokens,
			},
			"output_tokens_details": map[string]any{
				"reasoning_tokens": r.usage.ReasoningTokens,
			},
		}
	}
	return response
}

// emit writes one typed SSE event, numbering it as the API does.
func (r *responsesRenderer) emit(name string, payload map[string]any) {
	if !r.stream {
		return
	}
	payload["type"] = name
	payload["sequence_number"] = r.sequence
	r.sequence++

	encoded, err := json.Marshal(payload)
	if err != nil {
		return
	}
	r.sse.Event(name, string(encoded))
}

func (r *responsesRenderer) Usage() Usage { return r.usage }

func (r *responsesRenderer) WriteError(apiErr *APIError) {
	if r.stream && r.sse != nil {
		payload := map[string]any{
			"type":            "error",
			"sequence_number": r.sequence,
			"code":            apiErr.Code,
			"message":         apiErr.Message,
			"param":           nil,
		}
		r.sequence++
		if encoded, err := json.Marshal(payload); err == nil {
			r.sse.Event("error", string(encoded))
		}
		return
	}
	writeFormatError(r.w, nil, false, FormatOpenAIResponses, apiErr)
}

func responsesItemID(kind responsesItemKind) string {
	prefix := "msg_"
	switch kind {
	case itemReasoning:
		prefix = "rs_"
	case itemFunction:
		prefix = "fc_"
	}
	return prefix + strings.ReplaceAll(uuid.NewString(), "-", "")
}
