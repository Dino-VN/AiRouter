package proxy

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestAntigravityFilter_BlockMode verifies that a request whose `system`
// field mentions a known coding client is rejected under block mode.
func TestAntigravityFilter_BlockMode(t *testing.T) {
	filter := NewAntigravityFilter(AntigravityFilterBlock, true, nil)
	if filter == nil {
		t.Fatal("expected non-nil filter in block mode")
	}
	body := []byte(`{
		"model": "gemini-2.5-pro",
		"system": "You are an assistant helping with code in Cursor.",
		"contents": [{"role": "user", "parts": [{"text": "hello"}]}]
	}`)
	_, decision := filter.Apply(body)
	if !decision.Blocked {
		t.Fatalf("expected request blocked, got %+v", decision)
	}
	if decision.Signal != "system.keyword" {
		t.Errorf("unexpected signal %q", decision.Signal)
	}
	if decision.Detail != "cursor" {
		t.Errorf("unexpected detail %q (want \"cursor\")", decision.Detail)
	}
}

// TestAntigravityFilter_BlockMode_NoMatch ensures clean requests pass.
func TestAntigravityFilter_BlockMode_NoMatch(t *testing.T) {
	filter := NewAntigravityFilter(AntigravityFilterBlock, true, nil)
	body := []byte(`{
		"model": "gemini-2.5-pro",
		"system": "You are a helpful assistant.",
		"contents": [{"role": "user", "parts": [{"text": "hello"}]}]
	}`)
	_, decision := filter.Apply(body)
	if decision.Blocked {
		t.Fatalf("expected pass-through, got %+v", decision)
	}
}

// TestAntigravityFilter_BlockMode_OnlySystemField ensures that a keyword in
// `contents` (user message) does not trigger the filter. This mirrors the
// plugin behaviour: only the `system` field is scanned.
func TestAntigravityFilter_BlockMode_OnlySystemField(t *testing.T) {
	filter := NewAntigravityFilter(AntigravityFilterBlock, true, nil)
	body := []byte(`{
		"model": "gemini-2.5-pro",
		"system": "You are a helpful assistant.",
		"contents": [{"role": "user", "parts": [{"text": "I am using Cursor"}]}]
	}`)
	_, decision := filter.Apply(body)
	if decision.Blocked {
		t.Fatalf("keyword inside user prompt must not trigger filter, got %+v", decision)
	}
}

// TestAntigravityFilter_RewriteMode verifies that matched names are replaced
// with "Antigravity" and the rewritten body is returned.
func TestAntigravityFilter_RewriteMode(t *testing.T) {
	filter := NewAntigravityFilter(AntigravityFilterRewrite, true, nil)
	body := []byte(`{
		"model": "claude-3-5-sonnet",
		"system": "You are an assistant for Cursor and Windsurf users.",
		"contents": [{"role": "user", "parts": [{"text": "hello"}]}]
	}`)
	rewritten, decision := filter.Apply(body)
	if decision.Blocked {
		t.Fatalf("rewrite mode should never block, got %+v", decision)
	}
	if rewritten == nil {
		t.Fatal("expected rewritten body, got nil")
	}
	var got map[string]any
	if err := json.Unmarshal(rewritten, &got); err != nil {
		t.Fatalf("rewritten body is not valid JSON: %v", err)
	}
	system, _ := got["system"].(string)
	if !strings.Contains(system, "Antigravity") {
		t.Errorf("expected 'Antigravity' in rewritten system, got %q", system)
	}
	if strings.Contains(system, "Cursor") || strings.Contains(system, "Windsurf") {
		t.Errorf("matched keywords should be removed, got %q", system)
	}
}

// TestAntigravityFilter_RewriteMode_NoChange ensures that when no keyword is
// present, the body is returned unchanged (nil).
func TestAntigravityFilter_RewriteMode_NoChange(t *testing.T) {
	filter := NewAntigravityFilter(AntigravityFilterRewrite, true, nil)
	body := []byte(`{"system":"You are helpful.","contents":[]}`)
	rewritten, _ := filter.Apply(body)
	if rewritten != nil {
		t.Errorf("expected nil body when no rewrite happens, got %d bytes", len(rewritten))
	}
}

// TestAntigravityFilter_OffMode verifies that NewAntigravityFilter returns
// nil when mode is off, so the Router can skip the filter entirely.
func TestAntigravityFilter_OffMode(t *testing.T) {
	filter := NewAntigravityFilter(AntigravityFilterOff, true, nil)
	if filter != nil {
		t.Errorf("expected nil filter in off mode, got non-nil")
	}
}

// TestAntigravityFilter_DisabledKeywords verifies that disabling the default
// preset and providing no custom mappings makes the filter a no-op even in
// block mode.
func TestAntigravityFilter_DisabledKeywords(t *testing.T) {
	filter := NewAntigravityFilter(AntigravityFilterBlock, false, nil)
	body := []byte(`{"system":"You are Cursor."}`)
	_, decision := filter.Apply(body)
	if decision.Blocked {
		t.Errorf("with both default and custom mappings disabled, no request should be blocked")
	}
}

