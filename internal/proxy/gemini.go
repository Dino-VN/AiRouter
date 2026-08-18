package proxy

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// Wire types
// ---------------------------------------------------------------------------

// geminiPart is one part of a Gemini content block.
type geminiPart struct {
	Text       string `json:"text,omitempty"`
	Thought    bool   `json:"thought,omitempty"`
	InlineData *struct {
		MimeType string `json:"mimeType"`
		Data     string `json:"data"`
	} `json:"inlineData,omitempty"`
	FileData *struct {
		MimeType string `json:"mimeType"`
		FileURI  string `json:"fileUri"`
	} `json:"fileData,omitempty"`
	FunctionCall *struct {
		ID   string          `json:"id,omitempty"`
		Name string          `json:"name"`
		Args json.RawMessage `json:"args,omitempty"`
	} `json:"functionCall,omitempty"`
	FunctionResponse *struct {
		ID       string          `json:"id,omitempty"`
		Name     string          `json:"name"`
		Response json.RawMessage `json:"response,omitempty"`
	} `json:"functionResponse,omitempty"`
	ThoughtSignature string `json:"thoughtSignature,omitempty"`
}

type geminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []geminiPart `json:"parts,omitempty"`
}

type geminiRequestBody struct {
	Contents          []geminiContent `json:"contents"`
	SystemInstruction *geminiContent  `json:"systemInstruction"`
	Tools             []struct {
		FunctionDeclarations []struct {
			Name                 string          `json:"name"`
			Description          string          `json:"description"`
			Parameters           json.RawMessage `json:"parameters"`
			ParametersJSONSchema json.RawMessage `json:"parametersJsonSchema"`
		} `json:"functionDeclarations"`
	} `json:"tools"`
	ToolConfig *struct {
		FunctionCallingConfig *struct {
			Mode                 string   `json:"mode"`
			AllowedFunctionNames []string `json:"allowedFunctionNames"`
		} `json:"functionCallingConfig"`
	} `json:"toolConfig"`
	GenerationConfig *struct {
		Temperature      *float64        `json:"temperature"`
		TopP             *float64        `json:"topP"`
		TopK             *int            `json:"topK"`
		MaxOutputTokens  *int            `json:"maxOutputTokens"`
		StopSequences    []string        `json:"stopSequences"`
		ResponseMimeType string          `json:"responseMimeType"`
		ResponseSchema   json.RawMessage `json:"responseSchema"`
		ThinkingConfig   *struct {
			IncludeThoughts bool `json:"includeThoughts"`
			ThinkingBudget  *int `json:"thinkingBudget"`
		} `json:"thinkingConfig"`
	} `json:"generationConfig"`
}

// geminiResponse is the shape of a generateContent response (and of each
// streamGenerateContent chunk).
type geminiResponse struct {
	Candidates []struct {
		Content      geminiContent `json:"content"`
		FinishReason string        `json:"finishReason"`
		Index        int           `json:"index"`
	} `json:"candidates"`
	UsageMetadata *struct {
		PromptTokenCount        int64 `json:"promptTokenCount"`
		CandidatesTokenCount    int64 `json:"candidatesTokenCount"`
		ThoughtsTokenCount      int64 `json:"thoughtsTokenCount"`
		CachedContentTokenCount int64 `json:"cachedContentTokenCount"`
		TotalTokenCount         int64 `json:"totalTokenCount"`
	} `json:"usageMetadata"`
	ModelVersion   string `json:"modelVersion"`
	ResponseID     string `json:"responseId"`
	PromptFeedback *struct {
		BlockReason string `json:"blockReason"`
	} `json:"promptFeedback"`
}

// ---------------------------------------------------------------------------
// Request parsing
// ---------------------------------------------------------------------------

