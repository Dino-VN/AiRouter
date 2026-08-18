package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"aihub/internal/model"
	"aihub/internal/provider"
)

// codexResponsesURL is the ChatGPT backend the Codex CLI itself talks to.
const codexResponsesURL = "https://chatgpt.com/backend-api/codex/responses"

// codexUserAgent identifies this client as the Codex CLI. The endpoint refuses
// requests from unknown originators.
const (
	codexOriginator = "codex_cli_rs"
	codexUserAgent  = "codex_cli_rs/0.51.0 (Linux 6.1.0; x86_64) aihub"
)

// codexDefaultInstructions is used when the caller sends no system prompt. The
// field is required by the endpoint.
const codexDefaultInstructions = "You are a helpful assistant."

// codexDroppedFields are Responses API fields the Codex backend rejects or
// silently mishandles. They are removed before a passthrough request is
// forwarded.
var codexDroppedFields = []string{
	"previous_response_id", "prompt_cache_retention", "safety_identifier",
	"stream_options", "metadata", "service_tier", "temperature", "top_p",
	"top_logprobs", "max_output_tokens", "max_tool_calls", "truncation",
	"user", "background", "conversation", "prompt", "logit_bias", "n",
}

// codexExecutor proxies to the ChatGPT Codex backend, which speaks the OpenAI
// Responses protocol and only ever streams.
type codexExecutor struct {
	client *http.Client
	tokens *provider.TokenManager
	log    *slog.Logger
}

func newCodexExecutor(client *http.Client, tokens *provider.TokenManager, logger *slog.Logger) *codexExecutor {
	return &codexExecutor{client: client, tokens: tokens, log: logger}
}

func (e *codexExecutor) providerID() model.Provider { return model.ProviderCodex }

// passthrough forwards Responses traffic verbatim: the protocols are identical,
// so translating would only lose fidelity (encrypted reasoning, item ids).
func (e *codexExecutor) passthrough(format Format) bool {
	return format == FormatOpenAIResponses
}

