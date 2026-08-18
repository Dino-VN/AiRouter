package proxy

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// Request parsing
// ---------------------------------------------------------------------------

type anthropicRequestBody struct {
	Model         string          `json:"model"`
	MaxTokens     *int            `json:"max_tokens"`
	System        json.RawMessage `json:"system"`
	Messages      []anthropicMsg  `json:"messages"`
	Tools         json.RawMessage `json:"tools"`
	ToolChoice    json.RawMessage `json:"tool_choice"`
	Temperature   *float64        `json:"temperature"`
	TopP          *float64        `json:"top_p"`
	TopK          *int            `json:"top_k"`
	StopSequences []string        `json:"stop_sequences"`
	Stream        bool            `json:"stream"`
	Thinking      *struct {
		Type         string `json:"type"`
		BudgetTokens int    `json:"budget_tokens"`
	} `json:"thinking"`
}

type anthropicMsg struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// anthropicBlock is the union of every content block Anthropic accepts.
type anthropicBlock struct {
	Type   string `json:"type"`
	Text   string `json:"text"`
	Source *struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
		URL       string `json:"url"`
	} `json:"source"`

	// tool_use
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`

	// tool_result
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`

	// thinking
	Thinking  string `json:"thinking"`
	Signature string `json:"signature"`
	Data      string `json:"data"`
}

// parseAnthropic converts a /v1/messages body to canonical form.
func parseAnthropic(raw []byte) (*Request, error) {
	var body anthropicRequestBody
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, newAPIError(http.StatusBadRequest, "invalid_request_error",
			"could not parse request body: "+err.Error())
	}
	if strings.TrimSpace(body.Model) == "" {
		return nil, newAPIError(http.StatusBadRequest, "invalid_request_error", "model is required")
	}

	req := &Request{
		ClientFormat:    FormatAnthropic,
		Model:           normalizeModel(body.Model),
		Stream:          body.Stream,
		Temperature:     body.Temperature,
		TopP:            body.TopP,
		TopK:            body.TopK,
		MaxOutputTokens: body.MaxTokens,
		Stop:            body.StopSequences,
		Raw:             raw,
	}
	if body.Thinking != nil && body.Thinking.Type == "enabled" {
		req.Reasoning = Reasoning{
			Enabled:         true,
			IncludeThoughts: true,
			BudgetTokens:    body.Thinking.BudgetTokens,
			Effort:          effortForBudget(body.Thinking.BudgetTokens),
		}
	}

	systemParts, err := parseAnthropicContent(body.System)
	if err != nil {
		return nil, err
	}
	req.System = systemParts
	req.Tools = parseAnthropicTools(body.Tools)
	req.ToolChoice = parseAnthropicToolChoice(body.ToolChoice)

	for _, msg := range body.Messages {
		parts, err := parseAnthropicContent(msg.Content)
		if err != nil {
			return nil, err
		}
		if len(parts) == 0 {
			continue
		}
		// Tool results arrive in a user turn; the canonical form keeps them in
		// their own tool turn so every builder can place them correctly.
		var results, rest []Part
		for _, part := range parts {
			if part.Type == PartToolResult {
				results = append(results, part)
				continue
			}
			rest = append(rest, part)
		}

		role := RoleUser
		if strings.EqualFold(msg.Role, "assistant") {
			role = RoleAssistant
		}
		for _, result := range results {
			req.Messages = append(req.Messages, Message{Role: RoleTool, Parts: []Part{result}})
		}
		if len(rest) > 0 {
			req.Messages = append(req.Messages, Message{Role: role, Parts: rest})
		}
	}

	if len(req.Messages) == 0 {
		return nil, newAPIError(http.StatusBadRequest, "invalid_request_error", "messages must not be empty")
	}
	return req, nil
}

func parseAnthropicContent(raw json.RawMessage) ([]Part, error) {
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

	var blocks []anthropicBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, newAPIError(http.StatusBadRequest, "invalid_request_error",
			"could not parse content blocks: "+err.Error())
	}

	var parts []Part
	for _, block := range blocks {
		switch block.Type {
		case "text":
			if block.Text != "" {
				parts = append(parts, Part{Type: PartText, Text: block.Text})
			}
		case "thinking":
			if block.Thinking != "" || block.Signature != "" {
				parts = append(parts, Part{
					Type:      PartThinking,
					Text:      block.Thinking,
					Signature: block.Signature,
				})
			}
		case "redacted_thinking":
			if block.Data != "" {
				parts = append(parts, Part{Type: PartThinking, Signature: block.Data})
			}
		case "image", "document":
			if block.Source == nil {
				continue
			}
			switch block.Source.Type {
			case "url":
				parts = append(parts, Part{Type: PartImage, URL: block.Source.URL})
			default:
				parts = append(parts, Part{
					Type:     PartImage,
					MimeType: block.Source.MediaType,
					Data:     block.Source.Data,
				})
			}
		case "tool_use":
			arguments := "{}"
			if len(block.Input) > 0 {
				arguments = string(block.Input)
			}
			parts = append(parts, Part{
				Type:       PartToolCall,
				ToolCallID: block.ID,
				ToolName:   block.Name,
				Arguments:  arguments,
			})
		case "tool_result":
			parts = append(parts, Part{
				Type:       PartToolResult,
				ToolCallID: block.ToolUseID,
				Text:       anthropicResultText(block.Content),
				IsError:    block.IsError,
			})
		}
	}
	return parts, nil
}