// parseGemini converts a generateContent body to canonical form. The model comes
// from the URL, so it is passed in.
func parseGemini(raw []byte, modelFromPath string) (*Request, error) {
	var body geminiRequestBody
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, newAPIError(http.StatusBadRequest, "invalid_request_error",
			"could not parse request body: "+err.Error())
	}

	req := &Request{
		ClientFormat: FormatGemini,
		Model:        normalizeModel(modelFromPath),
		Raw:          raw,
	}
	if body.SystemInstruction != nil {
		req.System = geminiPartsToCanonical(body.SystemInstruction.Parts)
	}
	if cfg := body.GenerationConfig; cfg != nil {
		req.Temperature = cfg.Temperature
		req.TopP = cfg.TopP
		req.TopK = cfg.TopK
		req.MaxOutputTokens = cfg.MaxOutputTokens
		req.Stop = cfg.StopSequences
		req.ResponseMIMEType = cfg.ResponseMimeType
		req.ResponseSchema = cfg.ResponseSchema
		if cfg.ThinkingConfig != nil {
			req.Reasoning = Reasoning{
				Enabled:         true,
				IncludeThoughts: cfg.ThinkingConfig.IncludeThoughts,
			}
			if cfg.ThinkingConfig.ThinkingBudget != nil {
				req.Reasoning.BudgetTokens = *cfg.ThinkingConfig.ThinkingBudget
				req.Reasoning.Effort = effortForBudget(*cfg.ThinkingConfig.ThinkingBudget)
			}
		}
	}

	for _, tool := range body.Tools {
		for _, decl := range tool.FunctionDeclarations {
			if decl.Name == "" {
				continue
			}
			schema := decl.Parameters
			if len(schema) == 0 {
				schema = decl.ParametersJSONSchema
			}
			req.Tools = append(req.Tools, Tool{
				Name:        decl.Name,
				Description: decl.Description,
				Parameters:  schema,
			})
		}
	}
	if body.ToolConfig != nil && body.ToolConfig.FunctionCallingConfig != nil {
		cfg := body.ToolConfig.FunctionCallingConfig
		switch strings.ToUpper(cfg.Mode) {
		case "AUTO":
			req.ToolChoice = ToolChoice{Mode: "auto"}
		case "NONE":
			req.ToolChoice = ToolChoice{Mode: "none"}
		case "ANY", "VALIDATED":
			req.ToolChoice = ToolChoice{Mode: "required"}
			if len(cfg.AllowedFunctionNames) == 1 {
				req.ToolChoice = ToolChoice{Mode: "tool", Name: cfg.AllowedFunctionNames[0]}
			}
		}
	}

	for _, content := range body.Contents {
		parts := geminiPartsToCanonical(content.Parts)
		if len(parts) == 0 {
			continue
		}
		var results, rest []Part
		for _, part := range parts {
			if part.Type == PartToolResult {
				results = append(results, part)
				continue
			}
			rest = append(rest, part)
		}
		for _, result := range results {
			req.Messages = append(req.Messages, Message{Role: RoleTool, Parts: []Part{result}})
		}
		if len(rest) == 0 {
			continue
		}
		role := RoleUser
		if content.Role == "model" {
			role = RoleAssistant
		}
		req.Messages = append(req.Messages, Message{Role: role, Parts: rest})
	}

	if len(req.Messages) == 0 {
		return nil, newAPIError(http.StatusBadRequest, "invalid_request_error", "contents must not be empty")
	}
	return req, nil
}

func geminiPartsToCanonical(parts []geminiPart) []Part {
	var out []Part
	for _, part := range parts {
		switch {
		case part.FunctionCall != nil:
			arguments := "{}"
			if len(part.FunctionCall.Args) > 0 {
				arguments = string(part.FunctionCall.Args)
			}
			out = append(out, Part{
				Type:       PartToolCall,
				ToolCallID: firstNonEmpty(part.FunctionCall.ID, part.FunctionCall.Name),
				ToolName:   part.FunctionCall.Name,
				Arguments:  arguments,
				Signature:  part.ThoughtSignature,
			})
		case part.FunctionResponse != nil:
			out = append(out, Part{
				Type:       PartToolResult,
				ToolCallID: firstNonEmpty(part.FunctionResponse.ID, part.FunctionResponse.Name),
				ToolName:   part.FunctionResponse.Name,
				Text:       geminiResponseText(part.FunctionResponse.Response),
			})
		case part.InlineData != nil:
			out = append(out, Part{
				Type:     PartImage,
				MimeType: part.InlineData.MimeType,
				Data:     part.InlineData.Data,
			})
		case part.FileData != nil:
			out = append(out, Part{Type: PartImage, URL: part.FileData.FileURI, MimeType: part.FileData.MimeType})
		case part.Thought:
			out = append(out, Part{Type: PartThinking, Text: part.Text, Signature: part.ThoughtSignature})
		case part.Text != "":
			out = append(out, Part{Type: PartText, Text: part.Text, Signature: part.ThoughtSignature})
		}
	}
	return out
}

