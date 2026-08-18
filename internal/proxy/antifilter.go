package proxy

// antifilter.go — Antigravity Coding Filter
//
// This module is the in-process port of the upstream plugin
// github.com/jellyfish-p/cpa-plugin-antigravity-coding-filter. That project is
// built as a CGO dynamic library for the CLIProxyAPI host; this binary embeds
// the same logic natively so requests bound for the Antigravity upstream can
// be screened without a plugin runtime.
//
// Two modes are supported, mirroring the plugin:
//
//   - block (default): reject a request whose JSON `system` field mentions a
//     non-Antigravity coding client with HTTP 403.
//   - rewrite: replace matched names with "Antigravity" and forward.
//
// Matching is case-insensitive and only scans JSON fields named `system`.
// Mentions in user prompts, `messages`, or any other field do not trigger the
// filter, exactly like the plugin. The built-in preset is enabled by default
// and covers the same set of coding editors, terminal agents and general
// coding agents. Operators can replace or extend it through environment
// configuration.

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// AntigravityFilterMode is the action taken when a request matches a
// configured keyword.
type AntigravityFilterMode string

const (
	// AntigravityFilterOff disables the filter entirely; requests pass through.
	AntigravityFilterOff AntigravityFilterMode = "off"
	// AntigravityFilterBlock rejects matching requests with HTTP 403.
	AntigravityFilterBlock AntigravityFilterMode = "block"
	// AntigravityFilterRewrite replaces matching names with "Antigravity" and
	// forwards the rewritten request.
	AntigravityFilterRewrite AntigravityFilterMode = "rewrite"
)

// antigravityReplacement is the canonical replacement used by the default
// mapping table. Keeping it a single literal makes it obvious in code review
// what every keyword becomes after rewrite.
const antigravityReplacement = "Antigravity"

// antigravityFilterConfig is the resolved runtime configuration for the
// filter. It is immutable once built; the filter swaps the pointer it holds
// under a mutex when configuration changes (today only at startup, but the
// indirection keeps a future hot-reload path open).
type antigravityFilterConfig struct {
	Mode               AntigravityFilterMode
	UseDefaultKeywords bool
	CustomMappings     []AntigravityMapping
}

// AntigravityMapping is one match → replacement pair. The Match field is
// lower-cased and trimmed before matching; Replacement is the original case.
type AntigravityMapping struct {
	Match       string
	Replacement string
}

// AntigravityDecision reports what the filter concluded about a request.
type AntigravityDecision struct {
	// Blocked is true when the request should be rejected under block mode.
	Blocked bool
	// Signal is a short classification of why the decision was made.
	// Today it is always "system.keyword" when Blocked is true.
	Signal string
	// Detail is the matched keyword, useful for operator logs.
	Detail string
}

// AntigravityFilter scans request bodies for non-Antigravity coding-client
// names inside the JSON `system` field and either blocks or rewrites them.
type AntigravityFilter struct {
	mu     sync.RWMutex
	config antigravityFilterConfig
}

// NewAntigravityFilter constructs a filter from the supplied configuration.
// Returning a nil filter when the mode is "off" lets the Router skip the
// hot path entirely.
func NewAntigravityFilter(mode AntigravityFilterMode, useDefault bool, customMappings []AntigravityMapping) *AntigravityFilter {
	if mode == AntigravityFilterOff {
		return nil
	}
	return &AntigravityFilter{
		config: antigravityFilterConfig{
			Mode:               mode,
			UseDefaultKeywords: useDefault,
			CustomMappings:     normalizeAntigravityMappings(customMappings),
		},
	}
}

// Enabled reports whether the filter is on. A nil filter is disabled, so the
// Router can guard with `if r.antigravityFilter != nil`.
func (f *AntigravityFilter) Enabled() bool {
	return f != nil && f.config.Mode != AntigravityFilterOff
}

// Mode returns the configured mode. Safe on a nil receiver (returns off).
func (f *AntigravityFilter) Mode() AntigravityFilterMode {
	if f == nil {
		return AntigravityFilterOff
	}
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.config.Mode
}