// TestAntigravityFilter_CustomMappings verifies that custom mappings are
// matched in addition to the default set when the default is enabled.
func TestAntigravityFilter_CustomMappings(t *testing.T) {
	custom := []AntigravityMapping{
		{Match: "AcmeEditor", Replacement: "Antigravity"},
	}
	filter := NewAntigravityFilter(AntigravityFilterBlock, true, custom)
	body := []byte(`{"system":"You run inside AcmeEditor."}`)
	_, decision := filter.Apply(body)
	if !decision.Blocked {
		t.Fatalf("expected custom keyword to trigger block, got %+v", decision)
	}
	if decision.Detail != "acmeeditor" {
		t.Errorf("unexpected detail %q (want \"acmeeditor\")", decision.Detail)
	}
}

// TestAntigravityFilter_CustomMappings_OnlyDefaultDisabled verifies that
// custom mappings work even when the default set is disabled.
func TestAntigravityFilter_CustomMappings_OnlyCustom(t *testing.T) {
	custom := []AntigravityMapping{
		{Match: "AcmeEditor", Replacement: "Antigravity"},
	}
	filter := NewAntigravityFilter(AntigravityFilterBlock, false, custom)

	// A default-keyword request should now pass because defaults are off.
	body := []byte(`{"system":"You run inside Cursor."}`)
	if _, decision := filter.Apply(body); decision.Blocked {
		t.Errorf("default keyword should not block when default preset is off")
	}

	// A custom keyword should still block.
	body = []byte(`{"system":"You run inside AcmeEditor."}`)
	if _, decision := filter.Apply(body); !decision.Blocked {
		t.Errorf("custom keyword should block even with defaults off")
	}
}

// TestAntigravityFilter_Rewrite_PreservesStructure verifies that the rewrite
// only touches `system` field text, leaving other fields (model, messages)
// byte-for-byte identical in shape.
func TestAntigravityFilter_Rewrite_PreservesStructure(t *testing.T) {
	filter := NewAntigravityFilter(AntigravityFilterRewrite, true, nil)
	original := map[string]any{
		"model":  "claude-3-5-sonnet",
		"system": "Cursor is your host.",
		"contents": []any{
			map[string]any{"role": "user", "parts": []any{map[string]any{"text": "I use Cline daily"}}},
		},
		"systemInstruction": map[string]any{"parts": []any{map[string]any{"text": "Cursor"}}},
	}
	raw, _ := json.Marshal(original)
	rewritten, decision := filter.Apply(raw)
	if decision.Blocked {
		t.Fatalf("unexpected block in rewrite mode: %+v", decision)
	}
	if rewritten == nil {
		t.Fatal("expected rewritten body")
	}
	var got map[string]any
	if err := json.Unmarshal(rewritten, &got); err != nil {
		t.Fatalf("rewritten body is not valid JSON: %v", err)
	}

	// system field must have been rewritten.
	system, _ := got["system"].(string)
	if strings.Contains(system, "Cursor") {
		t.Errorf("system field not rewritten: %q", system)
	}

	// user prompt text mentioning "Cline" must survive untouched.
	contents, _ := got["contents"].([]any)
	if len(contents) == 0 {
		t.Fatal("contents array dropped from rewritten body")
	}
	first, _ := contents[0].(map[string]any)
	parts, _ := first["parts"].([]any)
	if len(parts) == 0 {
		t.Fatal("parts array dropped from rewritten body")
	}
	firstPart, _ := parts[0].(map[string]any)
	text, _ := firstPart["text"].(string)
	if !strings.Contains(text, "Cline") {
		t.Errorf("user prompt was modified by rewrite: %q", text)
	}

	// systemInstruction.parts[0].text inside systemInstruction (a key other
	// than "system") must NOT be rewritten, confirming only `system` is touched.
	si, _ := got["systemInstruction"].(map[string]any)
	siParts, _ := si["parts"].([]any)
	if len(siParts) == 0 {
		t.Fatal("systemInstruction.parts dropped from rewritten body")
	}
	siFirst, _ := siParts[0].(map[string]any)
	siText, _ := siFirst["text"].(string)
	if !strings.Contains(siText, "Cursor") {
		t.Errorf("non-system fields must not be rewritten, got %q", siText)
	}
}

// TestAntigravityFilter_SystemArray verifies that the filter handles `system`
// being a list of strings (Anthropic / OpenAI Responses formats allow this).
func TestAntigravityFilter_SystemArray(t *testing.T) {
	filter := NewAntigravityFilter(AntigravityFilterBlock, true, nil)
	body := []byte(`{
		"system": [
			{"type": "text", "text": "You are a Cursor assistant."},
			{"type": "text", "text": "Be concise."}
		]
	}`)
	_, decision := filter.Apply(body)
	if !decision.Blocked {
		t.Fatalf("expected block when keyword is inside system array element, got %+v", decision)
	}
}