// geminiResponseText flattens a functionResponse payload. Gemini wraps tool
// output in {"output": ...} or {"result": ...} depending on the client.
func geminiResponseText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var wrapper map[string]json.RawMessage
	if err := json.Unmarshal(raw, &wrapper); err == nil {
		for _, key := range []string{"output", "result", "content", "response"} {
			if value, ok := wrapper[key]; ok {
				var text string
				if err := json.Unmarshal(value, &text); err == nil {
					return text
				}
				return string(value)
			}
		}
	}
	return string(raw)
}

// ---------------------------------------------------------------------------
// Request building (canonical -> Gemini)
// ---------------------------------------------------------------------------

// buildGeminiRequest renders a canonical request as a Gemini generateContent
// body. Antigravity wraps this in its own envelope.
func buildGeminiRequest(req *Request) ([]byte, error) {
	body := map[string]any{}

	contents := make([]any, 0, len(req.Messages))
	for _, msg := range req.Messages {
		parts := canonicalPartsToGemini(msg)
		if len(parts) == 0 {
			continue
		}
		role := "user"
		if msg.Role == RoleAssistant {
			role = "model"
		}
		contents = append(contents, map[string]any{"role": role, "parts": parts})
	}
	body["contents"] = contents

	if system := req.SystemText(); system != "" {
		body["systemInstruction"] = map[string]any{
			"role":  "user",
			"parts": []any{map[string]any{"text": system}},
		}
	}

	if len(req.Tools) > 0 {
		declarations := make([]any, 0, len(req.Tools))
		for _, tool := range req.Tools {
			decl := map[string]any{"name": tool.Name}
			if tool.Description != "" {
				decl["description"] = tool.Description
			}
			if schema := sanitizeGeminiSchema(tool.Parameters); len(schema) > 0 {
				decl["parameters"] = json.RawMessage(schema)
			}
			declarations = append(declarations, decl)
		}
		body["tools"] = []any{map[string]any{"functionDeclarations": declarations}}
	}

	switch req.ToolChoice.Mode {
	case "auto":
		body["toolConfig"] = geminiToolConfig("AUTO", nil)
	case "none":
		body["toolConfig"] = geminiToolConfig("NONE", nil)
	case "required":
		body["toolConfig"] = geminiToolConfig("ANY", nil)
	case "tool":
		body["toolConfig"] = geminiToolConfig("ANY", []string{req.ToolChoice.Name})
	}

	generation := map[string]any{}
	if req.Temperature != nil {
		generation["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		generation["topP"] = *req.TopP
	}
	if req.TopK != nil {
		generation["topK"] = *req.TopK
	}
	if req.MaxOutputTokens != nil && *req.MaxOutputTokens > 0 {
		generation["maxOutputTokens"] = *req.MaxOutputTokens
	}
	if len(req.Stop) > 0 {
		generation["stopSequences"] = req.Stop
	}
	if req.ResponseMIMEType != "" {
		generation["responseMimeType"] = req.ResponseMIMEType
	}
	if schema := sanitizeGeminiSchema(req.ResponseSchema); len(schema) > 0 {
		generation["responseSchema"] = json.RawMessage(schema)
	}
	if req.Reasoning.Enabled {
		thinking := map[string]any{"includeThoughts": req.Reasoning.IncludeThoughts}
		if req.Reasoning.BudgetTokens > 0 {
			thinking["thinkingBudget"] = req.Reasoning.BudgetTokens
		}
		generation["thinkingConfig"] = thinking
	}
	if len(generation) > 0 {
		body["generationConfig"] = generation
	}

	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, newAPIError(http.StatusInternalServerError, "api_error",
			"could not encode upstream request: "+err.Error())
	}
	return encoded, nil
}