// Apply inspects body and returns:
//
//   - rewritten: a new body when mode is rewrite and the body was changed,
//     otherwise nil.
//   - decision: the classification; decision.Blocked is set under block mode
//     when a configured keyword was matched.
//
// Parsing failures are treated as "not blocked, not rewritten": a malformed
// body is forwarded unchanged so the upstream can produce its own error. This
// matches the upstream plugin's behaviour.
func (f *AntigravityFilter) Apply(body []byte) (rewritten []byte, decision AntigravityDecision) {
	if !f.Enabled() {
		return nil, AntigravityDecision{}
	}
	f.mu.RLock()
	cfg := f.snapshotConfig()
	f.mu.RUnlock()

	switch cfg.Mode {
	case AntigravityFilterBlock:
		decision = classifyAntigravityRequest(body, cfg)
		return nil, decision
	case AntigravityFilterRewrite:
		rewritten, _ = rewriteAntigravityBody(body, cfg)
		return rewritten, AntigravityDecision{}
	default:
		return nil, AntigravityDecision{}
	}
}

// snapshotConfig copies the immutable config out from under the mutex.
func (f *AntigravityFilter) snapshotConfig() antigravityFilterConfig {
	if f == nil {
		return antigravityFilterConfig{Mode: AntigravityFilterOff}
	}
	f.mu.RLock()
	defer f.mu.RUnlock()
	return antigravityFilterConfig{
		Mode:               f.config.Mode,
		UseDefaultKeywords: f.config.UseDefaultKeywords,
		CustomMappings:     append([]AntigravityMapping(nil), f.config.CustomMappings...),
	}
}

// defaultAntigravityMappings mirrors the built-in preset shipped with the
// upstream plugin. Keeping the list in sync with the plugin is intentional:
// the same clients should be treated the same way by both implementations.
//
// Categories below match the README of cpa-plugin-antigravity-coding-filter.
var defaultAntigravityMappings = []AntigravityMapping{
	// Major AI code editors, assistants, and terminal coding agents.
	{Match: "Claude Code", Replacement: antigravityReplacement},
	{Match: "OpenAI Codex", Replacement: antigravityReplacement},
	{Match: "Codex CLI", Replacement: antigravityReplacement},
	{Match: "Codex", Replacement: antigravityReplacement},
	{Match: "OpenCode", Replacement: antigravityReplacement},
	{Match: "GitHub Copilot CLI", Replacement: antigravityReplacement},
	{Match: "GitHub Copilot", Replacement: antigravityReplacement},
	{Match: "Gemini Code Assist", Replacement: antigravityReplacement},
	{Match: "Gemini CLI", Replacement: antigravityReplacement},
	{Match: "Cursor", Replacement: antigravityReplacement},
	{Match: "Windsurf", Replacement: antigravityReplacement},
	{Match: "Codeium", Replacement: antigravityReplacement},
	{Match: "Cline", Replacement: antigravityReplacement},
	{Match: "Roo Code", Replacement: antigravityReplacement},
	{Match: "Kilo Code", Replacement: antigravityReplacement},
	{Match: "Aider", Replacement: antigravityReplacement},
	{Match: "Continue.dev", Replacement: antigravityReplacement},
	{Match: "Amazon Q Developer", Replacement: antigravityReplacement},
	{Match: "Amazon CodeWhisperer", Replacement: antigravityReplacement},
	{Match: "JetBrains AI Assistant", Replacement: antigravityReplacement},
	{Match: "JetBrains Junie", Replacement: antigravityReplacement},
	{Match: "Kiro", Replacement: antigravityReplacement},
	{Match: "Qoder CLI", Replacement: antigravityReplacement},
	{Match: "Qoder", Replacement: antigravityReplacement},
	{Match: "Qwen Code", Replacement: antigravityReplacement},
	{Match: "Trae", Replacement: antigravityReplacement},
	{Match: "Tabnine", Replacement: antigravityReplacement},
	{Match: "Sourcegraph Cody", Replacement: antigravityReplacement},
	{Match: "Augment Code", Replacement: antigravityReplacement},
	{Match: "Replit Agent", Replacement: antigravityReplacement},
	{Match: "Replit Ghostwriter", Replacement: antigravityReplacement},
	{Match: "Devin", Replacement: antigravityReplacement},
	{Match: "OpenHands", Replacement: antigravityReplacement},
	{Match: "SWE-agent", Replacement: antigravityReplacement},
	{Match: "Goose", Replacement: antigravityReplacement},
	{Match: "Zed AI", Replacement: antigravityReplacement},
	{Match: "Void Editor", Replacement: antigravityReplacement},
	{Match: "PearAI", Replacement: antigravityReplacement},
	{Match: "Refact.ai", Replacement: antigravityReplacement},
	{Match: "Tabby", Replacement: antigravityReplacement},
	{Match: "GitLab Duo", Replacement: antigravityReplacement},
	{Match: "Visual Studio IntelliCode", Replacement: antigravityReplacement},
	{Match: "CodeBuddy", Replacement: antigravityReplacement},
	{Match: "Blackbox AI", Replacement: antigravityReplacement},
	{Match: "Pieces for Developers", Replacement: antigravityReplacement},
	{Match: "Qodo", Replacement: antigravityReplacement},
	{Match: "CodiumAI", Replacement: antigravityReplacement},
	{Match: "Rovo Dev CLI", Replacement: antigravityReplacement},
	{Match: "Factory Droid", Replacement: antigravityReplacement},

	// General-purpose local agents that can generate and modify code.
	{Match: "OpenClaw", Replacement: antigravityReplacement},
	{Match: "Clawdbot", Replacement: antigravityReplacement},
	{Match: "Moltbot", Replacement: antigravityReplacement},
	{Match: "Hermes Agent", Replacement: antigravityReplacement},
	{Match: "Hermes", Replacement: antigravityReplacement},
	{Match: "WorkBuddy", Replacement: antigravityReplacement},
}

