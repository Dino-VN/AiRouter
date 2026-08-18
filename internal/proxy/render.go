package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// Renderer turns canonical events into one client wire format. A renderer is
// created only once an upstream has accepted the request, so Begin is always the
// point at which response headers are committed.
type Renderer interface {
	// Begin commits the response headers with the resolved model id.
	Begin(model string)
	// Handle renders one canonical event.
	Handle(ev Event) error
	// Finish writes the terminal frames (or, for non-streaming clients, the
	// whole response body).
	Finish() error
	// Usage reports the token accounting observed on the stream.
	Usage() Usage
	// WriteError reports a failure in the client's own error format.
	WriteError(apiErr *APIError)
}

// NewRenderer builds the renderer for a client format.
func NewRenderer(format Format, w http.ResponseWriter, stream bool) Renderer {
	switch format {
	case FormatOpenAIResponses:
		return newResponsesRenderer(w, stream)
	case FormatAnthropic:
		return newAnthropicRenderer(w, stream)
	case FormatGemini:
		return newGeminiRenderer(w, stream)
	default:
		return newChatRenderer(w, stream)
	}
}

// ParseOptions carries the request facts that live outside the body.
type ParseOptions struct {
	// Model overrides the body's model field; the Gemini API puts it in the URL.
	Model string
	// ForceStream overrides the body's stream flag; the Gemini API and the
	// Anthropic /v1/messages?beta paths signal streaming in the URL.
	ForceStream *bool
}

// ParseRequest converts a client request body into canonical form.
func ParseRequest(format Format, raw []byte, opts ParseOptions) (*Request, error) {
	var (
		req *Request
		err error
	)
	switch format {
	case FormatOpenAIChat:
		req, err = parseOpenAIChat(raw)
	case FormatOpenAIResponses:
		req, err = parseOpenAIResponses(raw)
	case FormatAnthropic:
		req, err = parseAnthropic(raw)
	case FormatGemini:
		req, err = parseGemini(raw, opts.Model)
	default:
		return nil, newAPIError(http.StatusBadRequest, "invalid_request_error",
			fmt.Sprintf("unsupported request format %q", format))
	}
	if err != nil {
		return nil, err
	}
	if opts.Model != "" {
		req.Model = normalizeModel(opts.Model)
	}
	if opts.ForceStream != nil {
		req.Stream = *opts.ForceStream
	}
	return req, nil
}

// ---------------------------------------------------------------------------
// Passthrough rendering
// ---------------------------------------------------------------------------

// rawRenderer hands the provider's own frames to the client. It is used when the
// client format and the upstream protocol are the same, so no fidelity is lost
// to translation (OpenAI Responses over Codex, Gemini over Antigravity).
type rawRenderer struct {
	w      http.ResponseWriter
	sse    *sseWriter
	stream bool

	// separator joins buffered frames for a non-streaming client. The Gemini
	// non-stream API returns a JSON array of chunks when the upstream streamed.
	frames []json.RawMessage
	final  json.RawMessage
	usage  Usage

	// errorFormat renders errors in the client's shape.
	errorFormat Format
}

func newRawRenderer(w http.ResponseWriter, stream bool, format Format) *rawRenderer {
	return &rawRenderer{w: w, stream: stream, errorFormat: format}
}

func (r *rawRenderer) Begin(string) {
	if r.stream {
		r.sse = newSSEWriter(r.w)
		r.sse.WriteHeader(http.StatusOK)
	}
}

// HandleRaw forwards one provider frame.
func (r *rawRenderer) HandleRaw(ev sseEvent) error {
	if r.stream {
		if ev.Name != "" {
			r.sse.Event(ev.Name, ev.Data)
		} else {
			r.sse.Data(ev.Data)
		}
		return r.sse.Err()
	}
	// A non-streaming client gets the aggregate, so keep the frames.
	if json.Valid([]byte(ev.Data)) {
		r.frames = append(r.frames, json.RawMessage(ev.Data))
	}
	return nil
}

// SetFinal records the object a non-streaming client should receive verbatim.
func (r *rawRenderer) SetFinal(body json.RawMessage) { r.final = body }

// SetUsage records token accounting for the usage ledger.
func (r *rawRenderer) SetUsage(usage Usage) { r.usage = usage }

func (r *rawRenderer) Handle(Event) error { return nil }

func (r *rawRenderer) Finish() error {
	if r.stream {
		return r.sse.Err()
	}
	if len(r.final) > 0 {
		r.w.Header().Set("Content-Type", "application/json; charset=utf-8")
		r.w.WriteHeader(http.StatusOK)
		_, err := r.w.Write(r.final)
		return err
	}
	// No aggregate was produced: hand back the frames the upstream did send so
	// the caller still sees the model's answer.
	if len(r.frames) == 1 {
		return writeJSON(r.w, http.StatusOK, r.frames[0])
	}
	return writeJSON(r.w, http.StatusOK, r.frames)
}

func (r *rawRenderer) Usage() Usage { return r.usage }

func (r *rawRenderer) WriteError(apiErr *APIError) {
	writeFormatError(r.w, r.sse, r.stream, r.errorFormat, apiErr)
}

// WriteError reports a failure that happened before any upstream was contacted
// (authentication, quota, an unknown model) in the client's own dialect, so an
// SDK surfaces the message instead of failing to parse the envelope.
func WriteError(w http.ResponseWriter, format Format, status int, errType, message string) {
	writeFormatError(w, nil, false, format, &APIError{
		Status:  status,
		Type:    errType,
		Message: message,
	})
}

// writeFormatError renders an error in a client's own shape, either as a status
// response or, when the stream is already open, as a final frame.
func writeFormatError(w http.ResponseWriter, sse *sseWriter, stream bool, format Format, apiErr *APIError) {
	var payload any
	switch format {
	case FormatAnthropic:
		payload = map[string]any{
			"type": "error",
			"error": map[string]any{
				"type":    firstNonEmpty(apiErr.Type, "api_error"),
				"message": apiErr.Message,
			},
		}
	case FormatGemini:
		payload = map[string]any{"error": map[string]any{
			"code":    apiErr.Status,
			"message": apiErr.Message,
			"status":  firstNonEmpty(apiErr.Code, geminiStatusForCode(apiErr.Status)),
		}}
	default:
		payload = map[string]any{"error": map[string]any{
			"message": apiErr.Message,
			"type":    firstNonEmpty(apiErr.Type, "api_error"),
			"code":    apiErr.Code,
		}}
	}

	if stream && sse != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return
		}
		if format == FormatAnthropic {
			sse.Event("error", string(encoded))
			return
		}
		sse.Data(string(encoded))
		if format == FormatOpenAIChat || format == FormatOpenAIResponses {
			sse.Data("[DONE]")
		}
		return
	}
	_ = writeJSON(w, apiErr.Status, payload)
}

// geminiStatusForCode maps an HTTP status to the Google API status string.
func geminiStatusForCode(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "INVALID_ARGUMENT"
	case http.StatusUnauthorized:
		return "UNAUTHENTICATED"
	case http.StatusForbidden:
		return "PERMISSION_DENIED"
	case http.StatusNotFound:
		return "NOT_FOUND"
	case http.StatusTooManyRequests:
		return "RESOURCE_EXHAUSTED"
	case http.StatusServiceUnavailable:
		return "UNAVAILABLE"
	case http.StatusGatewayTimeout:
		return "DEADLINE_EXCEEDED"
	default:
		return "INTERNAL"
	}
}