func geminiToolConfig(mode string, allowed []string) map[string]any {
	cfg := map[string]any{"mode": mode}
	if len(allowed) > 0 {
		cfg["allowedFunctionNames"] = allowed
	}
	return map[string]any{"functionCallingConfig": cfg}
}

// canonicalPartsToGemini renders one canonical message as Gemini parts.
func canonicalPartsToGemini(msg Message) []any {
	parts := make([]any, 0, len(msg.Parts))
	for _, part := range msg.Parts {
		switch part.Type {
		case PartText:
			if part.Text == "" {
				continue
			}
			parts = append(parts, map[string]any{"text": part.Text})
		case PartThinking:
			if part.Text == "" && part.Signature == "" {
				continue
			}
			entry := map[string]any{"text": part.Text, "thought": true}
			if part.Signature != "" {
				entry["thoughtSignature"] = part.Signature
			}
			parts = append(parts, entry)
		case PartImage:
			if part.Data != "" {
				parts = append(parts, map[string]any{"inlineData": map[string]any{
					"mimeType": firstNonEmpty(part.MimeType, "image/png"),
					"data":     part.Data,
				}})
				continue
			}
			if part.URL != "" {
				parts = append(parts, map[string]any{"fileData": map[string]any{
					"mimeType": firstNonEmpty(part.MimeType, "image/png"),
					"fileUri":  part.URL,
				}})
			}
		case PartToolCall:
			call := map[string]any{"name": part.ToolName}
			var args any = map[string]any{}
			if raw := strings.TrimSpace(part.Arguments); raw != "" {
				var decoded any
				if err := json.Unmarshal([]byte(raw), &decoded); err == nil {
					args = decoded
				}
			}
			call["args"] = args
			entry := map[string]any{"functionCall": call}
			if part.Signature != "" {
				entry["thoughtSignature"] = part.Signature
			}
			parts = append(parts, entry)
		case PartToolResult:
			name := part.ToolName
			if name == "" {
				name = part.ToolCallID
			}
			payload := map[string]any{"output": part.Text}
			if part.IsError {
				payload = map[string]any{"error": part.Text}
			}
			parts = append(parts, map[string]any{"functionResponse": map[string]any{
				"name":     name,
				"response": payload,
			}})
		}
	}
	return parts
}

// ---------------------------------------------------------------------------
// Response decoding (Gemini -> canonical)
// ---------------------------------------------------------------------------

// geminiDecoder turns Gemini chunks into canonical events, keeping the little
// state a stream needs (whether the start event went out, the model id).
type geminiDecoder struct {
	started bool
	model   string
	calls   int
	finish  string
	usage   *Usage
}

func newGeminiDecoder(usage *Usage) *geminiDecoder {
	if usage == nil {
		usage = &Usage{}
	}
	return &geminiDecoder{usage: usage}
}