// TestAntigravityFilter_MalformedBody is treated as a no-op: the upstream
// will produce its own error, the filter should not invent one.
func TestAntigravityFilter_MalformedBody(t *testing.T) {
	filter := NewAntigravityFilter(AntigravityFilterBlock, true, nil)
	_, decision := filter.Apply([]byte(`{not json`))
	if decision.Blocked {
		t.Errorf("malformed body must not produce a block decision")
	}
}

// TestAntigravityFilter_CaseInsensitive verifies that matching is
// case-insensitive for both block and rewrite modes.
func TestAntigravityFilter_CaseInsensitive(t *testing.T) {
	filter := NewAntigravityFilter(AntigravityFilterBlock, true, nil)
	cases := []string{"cursor", "CURSOR", "Cursor", "CuRsOr"}
	for _, c := range cases {
		body := []byte(`{"system":"You run inside ` + c + `."}`)
		_, decision := filter.Apply(body)
		if !decision.Blocked {
			t.Errorf("expected case-insensitive match for %q to block", c)
		}
	}
}

// TestParseAntigravityCustomMappings covers the supported input shapes.
func TestParseAntigravityCustomMappings(t *testing.T) {
	t.Run("nil", func(t *testing.T) {
		out, err := ParseAntigravityCustomMappings(nil)
		if err != nil {
			t.Fatalf("nil should not error: %v", err)
		}
		if len(out) != 0 {
			t.Errorf("nil should produce zero mappings, got %d", len(out))
		}
	})

	t.Run("string form", func(t *testing.T) {
		out, err := ParseAntigravityCustomMappings("Cursor: Antigravity, Windsurf: Antigravity")
		if err != nil {
			t.Fatalf("string parse error: %v", err)
		}
		if len(out) != 2 {
			t.Fatalf("expected 2 mappings, got %d", len(out))
		}
	})

	t.Run("string form with newlines", func(t *testing.T) {
		out, err := ParseAntigravityCustomMappings("Cursor: Antigravity\nWindsurf: Antigravity")
		if err != nil {
			t.Fatalf("newline string parse error: %v", err)
		}
		if len(out) != 2 {
			t.Fatalf("expected 2 mappings, got %d", len(out))
		}
	})

	t.Run("map[string]string form", func(t *testing.T) {
		out, err := ParseAntigravityCustomMappings(map[string]string{
			"Cursor":   "Antigravity",
			"Windsurf": "Antigravity",
		})
		if err != nil {
			t.Fatalf("map parse error: %v", err)
		}
		if len(out) != 2 {
			t.Fatalf("expected 2 mappings, got %d", len(out))
		}
	})

	t.Run("map[string]any form", func(t *testing.T) {
		out, err := ParseAntigravityCustomMappings(map[string]any{
			"Cursor":   "Antigravity",
			"Windsurf": "Antigravity",
		})
		if err != nil {
			t.Fatalf("map parse error: %v", err)
		}
		if len(out) != 2 {
			t.Fatalf("expected 2 mappings, got %d", len(out))
		}
	})

	t.Run("malformed string entry", func(t *testing.T) {
		_, err := ParseAntigravityCustomMappings("Cursor Antigravity")
		if err == nil {
			t.Errorf("expected error on entry without ':'")
		}
	})

	t.Run("non-string map value", func(t *testing.T) {
		_, err := ParseAntigravityCustomMappings(map[string]any{"Cursor": 42})
		if err == nil {
			t.Errorf("expected error when map value is not a string")
		}
	})
}

// TestNormalizeAntigravityMappings_Deduplicates confirms that the same match
// string supplied twice collapses to a single entry, so custom mappings can
// overlap the defaults without doubling work.
func TestNormalizeAntigravityMappings_Deduplicates(t *testing.T) {
	out := normalizeAntigravityMappings([]AntigravityMapping{
		{Match: "Cursor", Replacement: "Antigravity"},
		{Match: "cursor", Replacement: "Antigravity"},
		{Match: "  Cursor  ", Replacement: "Antigravity"},
	})
	if len(out) != 1 {
		t.Errorf("expected 1 mapping after dedup, got %d (%+v)", len(out), out)
	}
}

// TestNormalizeAntigravityMappings_LongestFirst confirms longer matches
// appear before shorter ones, so "GitHub Copilot CLI" is tried before "Codex"
// and the classifier returns the most specific hit.
func TestNormalizeAntigravityMappings_LongestFirst(t *testing.T) {
	out := normalizeAntigravityMappings([]AntigravityMapping{
		{Match: "Codex", Replacement: "Antigravity"},
		{Match: "GitHub Copilot CLI", Replacement: "Antigravity"},
		{Match: "GitHub Copilot", Replacement: "Antigravity"},
	})
	if len(out) != 3 {
		t.Fatalf("expected 3 mappings, got %d", len(out))
	}
	if out[0].Match != "github copilot cli" {
		t.Errorf("expected longest match first, got %q", out[0].Match)
	}
	if out[1].Match != "github copilot" {
		t.Errorf("expected second-longest next, got %q", out[1].Match)
	}
	if out[2].Match != "codex" {
		t.Errorf("expected shortest last, got %q", out[2].Match)
	}
}