// effectiveAntigravityMappings returns the union of the default and custom
// tables (when both are enabled), longest-match-first so multi-word keywords
// win over their shorter substrings (e.g. "GitHub Copilot CLI" before "Codex").
func effectiveAntigravityMappings(cfg antigravityFilterConfig) []AntigravityMapping {
	mappings := make([]AntigravityMapping, 0, len(defaultAntigravityMappings)+len(cfg.CustomMappings))
	if cfg.UseDefaultKeywords {
		mappings = append(mappings, defaultAntigravityMappings...)
	}
	mappings = append(mappings, cfg.CustomMappings...)
	return normalizeAntigravityMappings(mappings)
}

// normalizeAntigravityMappings lower-cases and de-duplicates mappings, then
// sorts them so that longer matches come first. The plugin's behaviour is to
// prefer the longest match, and walking the list in that order lets the
// classify path return on the first hit without disambiguating.
func normalizeAntigravityMappings(mappings []AntigravityMapping) []AntigravityMapping {
	seen := make(map[string]struct{}, len(mappings))
	out := make([]AntigravityMapping, 0, len(mappings))
	for _, item := range mappings {
		match := strings.ToLower(strings.TrimSpace(item.Match))
		replacement := strings.TrimSpace(item.Replacement)
		if match == "" || replacement == "" {
			continue
		}
		if _, exists := seen[match]; exists {
			continue
		}
		seen[match] = struct{}{}
		out = append(out, AntigravityMapping{Match: match, Replacement: replacement})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if len(out[i].Match) == len(out[j].Match) {
			return out[i].Match < out[j].Match
		}
		return len(out[i].Match) > len(out[j].Match)
	})
	return out
}

// classifyAntigravityRequest walks body and reports whether any `system`
// field mentions a configured keyword.
func classifyAntigravityRequest(body []byte, cfg antigravityFilterConfig) AntigravityDecision {
	var root any
	if err := json.Unmarshal(body, &root); err != nil {
		return AntigravityDecision{}
	}
	mappings := effectiveAntigravityMappings(cfg)
	var decision AntigravityDecision
	walkAntigravityJSON(root, func(path []string, value any) bool {
		if len(path) == 0 || path[len(path)-1] != "system" {
			return true
		}
		text := strings.ToLower(collectAntigravityText(value))
		for _, mapping := range mappings {
			if strings.Contains(text, mapping.Match) {
				decision = AntigravityDecision{Blocked: true, Signal: "system.keyword", Detail: mapping.Match}
				return false
			}
		}
		return true
	})
	return decision
}