// decode converts one chunk into canonical events.
func (d *geminiDecoder) decode(data []byte) ([]Event, error) {
	var chunk geminiResponse
	if err := json.Unmarshal(data, &chunk); err != nil {
		return nil, nil
	}

	var events []Event
	if !d.started {
		d.started = true
		d.model = chunk.ModelVersion
		events = append(events, Event{
			Type:       EventStart,
			Model:      chunk.ModelVersion,
			ResponseID: chunk.ResponseID,
		})
	}

	if chunk.UsageMetadata != nil && d.usage != nil {
		d.usage.PromptTokens = chunk.UsageMetadata.PromptTokenCount
		d.usage.CompletionTokens = chunk.UsageMetadata.CandidatesTokenCount
		d.usage.ReasoningTokens = chunk.UsageMetadata.ThoughtsTokenCount
		d.usage.CachedTokens = chunk.UsageMetadata.CachedContentTokenCount
		d.usage.TotalTokens = chunk.UsageMetadata.TotalTokenCount
	}

	for _, candidate := range chunk.Candidates {
		for _, part := range candidate.Content.Parts {
			switch {
			case part.FunctionCall != nil:
				id := part.FunctionCall.ID
				if id == "" {
					id = "call_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:24]
				}
				arguments := "{}"
				if len(part.FunctionCall.Args) > 0 {
					arguments = string(part.FunctionCall.Args)
				}
				events = append(events,
					Event{Type: EventToolCallStart, Index: d.calls, ToolCallID: id, ToolName: part.FunctionCall.Name},
					Event{Type: EventToolCallDelta, Index: d.calls, ToolCallID: id, Arguments: arguments},
					Event{Type: EventToolCallDone, Index: d.calls, ToolCallID: id, ToolName: part.FunctionCall.Name},
				)
				d.calls++
				d.finish = FinishToolCalls
			case part.Thought:
				if part.Text == "" && part.ThoughtSignature == "" {
					continue
				}
				events = append(events, Event{
					Type:      EventThinking,
					Text:      part.Text,
					Signature: part.ThoughtSignature,
				})
			case part.InlineData != nil:
				// Image output has no canonical representation for the text
				// formats, so it is surfaced as a data URL.
				events = append(events, Event{
					Type: EventText,
					Text: "\n![image](data:" + part.InlineData.MimeType + ";base64," + part.InlineData.Data + ")\n",
				})
			case part.Text != "":
				events = append(events, Event{Type: EventText, Text: part.Text, Signature: part.ThoughtSignature})
			}
		}
		if candidate.FinishReason != "" && d.finish != FinishToolCalls {
			d.finish = canonicalFinishFromGemini(candidate.FinishReason)
		}
	}

	if chunk.PromptFeedback != nil && chunk.PromptFeedback.BlockReason != "" {
		d.finish = FinishContentFilter
	}
	return events, nil
}

// finalEvent closes the canonical stream.
func (d *geminiDecoder) finalEvent() Event {
	finish := d.finish
	if finish == "" {
		finish = FinishStop
	}
	ev := Event{Type: EventDone, FinishReason: finish}
	if d.usage != nil {
		usage := *d.usage
		ev.Usage = &usage
	}
	return ev
}

func canonicalFinishFromGemini(reason string) string {
	switch strings.ToUpper(reason) {
	case "MAX_TOKENS":
		return FinishLength
	case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII":
		return FinishContentFilter
	case "MALFORMED_FUNCTION_CALL":
		return FinishToolCalls
	default:
		return FinishStop
	}
}

// ---------------------------------------------------------------------------
// Response rendering
// ---------------------------------------------------------------------------

// geminiRenderer renders canonical events as Gemini generateContent output.
type geminiRenderer struct {
	w      http.ResponseWriter
	sse    *sseWriter
	stream bool

	responseID string
	model      string

	parts   []map[string]any
	pending *renderedToolCall
	usage   Usage
	finish  string
}

func newGeminiRenderer(w http.ResponseWriter, stream bool) *geminiRenderer {
	return &geminiRenderer{
		w:          w,
		stream:     stream,
		responseID: strings.ReplaceAll(uuid.NewString(), "-", ""),
	}
}

func (r *geminiRenderer) Begin(model string) {
	r.model = model
	if r.stream {
		r.sse = newSSEWriter(r.w)
		r.sse.WriteHeader(http.StatusOK)
	}
}

