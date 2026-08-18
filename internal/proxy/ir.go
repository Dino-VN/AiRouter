// Package proxy translates client requests between the OpenAI, Anthropic and
// Gemini wire formats and the two upstream protocols (Codex Responses and
// Antigravity CodeAssist), selects a connection to serve them, and streams the
// answer back.
package proxy

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Format identifies a client-facing wire format.
type Format string

const (
	// FormatOpenAIChat is POST /v1/chat/completions.
	FormatOpenAIChat Format = "openai-chat"
	// FormatOpenAIResponses is POST /v1/responses.
	FormatOpenAIResponses Format = "openai-responses"
	// FormatAnthropic is POST /v1/messages.
	FormatAnthropic Format = "anthropic"
	// FormatGemini is POST /v1beta/models/{model}:generateContent.
	FormatGemini Format = "gemini"
)

// Role is a canonical message role.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// PartType discriminates the canonical content parts.
type PartType string

const (
	// PartText is plain text.
	PartText PartType = "text"
	// PartImage is inline image data.
	PartImage PartType = "image"
	// PartThinking is reasoning text emitted by the model.
	PartThinking PartType = "thinking"
	// PartToolCall is a function call requested by the model.
	PartToolCall PartType = "tool_call"
	// PartToolResult is the output of a tool, sent back by the client.
	PartToolResult PartType = "tool_result"
)

// Part is one piece of message content.
type Part struct {
	Type PartType

	// Text carries text and thinking content.
	Text string

	// MimeType and Data hold inline image bytes (Data is base64, without a data:
	// URL prefix). URL holds a remote image reference instead.
	MimeType string
	Data     string
	URL      string

	// ToolCallID correlates a tool call with its result.
	ToolCallID string
	// ToolName is the function name for a call or result.
	ToolName string
	// Arguments is the raw JSON argument object of a tool call.
	Arguments string
	// IsError marks a tool result that reports failure.
	IsError bool
	// Signature carries a provider-specific opaque token (Anthropic thinking
	// signatures, Gemini thought signatures) so multi-turn reasoning survives a
	// round trip.
	Signature string
}

// Message is one canonical conversation turn.
type Message struct {
	Role  Role
	Parts []Part
}

// Text concatenates every text part of the message.
func (m Message) Text() string {
	var b strings.Builder
	for _, part := range m.Parts {
		if part.Type == PartText {
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(part.Text)
		}
	}
	return b.String()
}

// Tool is a function the model may call.
type Tool struct {
	Name        string
	Description string
	// Parameters is a JSON Schema object.
	Parameters json.RawMessage
}

// ToolChoice constrains tool usage.
type ToolChoice struct {
	// Mode is "auto", "none", "required" or "tool".
	Mode string
	// Name is set when Mode is "tool".
	Name string
}

// Reasoning carries the thinking configuration in a provider-neutral way.
type Reasoning struct {
	// Effort is "minimal", "low", "medium", "high" or "" (unset).
	Effort string
	// BudgetTokens is Anthropic/Gemini style explicit budget; 0 means unset.
	BudgetTokens int
	// Enabled reports whether the client asked for thinking at all.
	Enabled bool
	// IncludeThoughts asks the provider to stream reasoning text.
	IncludeThoughts bool
}

// Request is the canonical form of an inbound completion request.
type Request struct {
	// ClientFormat is the wire format the response must be written in.
	ClientFormat Format
	// Model is the requested model id, already normalised.
	Model string
	// Stream reports whether the client asked for an incremental response.
	Stream bool

	// System holds system-instruction parts.
	System []Part
	// Messages is the conversation, oldest first.
	Messages []Message

	Tools      []Tool
	ToolChoice ToolChoice

	Temperature     *float64
	TopP            *float64
	TopK            *int
	MaxOutputTokens *int
	Stop            []string
	Reasoning       Reasoning
	ParallelToolUse *bool

	// ResponseMIMEType and ResponseSchema carry structured-output requests.
	ResponseMIMEType string
	ResponseSchema   json.RawMessage

	// Raw is the untouched request body, used by the passthrough paths.
	Raw []byte
	// Extra keeps format-specific fields that would otherwise be lost.
	Extra map[string]json.RawMessage
}

// SystemText flattens the system instruction to plain text.
func (r *Request) SystemText() string {
	var b strings.Builder
	for _, part := range r.System {
		if part.Type != PartText || part.Text == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(part.Text)
	}
	return b.String()
}

// Usage is the token accounting for one request.
type Usage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	ReasoningTokens  int64 `json:"reasoning_tokens"`
	CachedTokens     int64 `json:"cached_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
}

// Total returns TotalTokens, computing it when the provider omitted it.
func (u Usage) Total() int64 {
	if u.TotalTokens > 0 {
		return u.TotalTokens
	}
	return u.PromptTokens + u.CompletionTokens
}

// EventType discriminates canonical stream events.
type EventType string

const (
	// EventStart is emitted once, before any content.
	EventStart EventType = "start"
	// EventText is a text delta.
	EventText EventType = "text"
	// EventThinking is a reasoning delta.
	EventThinking EventType = "thinking"
	// EventToolCallStart announces a new tool call.
	EventToolCallStart EventType = "tool_call_start"
	// EventToolCallDelta appends to a tool call's arguments.
	EventToolCallDelta EventType = "tool_call_delta"
	// EventToolCallDone closes a tool call.
	EventToolCallDone EventType = "tool_call_done"
	// EventUsage carries token accounting.
	EventUsage EventType = "usage"
	// EventDone ends the response.
	EventDone EventType = "done"
)

// Event is one canonical streaming update.
type Event struct {
	Type EventType

	// Index is the content block index the event belongs to.
	Index int

	// Text carries text and thinking deltas.
	Text string
	// Signature carries an opaque provider token for a thinking block.
	Signature string

	// ToolCallID, ToolName and Arguments describe a tool call.
	ToolCallID string
	ToolName   string
	Arguments  string

	// FinishReason is set on EventDone: "stop", "length", "tool_calls",
	// "content_filter" or "error".
	FinishReason string
	// Usage is set on EventUsage and may be set on EventDone.
	Usage *Usage
	// ResponseID is the upstream response id, when known.
	ResponseID string
	// Model is the upstream model id, when the provider echoes it.
	Model string
}

// Finish reasons in canonical form.
const (
	FinishStop          = "stop"
	FinishLength        = "length"
	FinishToolCalls     = "tool_calls"
	FinishContentFilter = "content_filter"
)

// APIError is an error that carries an HTTP status and a machine-readable type.
type APIError struct {
	// Status is the status this server answers the client with.
	Status  int
	Type    string
	Code    string
	Message string
	// Upstream is the provider's own status code, when the failure came from one.
	// It drives the router's retry and cooldown decisions, and may differ from
	// Status: an upstream 401 is a 502 as far as the client is concerned.
	Upstream int
	// Retryable reports whether another connection might succeed.
	Retryable bool
}

func (e *APIError) Error() string {
	if e.Upstream != 0 && e.Upstream != e.Status {
		return fmt.Sprintf("%s (%d, upstream %d)", e.Message, e.Status, e.Upstream)
	}
	return fmt.Sprintf("%s (%d)", e.Message, e.Status)
}

// newAPIError is a small constructor for the common case.
func newAPIError(status int, errType, message string) *APIError {
	return &APIError{Status: status, Type: errType, Message: message}
}