// rewriteAntigravityBody walks body, replaces keywords in every `system`
// field, and re-encodes the result. The second return is false when nothing
// changed, so the caller can skip swapping the body.
func rewriteAntigravityBody(body []byte, cfg antigravityFilterConfig) ([]byte, bool) {
	var root any
	if err := json.Unmarshal(body, &root); err != nil {
		return nil, false
	}
	rewritten, changed := rewriteAntigravitySystemFields(root, effectiveAntigravityMappings(cfg))
	if !changed {
		return nil, false
	}
	raw, err := json.Marshal(rewritten)
	if err != nil {
		return nil, false
	}
	return raw, true
}

// rewriteAntigravitySystemFields is the recursive worker for body rewriting.
// It descends into every object and array, replacing text inside `system`
// fields only.
func rewriteAntigravitySystemFields(value any, mappings []AntigravityMapping) (any, bool) {
	switch typed := value.(type) {
	case map[string]any:
		changed := false
		for key, child := range typed {
			if key == "system" {
				next, childChanged := rewriteAntigravitySystemValue(child, mappings)
				if childChanged {
					typed[key] = next
					changed = true
				}
				continue
			}
			next, childChanged := rewriteAntigravitySystemFields(child, mappings)
			if childChanged {
				typed[key] = next
				changed = true
			}
		}
		return typed, changed
	case []any:
		changed := false
		for i, child := range typed {
			next, childChanged := rewriteAntigravitySystemFields(child, mappings)
			if childChanged {
				typed[i] = next
				changed = true
			}
		}
		return typed, changed
	default:
		return value, false
	}
}

// rewriteAntigravitySystemValue applies replacements inside a `system` value.
// The system field can be a string, an object with text fields, or an array
// of either — mirroring how the upstream plugin treats the field.
func rewriteAntigravitySystemValue(value any, mappings []AntigravityMapping) (any, bool) {
	switch typed := value.(type) {
	case string:
		next := typed
		changed := false
		for _, mapping := range mappings {
			var replaced bool
			next, replaced = replaceAntigravityInsensitive(next, mapping.Match, mapping.Replacement)
			changed = changed || replaced
		}
		return next, changed
	case map[string]any:
		changed := false
		for key, child := range typed {
			next, childChanged := rewriteAntigravitySystemValue(child, mappings)
			if childChanged {
				typed[key] = next
				changed = true
			}
		}
		return typed, changed
	case []any:
		changed := false
		for i, child := range typed {
			next, childChanged := rewriteAntigravitySystemValue(child, mappings)
			if childChanged {
				typed[i] = next
				changed = true
			}
		}
		return typed, changed
	default:
		return value, false
	}
}

// replaceAntigravityInsensitive is a case-insensitive string replacer that
// reports whether any substitution happened. The standard library's
// strings.ReplaceAll is case-sensitive, and strings.Replacer does not return
// a "changed" flag, so a small helper is cleaner here.
func replaceAntigravityInsensitive(value, match, replacement string) (string, bool) {
	if match == "" {
		return value, false
	}
	lowerValue := strings.ToLower(value)
	lowerMatch := strings.ToLower(match)
	var builder strings.Builder
	start := 0
	changed := false
	for {
		index := strings.Index(lowerValue[start:], lowerMatch)
		if index < 0 {
			break
		}
		index += start
		builder.WriteString(value[start:index])
		builder.WriteString(replacement)
		start = index + len(match)
		changed = true
	}
	if !changed {
		return value, false
	}
	builder.WriteString(value[start:])
	return builder.String(), true
}

// walkAntigravityJSON visits every node in a generic JSON tree. visit returns
// false to stop the walk, which is how the classifier short-circuits on the
// first match.
func walkAntigravityJSON(value any, visit func(path []string, value any) bool) {
	var walk func(path []string, current any) bool
	walk = func(path []string, current any) bool {
		if !visit(path, current) {
			return false
		}
		switch typed := current.(type) {
		case map[string]any:
			for key, child := range typed {
				if !walk(appendAntigravityPath(path, key), child) {
					return false
				}
			}
		case []any:
			for index, child := range typed {
				if !walk(appendAntigravityPath(path, fmt.Sprintf("%d", index)), child) {
					return false
				}
			}
		}
		return true
	}
	walk(nil, value)
}