func (r *geminiRenderer) Handle(ev Event) error {
	if ev.Model != "" {
		r.model = ev.Model
	}

	switch ev.Type {
	case EventText:
		if ev.Text == "" {
			return nil
		}
		r.flushPending()
		part := map[string]any{"text": ev.Text}
		r.emitPart(part)
	case EventThinking:
		if ev.Text == "" {
			return nil
		}
		r.flushPending()
		part := map[string]any{"text": ev.Text, "thought": true}
		if ev.Signature != "" {
			part["thoughtSignature"] = ev.Signature
		}
		r.emitPart(part)
	case EventToolCallStart:
		r.flushPending()
		r.pending = &renderedToolCall{ID: ev.ToolCallID, Name: ev.ToolName}
	case EventToolCallDelta:
		if r.pending == nil {
			r.pending = &renderedToolCall{ID: ev.ToolCallID, Name: ev.ToolName}
		}
		r.pending.Arguments.WriteString(ev.Arguments)
	case EventToolCallDone:
		if r.pending == nil {
			r.pending = &renderedToolCall{ID: ev.ToolCallID, Name: ev.ToolName}
		}
		if r.pending.Arguments.Len() == 0 && ev.Arguments != "" {
			r.pending.Arguments.WriteString(ev.Arguments)
		}
		if r.pending.Name == "" {
			r.pending.Name = ev.ToolName
		}
		r.flushPending()
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

// flushPending materialises a buffered tool call: Gemini has no argument deltas,
// so the call is only emitted once its arguments are complete.
func (r *geminiRenderer) flushPending() {
	call := r.pending
	if call == nil {
		return
	}
	r.pending = nil

	var args any = map[string]any{}
	if raw := strings.TrimSpace(call.Arguments.String()); raw != "" {
		var decoded any
		if err := json.Unmarshal([]byte(raw), &decoded); err == nil {
			args = decoded
		}
	}
	r.emitPart(map[string]any{"functionCall": map[string]any{
		"name": call.Name,
		"args": args,
	}})
}

// emitPart streams one part, or buffers it for a non-streaming client.
func (r *geminiRenderer) emitPart(part map[string]any) {
	if !r.stream {
		r.parts = append(r.parts, part)
		return
	}
	r.write(map[string]any{
		"candidates": []any{map[string]any{
			"content": map[string]any{"role": "model", "parts": []any{part}},
			"index":   0,
		}},
		"modelVersion": r.model,
		"responseId":   r.responseID,
	})
}

func (r *geminiRenderer) Finish() error {
	r.flushPending()
	finish := geminiFinishReason(r.finish)

	if r.stream {
		r.write(map[string]any{
			"candidates": []any{map[string]any{
				"content":      map[string]any{"role": "model", "parts": []any{}},
				"finishReason": finish,
				"index":        0,
			}},
			"usageMetadata": r.usagePayload(),
			"modelVersion":  r.model,
			"responseId":    r.responseID,
		})
		return r.sse.Err()
	}

	parts := make([]any, 0, len(r.parts))
	for _, part := range r.parts {
		parts = append(parts, part)
	}
	return writeJSON(r.w, http.StatusOK, map[string]any{
		"candidates": []any{map[string]any{
			"content":      map[string]any{"role": "model", "parts": parts},
			"finishReason": finish,
			"index":        0,
		}},
		"usageMetadata": r.usagePayload(),
		"modelVersion":  r.model,
		"responseId":    r.responseID,
	})
}

func (r *geminiRenderer) usagePayload() map[string]any {
	usage := map[string]any{
		"promptTokenCount":     r.usage.PromptTokens,
		"candidatesTokenCount": r.usage.CompletionTokens,
		"totalTokenCount":      r.usage.Total(),
	}
	if r.usage.ReasoningTokens > 0 {
		usage["thoughtsTokenCount"] = r.usage.ReasoningTokens
	}
	if r.usage.CachedTokens > 0 {
		usage["cachedContentTokenCount"] = r.usage.CachedTokens
	}
	return usage
}

func (r *geminiRenderer) write(payload map[string]any) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return
	}
	r.sse.Data(string(encoded))
}

func (r *geminiRenderer) Usage() Usage { return r.usage }

func (r *geminiRenderer) WriteError(apiErr *APIError) {
	writeFormatError(r.w, r.sse, r.stream, FormatGemini, apiErr)
}

func geminiFinishReason(finish string) string {
	switch finish {
	case FinishLength:
		return "MAX_TOKENS"
	case FinishContentFilter:
		return "SAFETY"
	default:
		return "STOP"
	}
}