func (e *codexExecutor) send(ctx context.Context, conn *model.Connection, req *Request, opts sendOptions) (*upstreamStream, error) {
	cred, err := e.tokens.Ensure(ctx, conn)
	if err != nil {
		return nil, asAPIError(model.ProviderCodex, err)
	}

	var body []byte
	if opts.Raw {
		body, err = sanitizeCodexPassthrough(req)
	} else {
		body, err = buildCodexRequest(req)
	}
	if err != nil {
		return nil, asAPIError(model.ProviderCodex, err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, codexResponsesURL, bytes.NewReader(body))
	if err != nil {
		return nil, asAPIError(model.ProviderCodex, err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+cred.AccessToken)
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("OpenAI-Beta", "responses=experimental")
	httpReq.Header.Set("Originator", codexOriginator)
	httpReq.Header.Set("User-Agent", codexUserAgent)
	httpReq.Header.Set("Session-Id", uuid.NewString())
	if conn.AccountID != "" {
		httpReq.Header.Set("Chatgpt-Account-Id", conn.AccountID)
	}

	resp, err := e.client.Do(httpReq)
	if err != nil {
		return nil, asAPIError(model.ProviderCodex, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		defer resp.Body.Close()
		return nil, apiErrorFromResponse(model.ProviderCodex, resp.StatusCode, resp.Header, readErrorBody(resp.Body))
	}

	stream := &upstreamStream{
		Header:  resp.Header,
		Body:    resp.Body,
		scanner: newSSEScanner(resp.Body),
	}
	if opts.Raw {
		stream.sniff = codexSniffUsage
		stream.rewrite = func(frame sseEvent) (sseEvent, bool) {
			// The final object is kept for non-streaming callers, but the frame is
			// still forwarded so a streaming client sees the real protocol.
			if frame.Name == "response.completed" || frame.Name == "response.incomplete" {
				if final := codexFinalResponse(frame.Data); len(final) > 0 {
					stream.aggregate = final
				}
			}
			return frame, true
		}
		return stream, nil
	}

	decoder := newCodexDecoder()
	stream.decode = decoder.decode
	stream.trailer = decoder.trailer
	return stream, nil
}

// ---------------------------------------------------------------------------
// Request building
// ---------------------------------------------------------------------------

// buildCodexRequest renders a canonical request as a Codex Responses body.
func buildCodexRequest(req *Request) ([]byte, error) {
	input := make([]any, 0, len(req.Messages)*2)
	for _, msg := range req.Messages {
		input = append(input, codexInputItems(msg)...)
	}

	body := map[string]any{
		"model":               req.Model,
		"instructions":        firstNonEmpty(req.SystemText(), codexDefaultInstructions),
		"input":               input,
		"stream":              true,
		"store":               false,
		"parallel_tool_calls": req.ParallelToolUse == nil || *req.ParallelToolUse,
		"include":             []string{"reasoning.encrypted_content"},
		"reasoning": map[string]any{
			"effort":  codexEffort(req.Reasoning.Effort),
			"summary": "auto",
		},
	}

	if len(req.Tools) > 0 {
		tools := make([]any, 0, len(req.Tools))
		for _, tool := range req.Tools {
			entry := map[string]any{
				"type":        "function",
				"name":        tool.Name,
				"description": tool.Description,
				"strict":      false,
			}
			if len(tool.Parameters) > 0 {
				entry["parameters"] = tool.Parameters
			} else {
				entry["parameters"] = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			tools = append(tools, entry)
		}
		body["tools"] = tools

		switch req.ToolChoice.Mode {
		case "auto", "none", "required":
			body["tool_choice"] = req.ToolChoice.Mode
		case "tool":
			body["tool_choice"] = map[string]any{"type": "function", "name": req.ToolChoice.Name}
		}
	}

	// The Codex backend does not accept sampling knobs, so temperature, top_p and
	// max_output_tokens are deliberately not forwarded.
	if req.ResponseMIMEType == "application/json" && len(req.ResponseSchema) > 0 {
		body["text"] = map[string]any{"format": map[string]any{
			"type":   "json_schema",
			"name":   "response",
			"schema": req.ResponseSchema,
			"strict": false,
		}}
	}

	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encode codex request: %w", err)
	}
	return encoded, nil
}

// codexInputItems renders one canonical message as Responses input items.
func codexInputItems(msg Message) []any {
	var (
		items   []any
		content []any
	)

	textType := "input_text"
	if msg.Role == RoleAssistant {
		textType = "output_text"
	}

	for _, part := range msg.Parts {
		switch part.Type {
		case PartText:
			if part.Text == "" {
				continue
			}
			content = append(content, map[string]any{"type": textType, "text": part.Text})
		case PartImage:
			url := part.URL
			if url == "" && part.Data != "" {
				url = "data:" + firstNonEmpty(part.MimeType, "image/png") + ";base64," + part.Data
			}
			if url == "" {
				continue
			}
			content = append(content, map[string]any{"type": "input_image", "image_url": url})
		case PartThinking:
			// Reasoning can only be replayed when the encrypted payload came from
			// this same backend; a summary on its own is rejected.
			if part.Signature == "" {
				continue
			}
			item := map[string]any{
				"type":              "reasoning",
				"encrypted_content": part.Signature,
				"summary":           []any{},
			}
			if part.Text != "" {
				item["summary"] = []any{map[string]any{"type": "summary_text", "text": part.Text}}
			}
			items = append(items, item)
		case PartToolCall:
			items = append(items, map[string]any{
				"type":      "function_call",
				"call_id":   part.ToolCallID,
				"name":      part.ToolName,
				"arguments": defaultArguments(part.Arguments),
			})
		case PartToolResult:
			items = append(items, map[string]any{
				"type":    "function_call_output",
				"call_id": part.ToolCallID,
				"output":  part.Text,
			})
		}
	}

	if len(content) > 0 {
		role := "user"
		if msg.Role == RoleAssistant {
			role = "assistant"
		}
		// Content comes before the tool calls it accompanies.
		items = append([]any{map[string]any{
			"type":    "message",
			"role":    role,
			"content": content,
		}}, items...)
	}
	return items
}

// codexEffort clamps a reasoning effort to the values the backend accepts.
func codexEffort(effort string) string {
	switch strings.ToLower(effort) {
	case "minimal", "none":
		return "minimal"
	case "low":
		return "low"
	case "high", "max", "xhigh":
		return "high"
	default:
		return "medium"
	}
}

// sanitizeCodexPassthrough adapts a client's own Responses body to what the
// Codex backend accepts, without otherwise reshaping it.
func sanitizeCodexPassthrough(req *Request) ([]byte, error) {
	var body map[string]any
	if err := json.Unmarshal(req.Raw, &body); err != nil {
		return nil, fmt.Errorf("parse request body: %w", err)
	}

	for _, key := range codexDroppedFields {
		delete(body, key)
	}
	body["model"] = req.Model
	body["stream"] = true
	body["store"] = false
	if instructions, _ := body["instructions"].(string); strings.TrimSpace(instructions) == "" {
		body["instructions"] = codexDefaultInstructions
	}
	if _, ok := body["parallel_tool_calls"]; !ok {
		body["parallel_tool_calls"] = true
	}

	// Encrypted reasoning must be requested explicitly, otherwise multi-turn tool
	// use loses the model's chain of thought.
	include := []any{"reasoning.encrypted_content"}
	if existing, ok := body["include"].([]any); ok {
		seen := false
		for _, item := range existing {
			if text, _ := item.(string); text == "reasoning.encrypted_content" {
				seen = true
			}
		}
		if seen {
			include = existing
		} else {
			include = append(existing, "reasoning.encrypted_content")
		}
	}
	body["include"] = include

	if reasoning, ok := body["reasoning"].(map[string]any); ok {
		if effort, _ := reasoning["effort"].(string); effort != "" {
			reasoning["effort"] = codexEffort(effort)
		}
	} else {
		body["reasoning"] = map[string]any{
			"effort":  codexEffort(req.Reasoning.Effort),
			"summary": "auto",
		}
	}

	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encode codex request: %w", err)
	}
	return encoded, nil
}

// ---------------------------------------------------------------------------
// Response decoding
// ---------------------------------------------------------------------------

// codexUsageBody is the usage block on a finished response.
type codexUsageBody struct {
	InputTokens        int64 `json:"input_tokens"`
	OutputTokens       int64 `json:"output_tokens"`
	TotalTokens        int64 `json:"total_tokens"`
	InputTokensDetails *struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	OutputTokensDetails *struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

// codexEventPayload is the union of the Responses stream frames this proxy reads.
type codexEventPayload struct {
	Type     string `json:"type"`
	Delta    string `json:"delta"`
	Text     string `json:"text"`
	Input    string `json:"input"`
	ItemID   string `json:"item_id"`
	Sequence int    `json:"sequence_number"`

	Arguments string `json:"arguments"`

	Item *struct {
		ID               string `json:"id"`
		Type             string `json:"type"`
		CallID           string `json:"call_id"`
		Name             string `json:"name"`
		Arguments        string `json:"arguments"`
		EncryptedContent string `json:"encrypted_content"`
		Summary          []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"summary"`
	} `json:"item"`

	Response *struct {
		ID                string          `json:"id"`
		Model             string          `json:"model"`
		Status            string          `json:"status"`
		Usage             *codexUsageBody `json:"usage"`
		IncompleteDetails *struct {
			Reason string `json:"reason"`
		} `json:"incomplete_details"`
		Error json.RawMessage `json:"error"`
	} `json:"response"`

	Error json.RawMessage `json:"error"`
}

// codexToolCall tracks a streamed function call so deltas can be attributed.
type codexToolCall struct {
	CallID string
	Name   string
	Index  int
	Done   bool
}

// codexDecoder converts the Responses stream into canonical events.
type codexDecoder struct {
	calls  map[string]*codexToolCall
	count  int
	usage  *Usage
	finish string
	seen   bool
}

func newCodexDecoder() *codexDecoder {
	return &codexDecoder{calls: map[string]*codexToolCall{}}
}

func (d *codexDecoder) decode(frame sseEvent) ([]Event, error) {
	var payload codexEventPayload
	if err := json.Unmarshal([]byte(frame.Data), &payload); err != nil {
		// A frame this proxy cannot read is not worth failing the whole request.
		return nil, nil
	}
	name := firstNonEmpty(payload.Type, frame.Name)

	switch name {
	case "response.created":
		d.seen = true
		ev := Event{Type: EventStart}
		if payload.Response != nil {
			ev.ResponseID = payload.Response.ID
			ev.Model = payload.Response.Model
		}
		return []Event{ev}, nil

	case "response.output_text.delta":
		if payload.Delta == "" {
			return nil, nil
		}
		return []Event{{Type: EventText, Text: payload.Delta}}, nil

	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		if payload.Delta == "" {
			return nil, nil
		}
		return []Event{{Type: EventThinking, Text: payload.Delta}}, nil

	case "response.output_item.added":
		if payload.Item == nil {
			return nil, nil
		}
		switch payload.Item.Type {
		case "function_call", "custom_tool_call":
			call := &codexToolCall{
				CallID: firstNonEmpty(payload.Item.CallID, payload.Item.ID),
				Name:   payload.Item.Name,
				Index:  d.count,
			}
			d.count++
			d.calls[payload.Item.ID] = call
			return []Event{{
				Type:       EventToolCallStart,
				Index:      call.Index,
				ToolCallID: call.CallID,
				ToolName:   call.Name,
			}}, nil
		}
		return nil, nil

	case "response.function_call_arguments.delta", "response.custom_tool_call_input.delta":
		call := d.call(payload.ItemID)
		if call == nil {
			return nil, nil
		}
		delta := firstNonEmpty(payload.Delta, payload.Input)
		if delta == "" {
			return nil, nil
		}
		return []Event{{
			Type:       EventToolCallDelta,
			Index:      call.Index,
			ToolCallID: call.CallID,
			ToolName:   call.Name,
			Arguments:  delta,
		}}, nil

	case "response.function_call_arguments.done", "response.custom_tool_call_input.done":
		call := d.call(payload.ItemID)
		if call == nil || call.Done {
			return nil, nil
		}
		call.Done = true
		d.finish = FinishToolCalls
		return []Event{{
			Type:       EventToolCallDone,
			Index:      call.Index,
			ToolCallID: call.CallID,
			ToolName:   call.Name,
			Arguments:  firstNonEmpty(payload.Arguments, payload.Input),
		}}, nil

	case "response.output_item.done":
		if payload.Item == nil {
			return nil, nil
		}
		switch payload.Item.Type {
		case "reasoning":
			// The encrypted payload only arrives here; forwarding it lets a client
			// replay the reasoning on its next turn.
			if payload.Item.EncryptedContent == "" {
				return nil, nil
			}
			return []Event{{Type: EventThinking, Signature: payload.Item.EncryptedContent}}, nil
		case "function_call", "custom_tool_call":
			call := d.calls[payload.Item.ID]
			if call == nil || call.Done {
				return nil, nil
			}
			call.Done = true
			d.finish = FinishToolCalls
			return []Event{{
				Type:       EventToolCallDone,
				Index:      call.Index,
				ToolCallID: call.CallID,
				ToolName:   call.Name,
				Arguments:  payload.Item.Arguments,
			}}, nil
		}
		return nil, nil

	case "response.completed", "response.incomplete":
		if payload.Response != nil {
			d.usage = codexUsage(payload.Response.Usage)
			if payload.Response.IncompleteDetails != nil &&
				payload.Response.IncompleteDetails.Reason == "max_output_tokens" {
				d.finish = FinishLength
			}
		}
		if name == "response.incomplete" && d.finish == "" {
			d.finish = FinishLength
		}
		return nil, nil

	case "response.failed", "error":
		return nil, codexStreamError(payload)
	}
	return nil, nil
}

// call resolves the tool call an argument delta belongs to, tolerating providers
// that omit item ids.
func (d *codexDecoder) call(itemID string) *codexToolCall {
	if call, ok := d.calls[itemID]; ok {
		return call
	}
	var latest *codexToolCall
	for _, call := range d.calls {
		if latest == nil || call.Index > latest.Index {
			latest = call
		}
	}
	return latest
}

func (d *codexDecoder) trailer() []Event {
	finish := d.finish
	if finish == "" {
		finish = FinishStop
	}
	return []Event{{Type: EventDone, FinishReason: finish, Usage: d.usage}}
}

// codexStreamError turns an error frame into an *APIError.
func codexStreamError(payload codexEventPayload) error {
	raw := payload.Error
	if len(raw) == 0 && payload.Response != nil {
		raw = payload.Response.Error
	}
	message, code, errType := parseUpstreamError(wrapErrorObject(raw))
	if message == "" {
		message = "the upstream reported a failed response"
	}
	return &APIError{
		Status:   http.StatusBadGateway,
		Type:     firstNonEmpty(errType, "api_error"),
		Code:     code,
		Message:  "codex: " + truncate(message, 2000),
		Upstream: http.StatusBadGateway,
	}
}

// wrapErrorObject re-wraps a bare error object so parseUpstreamError can read it.
func wrapErrorObject(raw json.RawMessage) []byte {
	if len(raw) == 0 {
		return nil
	}
	return append(append([]byte(`{"error":`), raw...), '}')
}

func codexUsage(usage *codexUsageBody) *Usage {
	if usage == nil {
		return nil
	}
	out := &Usage{
		PromptTokens:     usage.InputTokens,
		CompletionTokens: usage.OutputTokens,
		TotalTokens:      usage.TotalTokens,
	}
	if usage.InputTokensDetails != nil {
		out.CachedTokens = usage.InputTokensDetails.CachedTokens
	}
	if usage.OutputTokensDetails != nil {
		out.ReasoningTokens = usage.OutputTokensDetails.ReasoningTokens
	}
	return out
}

// codexSniffUsage reads usage out of a raw frame so passthrough still bills.
func codexSniffUsage(frame sseEvent) *Usage {
	if frame.Name != "response.completed" && frame.Name != "response.incomplete" &&
		!strings.Contains(frame.Data, `"response.completed"`) {
		return nil
	}
	var payload codexEventPayload
	if err := json.Unmarshal([]byte(frame.Data), &payload); err != nil || payload.Response == nil {
		return nil
	}
	return codexUsage(payload.Response.Usage)
}

// codexFinalResponse extracts the complete response object a non-streaming
// client should receive.
func codexFinalResponse(data string) json.RawMessage {
	var payload struct {
		Response json.RawMessage `json:"response"`
	}
	if err := json.Unmarshal([]byte(data), &payload); err != nil {
		return nil
	}
	return payload.Response
}