// appendAntigravityPath returns a new slice with item appended to path. It
// copies the backing array so the caller's slice is not mutated.
func appendAntigravityPath(path []string, item string) []string {
	next := make([]string, len(path), len(path)+1)
	copy(next, path)
	return append(next, item)
}

// collectAntigravityText flattens any string value reachable from value into
// one newline-joined string. It mirrors the plugin's collectText so the
// classifier sees the same text regardless of whether `system` was a plain
// string, a list of strings, or a structured object.
func collectAntigravityText(value any) string {
	var parts []string
	var collect func(any)
	collect = func(current any) {
		switch typed := current.(type) {
		case string:
			parts = append(parts, typed)
		case map[string]any:
			for _, child := range typed {
				collect(child)
			}
		case []any:
			for _, child := range typed {
				collect(child)
			}
		}
	}
	collect(value)
	return strings.Join(parts, "\n")
}

// ParseAntigravityCustomMappings parses the operator-supplied mapping
// expression. Three input shapes are accepted, matching the upstream plugin:
//
//   - nil: no custom mappings.
//   - a string: comma- or newline-delimited "from: to" entries, e.g.
//     "Cursor: Antigravity\nWindsurf: Antigravity".
//   - a map[string]string: explicit pairs.
//
// The parser is intentionally lenient: blank entries and duplicate sources
// are dropped, and any malformed entry produces an error so a typo is loud
// rather than silently ignored.
func ParseAntigravityCustomMappings(value any) ([]AntigravityMapping, error) {
	switch typed := value.(type) {
	case nil:
		return nil, nil
	case string:
		return parseAntigravityMappingString(typed)
	case map[string]string:
		out := make([]AntigravityMapping, 0, len(typed))
		for match, replacement := range typed {
			out = append(out, AntigravityMapping{Match: match, Replacement: replacement})
		}
		return out, nil
	case map[string]any:
		out := make([]AntigravityMapping, 0, len(typed))
		for match, replacement := range typed {
			text, ok := replacement.(string)
			if !ok {
				return nil, fmt.Errorf("custom_mappings values must be strings, got %T", replacement)
			}
			out = append(out, AntigravityMapping{Match: match, Replacement: text})
		}
		return out, nil
	case []string:
		out := make([]AntigravityMapping, 0, len(typed))
		for _, entry := range typed {
			parsed, err := parseAntigravityMappingString(entry)
			if err != nil {
				return nil, err
			}
			out = append(out, parsed...)
		}
		return out, nil
	case []any:
		out := make([]AntigravityMapping, 0, len(typed))
		for _, item := range typed {
			text, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("custom_mappings entries must be strings, got %T", item)
			}
			parsed, err := parseAntigravityMappingString(text)
			if err != nil {
				return nil, err
			}
			out = append(out, parsed...)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("custom_mappings must be a string, a map, or a list; got %T", value)
	}
}

// parseAntigravityMappingString splits a comma- or newline-delimited string of
// "from: to" entries into mappings.
func parseAntigravityMappingString(value string) ([]AntigravityMapping, error) {
	entries := strings.FieldsFunc(value, func(r rune) bool {
		return r == ',' || r == '\n' || r == '\r'
	})
	out := make([]AntigravityMapping, 0, len(entries))
	for _, entry := range entries {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		match, replacement, ok := strings.Cut(entry, ":")
		if !ok {
			return nil, fmt.Errorf("custom_mappings entry %q is missing a ':' separator", entry)
		}
		match = strings.TrimSpace(match)
		replacement = strings.TrimSpace(replacement)
		if match == "" || replacement == "" {
			return nil, fmt.Errorf("custom_mappings entry %q has an empty side", entry)
		}
		out = append(out, AntigravityMapping{Match: match, Replacement: replacement})
	}
	return out, nil
}
