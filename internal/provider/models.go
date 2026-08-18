package provider

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"aihub/internal/model"
)

// modelsSeed is the catalog compiled into the binary so a fresh install lists
// models without network access.
//
//go:embed models.json
var modelsSeed []byte

// catalogURL is the upstream catalog, refreshed in the background.
const catalogURL = "https://raw.githubusercontent.com/router-for-me/models/refs/heads/main/models.json"

// ModelInfo describes one model exposed to clients.
type ModelInfo struct {
	ID                  string   `json:"id"`
	Object              string   `json:"object,omitempty"`
	Created             int64    `json:"created,omitempty"`
	OwnedBy             string   `json:"owned_by,omitempty"`
	Type                string   `json:"type,omitempty"`
	DisplayName         string   `json:"display_name,omitempty"`
	Description         string   `json:"description,omitempty"`
	ContextLength       int      `json:"context_length,omitempty"`
	MaxCompletionTokens int      `json:"max_completion_tokens,omitempty"`
	InputModalities     []string `json:"supportedInputModalities,omitempty"`
	OutputModalities    []string `json:"supportedOutputModalities,omitempty"`

	// Provider is filled in by the catalog, not the upstream file.
	Provider model.Provider `json:"provider,omitempty"`
	// Plans lists the Codex plans that may use the model; empty means "any".
	Plans []string `json:"plans,omitempty"`
}

// Catalog resolves model ids to providers and lists the models a connection can
// serve.
type Catalog struct {
	http *http.Client
	log  *slog.Logger

	mu        sync.RWMutex
	byKey     map[string][]ModelInfo // catalog key -> models
	byID      map[string]ModelInfo   // model id -> model
	byProv    map[model.Provider][]ModelInfo
	refreshed time.Time
}

// NewCatalog builds a catalog from the embedded seed.
func NewCatalog(client *http.Client, logger *slog.Logger) *Catalog {
	c := &Catalog{http: client, log: logger}
	if err := c.load(modelsSeed); err != nil {
		// The seed ships with the binary; a failure here is a build problem.
		logger.Error("model catalog seed is invalid", "error", err)
		c.mu.Lock()
		c.byKey = map[string][]ModelInfo{}
		c.byID = map[string]ModelInfo{}
		c.byProv = map[model.Provider][]ModelInfo{}
		c.mu.Unlock()
	}
	return c
}

// codexPlanKeys maps a ChatGPT plan to its catalog key.
var codexPlanKeys = map[string]string{
	"free":       "codex-free",
	"plus":       "codex-plus",
	"pro":        "codex-pro",
	"team":       "codex-team",
	"business":   "codex-team",
	"enterprise": "codex-team",
	"edu":        "codex-team",
}

// load parses a catalog document.
func (c *Catalog) load(raw []byte) error {
	var doc map[string][]ModelInfo
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("decode model catalog: %w", err)
	}

	byKey := map[string][]ModelInfo{}
	byID := map[string]ModelInfo{}
	byProv := map[model.Provider][]ModelInfo{}

	for key, models := range doc {
		provider, plan, ok := catalogKeyProvider(key)
		if !ok {
			continue
		}
		list := make([]ModelInfo, 0, len(models))
		for _, m := range models {
			if m.ID == "" {
				continue
			}
			m.Provider = provider
			if m.Object == "" {
				m.Object = "model"
			}
			list = append(list, m)

			existing, seen := byID[m.ID]
			if !seen {
				if plan != "" {
					m.Plans = []string{plan}
				}
				byID[m.ID] = m
				byProv[provider] = append(byProv[provider], m)
				continue
			}
			if plan != "" && !containsString(existing.Plans, plan) {
				existing.Plans = append(existing.Plans, plan)
				byID[m.ID] = existing
			}
		}
		byKey[key] = list
	}

	for provider, list := range byProv {
		// Re-read the merged plan lists and sort for stable output.
		for i := range list {
			list[i] = byID[list[i].ID]
		}
		sort.Slice(list, func(a, b int) bool { return list[a].ID < list[b].ID })
		byProv[provider] = list
	}

	c.mu.Lock()
	c.byKey = byKey
	c.byID = byID
	c.byProv = byProv
	c.refreshed = time.Now()
	c.mu.Unlock()
	return nil
}

// catalogKeyProvider maps a catalog key to a provider this build supports.
func catalogKeyProvider(key string) (model.Provider, string, bool) {
	if key == "antigravity" {
		return model.ProviderAntigravity, "", true
	}
	if plan, ok := strings.CutPrefix(key, "codex-"); ok {
		return model.ProviderCodex, plan, true
	}
	return "", "", false
}

// Lookup resolves a model id to its provider entry.
func (c *Catalog) Lookup(id string) (ModelInfo, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	info, ok := c.byID[normalizeModelID(id)]
	return info, ok
}

// ProviderFor resolves which provider serves a model id.
func (c *Catalog) ProviderFor(id string) (model.Provider, bool) {
	info, ok := c.Lookup(id)
	if !ok {
		return "", false
	}
	return info.Provider, true
}

// ForProvider lists every model a provider can serve.
func (c *Catalog) ForProvider(provider model.Provider) []ModelInfo {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return append([]ModelInfo(nil), c.byProv[provider]...)
}

// ForPlan lists the models available to a provider account on a given plan.
func (c *Catalog) ForPlan(provider model.Provider, plan string) []ModelInfo {
	if provider != model.ProviderCodex {
		return c.ForProvider(provider)
	}
	key, ok := codexPlanKeys[strings.ToLower(strings.TrimSpace(plan))]
	if !ok {
		// Unknown or missing plan: be permissive rather than hiding models.
		return c.ForProvider(provider)
	}

	c.mu.RLock()
	defer c.mu.RUnlock()
	list := append([]ModelInfo(nil), c.byKey[key]...)
	sort.Slice(list, func(a, b int) bool { return list[a].ID < list[b].ID })
	return list
}

// All lists every model of every supported provider.
func (c *Catalog) All() []ModelInfo {
	var out []ModelInfo
	for _, provider := range model.Providers() {
		out = append(out, c.ForProvider(provider)...)
	}
	return out
}

// RefreshedAt reports when the catalog was last loaded.
func (c *Catalog) RefreshedAt() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.refreshed
}

// Refresh pulls the latest catalog. Failures leave the current catalog intact.
func (c *Catalog) Refresh(ctx context.Context) error {
	reqCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, catalogURL, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("fetch model catalog: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetch model catalog: status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("read model catalog: %w", err)
	}
	return c.load(body)
}

// normalizeModelID strips the decorations clients sometimes add to a model name.
func normalizeModelID(id string) string {
	id = strings.TrimSpace(id)
	// Gemini clients address models as "models/<id>".
	id = strings.TrimPrefix(id, "models/")
	// Some clients suffix a routing hint such as "gpt-5.5:online".
	if idx := strings.IndexByte(id, ':'); idx > 0 {
		id = id[:idx]
	}
	return id
}

func containsString(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}
