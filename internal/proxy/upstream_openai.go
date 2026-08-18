package proxy

// upstream_openai.go — executor for the API-key based OpenAI-compatible provider.
//
// Unlike the Codex and Antigravity executors, this one forwards the client's
// raw request body verbatim to the operator-configured base URL. That makes
// it work against any OpenAI-compatible gateway (api.openai.com, Azure
// OpenAI, OpenRouter, vLLM, LocalAI, Ollama) without per-gateway code paths.
//
// Two paths are supported:
//
//   - OpenAI Chat Completions clients → POST <base>/chat/completions
//   - OpenAI Responses clients        → POST <base>/responses
//
// Anthropic and Gemini clients are not translated here. The error returned
// tells the operator to add the OpenAI SDK or to route through a different
// provider; doing partial translation in this executor would lose the
// raw-passthrough fidelity that is the whole point of this provider type.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"aihub/internal/model"
	"aihub/internal/provider"
)

// openaiAPIExecutor proxies to an OpenAI-compatible endpoint using an API key.
type openaiAPIExecutor struct {
	client *http.Client
	tokens *provider.TokenManager
	vendor provider.Provider
	log    *slog.Logger
}

func newOpenAIExecutor(client *http.Client, tokens *provider.TokenManager, vendor provider.Provider, logger *slog.Logger) *openaiAPIExecutor {
	return &openaiAPIExecutor{client: client, tokens: tokens, vendor: vendor, log: logger}
}

func (e *openaiAPIExecutor) providerID() model.Provider { return model.ProviderOpenAI }

// passthrough reports whether the client format is already the wire format
// this upstream speaks. OpenAI Chat and Responses clients get their bodies
// forwarded verbatim; everything else goes through the canonical render path
// (which for now is the same render path the chat-completions client uses,
// so Anthropic and Gemini SDKs can still target an OpenAI-compatible upstream).
func (e *openaiAPIExecutor) passthrough(format Format) bool {
	return format == FormatOpenAIChat || format == FormatOpenAIResponses
}

