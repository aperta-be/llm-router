package router

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"llm-router/store"
)

type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ChatRequest struct {
	Model    string        `json:"model"`
	Messages []ChatMessage `json:"messages"`
	Stream   bool          `json:"stream"`
}

type chatChoice struct {
	Message ChatMessage `json:"message"`
}

type chatResponse struct {
	Choices []chatChoice `json:"choices"`
}

// classificationCache is a simple TTL cache keyed by message hash.
type classificationCache struct {
	mu      sync.Mutex
	entries map[string]cacheEntry
}

type cacheEntry struct {
	classification string
	expiry         time.Time
}

func newCache() *classificationCache {
	return &classificationCache{entries: make(map[string]cacheEntry)}
}

func (c *classificationCache) get(key string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		return "", false
	}
	if time.Now().After(e.expiry) {
		delete(c.entries, key)
		return "", false
	}
	return e.classification, true
}

func (c *classificationCache) set(key, classification string, ttl time.Duration, maxSize int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= maxSize {
		// Evict all expired entries first
		now := time.Now()
		for k, v := range c.entries {
			if now.After(v.expiry) {
				delete(c.entries, k)
			}
		}
		// If still full, skip caching
		if len(c.entries) >= maxSize {
			return
		}
	}
	c.entries[key] = cacheEntry{classification: classification, expiry: time.Now().Add(ttl)}
}

type Router struct {
	store  *store.Store
	client *http.Client
	cache  *classificationCache
}

func New(s *store.Store) *Router {
	return &Router{
		store:  s,
		client: &http.Client{},
		cache:  newCache(),
	}
}

// ClassifyTask classifies the last user message. Returns task type and current config.
func (r *Router) ClassifyTask(messages []ChatMessage) (taskType string, cfg store.AppConfig, cacheHit bool, err error) {
	cfg, err = r.store.GetConfig()
	if err != nil {
		return "general", cfg, false, fmt.Errorf("get config: %w", err)
	}

	var lastUserMsg string
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			lastUserMsg = messages[i].Content
			break
		}
	}
	if lastUserMsg == "" {
		return "general", cfg, false, nil
	}

	// Check cache
	cacheKey := msgHash(lastUserMsg)
	if cached, ok := r.cache.get(cacheKey); ok {
		log.Printf("classification cache hit: %q → %s", truncate(lastUserMsg, 60), cached)
		return cached, cfg, true, nil
	}

	// Call classifier with timeout
	timeout := time.Duration(cfg.ClassifierTimeoutS) * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	req := ChatRequest{
		Model: cfg.ClassifierModel,
		Messages: []ChatMessage{
			{Role: "system", Content: cfg.ClassificationPrompt},
			{Role: "user", Content: lastUserMsg},
		},
		Stream: false,
	}

	body, err := json.Marshal(req)
	if err != nil {
		return "general", cfg, false, fmt.Errorf("marshal: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.OllamaBaseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "general", cfg, false, fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := r.client.Do(httpReq)
	if err != nil {
		if ctx.Err() != nil {
			log.Printf("classifier timeout after %s, falling back to default model", timeout)
		} else {
			log.Printf("classifier request failed: %v, falling back to default model", err)
		}
		return "general", cfg, false, nil // graceful fallback, no error propagated
	}
	defer resp.Body.Close()

	var cr chatResponse
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		return "general", cfg, false, fmt.Errorf("decode response: %w", err)
	}
	if len(cr.Choices) == 0 {
		return "general", cfg, false, nil
	}

	classification := strings.ToLower(strings.TrimSpace(cr.Choices[0].Message.Content))
	// Grab just the first word in case the model emits extra tokens
	if idx := strings.IndexAny(classification, " \n\r\t.,"); idx > 0 {
		classification = classification[:idx]
	}

	switch classification {
	case "thinking", "coding", "simple", "general":
		r.cache.set(cacheKey, classification, time.Duration(cfg.CacheTTLS)*time.Second, cfg.CacheMaxSize)
		return classification, cfg, false, nil
	default:
		log.Printf("unexpected classification %q, defaulting to general", classification)
		return "general", cfg, false, nil
	}
}

// SelectModel picks the model for a given task type using the provided config.
func SelectModel(taskType string, cfg store.AppConfig) string {
	switch taskType {
	case "thinking":
		return cfg.ThinkingModel
	case "coding":
		return cfg.CodingModel
	case "simple":
		return cfg.SimpleModel
	default:
		return cfg.DefaultModel
	}
}

// ForwardRequest proxies the request to Ollama, supporting both streaming and non-streaming.
func (r *Router) ForwardRequest(ollamaURL, model string, origReq ChatRequest, w http.ResponseWriter, streaming bool) {
	origReq.Model = model

	body, err := json.Marshal(origReq)
	if err != nil {
		http.Error(w, `{"error":"failed to encode request"}`, http.StatusInternalServerError)
		return
	}

	upstreamReq, err := http.NewRequest(http.MethodPost, ollamaURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		http.Error(w, `{"error":"failed to build upstream request"}`, http.StatusInternalServerError)
		return
	}
	upstreamReq.Header.Set("Content-Type", "application/json")

	resp, err := r.client.Do(upstreamReq)
	if err != nil {
		http.Error(w, `{"error":"upstream request failed"}`, http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if streaming {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(resp.StatusCode)

		flusher, ok := w.(http.Flusher)
		buf := make([]byte, 4096)
		for {
			n, readErr := resp.Body.Read(buf)
			if n > 0 {
				w.Write(buf[:n])
				if ok {
					flusher.Flush()
				}
			}
			if readErr == io.EOF {
				break
			}
			if readErr != nil {
				log.Printf("stream read error: %v", readErr)
				break
			}
		}
	} else {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
	}
}

func msgHash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