// anthropicResultText flattens a tool_result content field.
func anthropicResultText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text
	}
	if parts, err := parseAnthropicContent(raw); err == nil && len(parts) > 0 {
		return partsText(parts)
	}
	return string(raw)
}

func parseAnthropicTools(raw json.RawMessage) []Tool {
	if len(raw) == 0 {
		return nil
	}
	var items []struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		InputSchema json.RawMessage `json:"input_schema"`
		Type        string          `json:"type"`
	}
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil
	}
	var tools []Tool
	for _, item := range items {
		// Server-side tools (computer_20241022 and friends) have no schema this
		// proxy could forward, so they are dropped.
		if item.Name == "" || len(item.InputSchema) == 0 {
			continue
		}
		tools = append(tools, Tool{
			Name:        item.Name,
			Description: item.Description,
			Parameters:  item.InputSchema,
		})
	}
	return tools
}

func parseAnthropicToolChoice(raw json.RawMessage) ToolChoice {
	if len(raw) == 0 {
		return ToolChoice{}
	}
	var object struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &object); err != nil {
		return ToolChoice{}
	}
	switch object.Type {
	case "auto":
		return ToolChoice{Mode: "auto"}
	case "any":
		return ToolChoice{Mode: "required"}
	case "none":
		return ToolChoice{Mode: "none"}
	case "tool":
		return ToolChoice{Mode: "tool", Name: object.Name}
	}
	return ToolChoice{}
}

// effortForBudget maps an Anthropic thinking budget onto a coarse effort level,
// which is all the Codex and Antigravity APIs accept.
func effortForBudget(budget int) string {
	switch {
	case budget <= 0:
		return ""
	case budget <= 2048:
		return "low"
	case budget <= 16384:
		return "medium"
	default:
		return "high"
	}
}

// ---------------------------------------------------------------------------
// Response rendering
// ---------------------------------------------------------------------------

// anthropicBlockKind discriminates the streamed content blocks.
type anthropicBlockKind string

const (
	blockText     anthropicBlockKind = "text"
	blockThinking anthropicBlockKind = "thinking"
	blockToolUse  anthropicBlockKind = "tool_use"
)

type anthropicOutBlock struct {
	Kind      anthropicBlockKind
	Index     int
	ID        string
	Name      string
	Signature string
	Body      strings.Builder
}

// anthropicRenderer renders canonical events as Anthropic Messages output.
type anthropicRenderer struct {
	w      http.ResponseWriter
	sse    *sseWriter
	stream bool

	id    string
	model string

	blocks    []*anthropicOutBlock
	open      *anthropicOutBlock
	usage     Usage
	finish    string
	sentStart bool
}

func newAnthropicRenderer(w http.ResponseWriter, stream bool) *anthropicRenderer {
	return &anthropicRenderer{
		w:      w,
		stream: stream,
		id:     "msg_" + strings.ReplaceAll(uuid.NewString(), "-", ""),
	}
}

func (r *anthropicRenderer) Begin(model string) {
	r.model = model
	if !r.stream {
		return
	}
	r.sse = newSSEWriter(r.w)
	r.sse.WriteHeader(http.StatusOK)
	r.emit("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            r.id,
			"type":          "message",
			"role":          "assistant",
			"model":         r.model,
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage": map[string]any{
				"input_tokens":  r.usage.PromptTokens,
				"output_tokens": 0,
			},
		},
	})
	r.sentStart = true
}

func (r *anthropicRenderer) Handle(ev Event) error {
	if ev.Model != "" {
		r.model = ev.Model
	}

	switch ev.Type {
	case EventThinking:
		block := r.ensureBlock(blockThinking, "", "")
		if ev.Signature != "" {
			block.Signature = ev.Signature
			r.emit("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": block.Index,
				"delta": map[string]any{"type": "signature_delta", "signature": ev.Signature},
			})
		}
		if ev.Text == "" {
			return nil
		}
		block.Body.WriteString(ev.Text)
		r.emit("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": block.Index,
			"delta": map[string]any{"type": "thinking_delta", "thinking": ev.Text},
		})
	case EventText:
		if ev.Text == "" {
			return nil
		}
		block := r.ensureBlock(blockText, "", "")
		block.Body.WriteString(ev.Text)
		r.emit("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": block.Index,
			"delta": map[string]any{"type": "text_delta", "text": ev.Text},
		})
	case EventToolCallStart:
		r.ensureBlock(blockToolUse, ev.ToolCallID, ev.ToolName)
	case EventToolCallDelta:
		block := r.open
		if block == nil || block.Kind != blockToolUse {
			block = r.ensureBlock(blockToolUse, ev.ToolCallID, ev.ToolName)
		}
		if ev.Arguments == "" {
			return nil
		}
		block.Body.WriteString(ev.Arguments)
		r.emit("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": block.Index,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": ev.Arguments},
		})
	case EventToolCallDone:
		block := r.open
		if block == nil || block.Kind != blockToolUse {
			block = r.ensureBlock(blockToolUse, ev.ToolCallID, ev.ToolName)
		}
		if block.Body.Len() == 0 && ev.Arguments != "" {
			block.Body.WriteString(ev.Arguments)
			r.emit("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": block.Index,
				"delta": map[string]any{"type": "input_json_delta", "partial_json": ev.Arguments},
			})
		}
		r.closeOpen()
		r.finish = FinishToolCalls
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