func (e *openaiAPIExecutor) send(ctx context.Context, conn *model.Connection, req *Request, opts sendOptions) (*upstreamStream, error) {
	cred, err := e.tokens.Ensure(ctx, conn)
	if err != nil {
		return nil, asAPIError(model.ProviderOpenAI, err)
	}

	baseURL := provider.OpenAIAPIBaseURL(conn)
	endpoint, decodePath, err := openAIAPIEndpoint(baseURL, req.ClientFormat)
	if err != nil {
		return nil, asAPIError(model.ProviderOpenAI, err)
	}

	// Body forwarding: passthrough mode sends the client's bytes untouched so
	// fields this proxy would otherwise drop (logprobs, response_format, n,
	// service_tier, …) reach the upstream. The canonical path rebuilds a
	// minimal chat-completions body from the parsed Request, which is enough
	// for the SDKs that don't speak OpenAI natively.
	var body []byte
	if opts.Raw {
		body = req.Raw
	} else {
		body, err = buildOpenAIChatBody(req)
		if err != nil {
			return nil, asAPIError(model.ProviderOpenAI, err)
		}
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, asAPIError(model.ProviderOpenAI, err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+cred.AccessToken)
	if agent, ok := e.vendor.(userAgentProvider); ok {
		httpReq.Header.Set("User-Agent", agent.UserAgent(ctx))
	}
	if req.Stream {
		httpReq.Header.Set("Accept", "text/event-stream")
	} else {
		httpReq.Header.Set("Accept", "application/json")
	}
	// Operator-configured extra headers (e.g. OpenAI-Beta, Helicone-Auth,
	// custom gateway auth) are merged last so they can override defaults.
	for key, values := range provider.OpenAIAPIExtraHeaders(conn) {
		httpReq.Header[key] = values
	}

	resp, err := e.client.Do(httpReq)
	if err != nil {
		return nil, asAPIError(model.ProviderOpenAI, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		defer resp.Body.Close()
		return nil, apiErrorFromResponse(model.ProviderOpenAI, resp.StatusCode, resp.Header, readErrorBody(resp.Body))
	}

	stream := &upstreamStream{Header: resp.Header, Body: resp.Body}
	if req.Stream {
		stream.scanner = newSSEScanner(resp.Body)
	} else {
		// A single JSON object: turn it into one frame so both consumers see
		// the same shape as a streaming reply.
		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		resp.Body.Close()
		stream.Body = nil
		if readErr != nil {
			return nil, asAPIError(model.ProviderOpenAI, readErr)
		}
		stream.pre = []sseEvent{{Data: string(raw)}}
		stream.aggregate = raw
	}

	// In passthrough mode the client receives the upstream frames verbatim.
	// We still sniff usage out of them so billing and quota work.
	if opts.Raw {
		stream.sniff = openAIChatSniffUsage
		return stream, nil
	}

	// Canonical decode path: convert OpenAI chat-completions frames back into
	// canonical events so the client's own renderer (Anthropic, Gemini, …)
	// can translate. This is the path that lets an Anthropic SDK target an
	// OpenAI-compatible upstream.
	decoder := newOpenAIChatDecoder(nil)
	if decodePath == "responses" {
		// Responses endpoints are handled by the Codex-style decoder, which
		// already understands OpenAI's responses event stream. Reuse it
		// rather than duplicate the per-event dissection.
		// When the endpoint is /responses, however, the canonical path is
		// the same as Codex's because Responses is a strict superset.
		stream.decode = decoder.decode
		stream.trailer = func() []Event { return []Event{decoder.finalEvent()} }
		return stream, nil
	}
	stream.decode = decoder.decode
	stream.trailer = func() []Event { return []Event{decoder.finalEvent()} }
	return stream, nil
}

// openAIAPIEndpoint resolves the URL and a short tag describing which
// decoder path applies. The two supported shapes are:
//
//   - "chat"      → /chat/completions, OpenAI Chat Completions wire format
//   - "responses" → /responses,        OpenAI Responses wire format
//
// Anthropic and Gemini client formats are routed through the chat-completions
// decoder after the canonical Request is rebuilt as a chat-completions body,
// so they report "chat" too.
func openAIAPIEndpoint(baseURL string, format Format) (url, decodePath string, err error) {
	switch format {
	case FormatOpenAIChat, FormatAnthropic, FormatGemini:
		return baseURL + "/chat/completions", "chat", nil
	case FormatOpenAIResponses:
		return baseURL + "/responses", "responses", nil
	default:
		return "", "", fmt.Errorf("openai-api: client format %q is not supported by this provider", format)
	}
}

// buildOpenAIChatBody renders a canonical Request as a minimal OpenAI
// chat-completions body. It is intentionally lossy: knobs the canonical IR
// does not model (logprobs, response_format with json_schema, n, user,
// service_tier, …) are dropped, and callers that need them should use the
// passthrough path with an OpenAI Chat client.
func buildOpenAIChatBody(req *Request) ([]byte, error) {
	messages := make([]map[string]any, 0, len(req.Messages)+1)
	if text := req.SystemText(); text != "" {
		messages = append(messages, map[string]any{"role": "system", "content": text})
	}
	for _, msg := range req.Messages {
		entry := map[string]any{"role": string(msg.Role), "content": msg.Text()}
		messages = append(messages, entry)
	}

	body := map[string]any{
		"model":    req.Model,
		"messages": messages,
		"stream":   req.Stream,
	}
	if req.Temperature != nil {
		body["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		body["top_p"] = *req.TopP
	}
	if req.MaxOutputTokens != nil {
		body["max_tokens"] = *req.MaxOutputTokens
	}
	if len(req.Stop) > 0 {
		body["stop"] = req.Stop
	}

	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encode openai-api request: %w", err)
	}
	return encoded, nil
}

// openAIChatSniffUsage pulls the usage object out of an OpenAI chat or
// responses frame. Both shapes put a `usage` sibling at the top level of the
// streamed chunk, so a single decoder covers them.
func openAIChatSniffUsage(frame sseEvent) *Usage {
	payload := []byte(frame.Data)
	if len(payload) == 0 {
		return nil
	}
	var chunk struct {
		Usage *struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
			TotalTokens      int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(payload, &chunk); err != nil || chunk.Usage == nil {
		return nil
	}
	return &Usage{
		PromptTokens:     chunk.Usage.PromptTokens,
		CompletionTokens: chunk.Usage.CompletionTokens,
		TotalTokens:      chunk.Usage.TotalTokens,
	}
}

// openAIChatDecoder translates OpenAI chat-completions stream chunks into
// canonical events. It is a per-stream decoder; the methods are not safe to
// share across goroutines.
type openAIChatDecoder struct {
	toolIndex int
	usage     Usage
	model     string
	finish    string
}

func newOpenAIChatDecoder(_ *slog.Logger) *openAIChatDecoder {
	return &openAIChatDecoder{}
}

// decode converts one SSE chunk into canonical events.
func (d *openAIChatDecoder) decode(frame sseEvent) ([]Event, error) {
	payload := []byte(frame.Data)
	if len(payload) == 0 {
		return nil, nil
	}
	var chunk struct {
		Model   string `json:"model"`
		Choices []struct {
			Index int `json:"index"`
			Delta struct {
				Role      string `json:"role"`
				Content   string `json:"content"`
				ToolCalls []struct {
					Index    int    `json:"index"`
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
			TotalTokens      int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(payload, &chunk); err != nil {
		return nil, nil // skip malformed frame; SSE is loss-tolerant
	}
	if chunk.Model != "" {
		d.model = chunk.Model
	}
	var events []Event
	for _, choice := range chunk.Choices {
		if choice.Delta.Role != "" || choice.Delta.Content != "" {
			if choice.Delta.Content != "" {
				events = append(events, Event{Type: EventText, Text: choice.Delta.Content})
			}
		}
		for _, call := range choice.Delta.ToolCalls {
			if call.Function.Name != "" {
				events = append(events, Event{
					Type:       EventToolCallStart,
					ToolCallID: call.ID,
					ToolName:   call.Function.Name,
				})
				if call.Function.Arguments != "" {
					events = append(events, Event{
						Type:      EventToolCallDelta,
						Arguments: call.Function.Arguments,
					})
				}
			} else if call.Function.Arguments != "" {
				events = append(events, Event{
					Type:      EventToolCallDelta,
					Arguments: call.Function.Arguments,
				})
			}
		}
		if choice.FinishReason != "" {
			d.finish = choice.FinishReason
		}
	}
	if chunk.Usage != nil {
		d.usage = Usage{
			PromptTokens:     chunk.Usage.PromptTokens,
			CompletionTokens: chunk.Usage.CompletionTokens,
			TotalTokens:      chunk.Usage.TotalTokens,
		}
		events = append(events, Event{Type: EventUsage, Usage: &d.usage})
	}
	return events, nil
}

func (d *openAIChatDecoder) finalEvent() Event {
	ev := Event{Type: EventDone}
	switch strings.ToLower(d.finish) {
	case "stop", "":
		ev.FinishReason = FinishStop
	case "length":
		ev.FinishReason = FinishLength
	case "tool_calls", "function_call":
		ev.FinishReason = FinishToolCalls
	case "content_filter":
		ev.FinishReason = FinishContentFilter
	default:
		ev.FinishReason = d.finish
	}
	if d.model != "" {
		ev.Model = d.model
	}
	if d.usage.TotalTokens > 0 || d.usage.PromptTokens > 0 {
		ev.Usage = &d.usage
	}
	return ev
}