func (r *anthropicRenderer) ensureBlock(kind anthropicBlockKind, id, name string) *anthropicOutBlock {
	if r.open != nil && r.open.Kind == kind && kind != blockToolUse {
		return r.open
	}
	r.closeOpen()

	block := &anthropicOutBlock{Kind: kind, Index: len(r.blocks), ID: id, Name: name}
	if kind == blockToolUse && block.ID == "" {
		block.ID = "toolu_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:24]
	}
	r.blocks = append(r.blocks, block)
	r.open = block

	r.emit("content_block_start", map[string]any{
		"type":          "content_block_start",
		"index":         block.Index,
		"content_block": block.payload(),
	})
	return block
}

func (r *anthropicRenderer) closeOpen() {
	block := r.open
	if block == nil {
		return
	}
	r.open = nil
	r.emit("content_block_stop", map[string]any{
		"type":  "content_block_stop",
		"index": block.Index,
	})
}

// payload renders the block in its wire form. Streaming blocks start empty.
func (b *anthropicOutBlock) payload() map[string]any {
	switch b.Kind {
	case blockThinking:
		return map[string]any{"type": "thinking", "thinking": "", "signature": ""}
	case blockToolUse:
		return map[string]any{
			"type":  "tool_use",
			"id":    b.ID,
			"name":  b.Name,
			"input": map[string]any{},
		}
	default:
		return map[string]any{"type": "text", "text": ""}
	}
}

// final renders the block with its accumulated content.
func (b *anthropicOutBlock) final() map[string]any {
	switch b.Kind {
	case blockThinking:
		return map[string]any{
			"type":      "thinking",
			"thinking":  b.Body.String(),
			"signature": b.Signature,
		}
	case blockToolUse:
		var input any = map[string]any{}
		if raw := strings.TrimSpace(b.Body.String()); raw != "" {
			var decoded any
			if err := json.Unmarshal([]byte(raw), &decoded); err == nil {
				input = decoded
			}
		}
		return map[string]any{
			"type":  "tool_use",
			"id":    b.ID,
			"name":  b.Name,
			"input": input,
		}
	default:
		return map[string]any{"type": "text", "text": b.Body.String()}
	}
}

func (r *anthropicRenderer) Finish() error {
	r.closeOpen()
	stopReason := anthropicStopReason(r.finish)

	if r.stream {
		r.emit("message_delta", map[string]any{
			"type": "message_delta",
			"delta": map[string]any{
				"stop_reason":   stopReason,
				"stop_sequence": nil,
			},
			"usage": r.usagePayload(),
		})
		r.emit("message_stop", map[string]any{"type": "message_stop"})
		return r.sse.Err()
	}

	content := make([]any, 0, len(r.blocks))
	for _, block := range r.blocks {
		content = append(content, block.final())
	}
	return writeJSON(r.w, http.StatusOK, map[string]any{
		"id":            r.id,
		"type":          "message",
		"role":          "assistant",
		"model":         r.model,
		"content":       content,
		"stop_reason":   stopReason,
		"stop_sequence": nil,
		"usage":         r.usagePayload(),
	})
}

func (r *anthropicRenderer) usagePayload() map[string]any {
	usage := map[string]any{
		"input_tokens":  r.usage.PromptTokens,
		"output_tokens": r.usage.CompletionTokens,
	}
	if r.usage.CachedTokens > 0 {
		usage["cache_read_input_tokens"] = r.usage.CachedTokens
	}
	return usage
}

func (r *anthropicRenderer) Usage() Usage { return r.usage }

func (r *anthropicRenderer) WriteError(apiErr *APIError) {
	writeFormatError(r.w, r.sse, r.stream && r.sentStart, FormatAnthropic, apiErr)
}

func (r *anthropicRenderer) emit(name string, payload map[string]any) {
	if !r.stream {
		return
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return
	}
	r.sse.Event(name, string(encoded))
}

// anthropicStopReason maps a canonical finish reason to Anthropic's vocabulary.
func anthropicStopReason(finish string) string {
	switch finish {
	case FinishToolCalls:
		return "tool_use"
	case FinishLength:
		return "max_tokens"
	case FinishContentFilter:
		return "refusal"
	default:
		return "end_turn"
	}
}
