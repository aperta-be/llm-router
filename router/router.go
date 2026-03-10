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

	"github.com/aperta-be/llm-router/store"
)

type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ChatRequest struct {
	Model       string            `json:"model"`
	Messages    []ChatMessage     `json:"messages"`
	Stream      bool              `json:"stream"`
	Temperature *float64          `json:"temperature,omitempty"`
	MaxTokens   *int              `json:"max_tokens,omitempty"`
	TopP        *float64          `json:"top_p,omitempty"`
	Stop        json.RawMessage   `json:"stop,omitempty"`
	Tools       json.RawMessage   `json:"tools,omitempty"`
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
		client: &http.Client{Timeout: 5 * time.Minute},
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
	model, _ := SelectModelAndProvider(taskType, cfg)
	return model
}

// FindProviderForModel returns the provider configured for the given model name,
// falling back to the default provider if no exact match is found.
func FindProviderForModel(model string, cfg store.AppConfig) store.Provider {
	switch model {
	case cfg.ThinkingModel:
		return cfg.ThinkingProvider
	case cfg.CodingModel:
		return cfg.CodingProvider
	case cfg.SimpleModel:
		return cfg.SimpleProvider
	default:
		return cfg.DefaultProvider
	}
}

// SelectModelAndProvider picks the model and provider for a given task type.
func SelectModelAndProvider(taskType string, cfg store.AppConfig) (model string, provider store.Provider) {
	switch taskType {
	case "thinking":
		return cfg.ThinkingModel, cfg.ThinkingProvider
	case "coding":
		return cfg.CodingModel, cfg.CodingProvider
	case "simple":
		return cfg.SimpleModel, cfg.SimpleProvider
	default:
		return cfg.DefaultModel, cfg.DefaultProvider
	}
}

// ForwardRequest proxies the request to the given provider, supporting both streaming and non-streaming.
func (r *Router) ForwardRequest(provider store.Provider, model string, origReq ChatRequest, w http.ResponseWriter, streaming bool, requestID string) {
	origReq.Model = model

	if provider.Type == "anthropic" {
		r.forwardAnthropic(provider, origReq, w, streaming, requestID)
		return
	}

	// Ollama or OpenAI-compatible
	body, err := json.Marshal(origReq)
	if err != nil {
		http.Error(w, `{"error":"failed to encode request"}`, http.StatusInternalServerError)
		return
	}

	upstreamReq, err := http.NewRequest(http.MethodPost, provider.BaseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		http.Error(w, `{"error":"failed to build upstream request"}`, http.StatusInternalServerError)
		return
	}
	upstreamReq.Header.Set("Content-Type", "application/json")
	if provider.APIKey != "" {
		upstreamReq.Header.Set("Authorization", "Bearer "+provider.APIKey)
	}
	if requestID != "" {
		upstreamReq.Header.Set("X-Request-ID", requestID)
	}

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

// anthropicRequest is the Anthropic Messages API request body.
type anthropicRequest struct {
	Model     string             `json:"model"`
	System    string             `json:"system,omitempty"`
	Messages  []ChatMessage      `json:"messages"`
	MaxTokens int                `json:"max_tokens"`
	Stream    bool               `json:"stream"`
}

// forwardAnthropic translates an OpenAI-style request to Anthropic and translates the response back.
func (r *Router) forwardAnthropic(provider store.Provider, origReq ChatRequest, w http.ResponseWriter, streaming bool, requestID string) {
	// Separate system message from user/assistant messages.
	var systemPrompt string
	var msgs []ChatMessage
	for _, m := range origReq.Messages {
		if m.Role == "system" {
			systemPrompt = m.Content
		} else {
			msgs = append(msgs, m)
		}
	}

	aReq := anthropicRequest{
		Model:     origReq.Model,
		System:    systemPrompt,
		Messages:  msgs,
		MaxTokens: 8192,
		Stream:    streaming,
	}

	body, err := json.Marshal(aReq)
	if err != nil {
		http.Error(w, `{"error":"failed to encode anthropic request"}`, http.StatusInternalServerError)
		return
	}

	upstreamReq, err := http.NewRequest(http.MethodPost, provider.BaseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		http.Error(w, `{"error":"failed to build anthropic request"}`, http.StatusInternalServerError)
		return
	}
	upstreamReq.Header.Set("Content-Type", "application/json")
	upstreamReq.Header.Set("x-api-key", provider.APIKey)
	upstreamReq.Header.Set("anthropic-version", "2023-06-01")
	if requestID != "" {
		upstreamReq.Header.Set("X-Request-ID", requestID)
	}

	resp, err := r.client.Do(upstreamReq)
	if err != nil {
		http.Error(w, `{"error":"anthropic upstream request failed"}`, http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if streaming {
		r.streamAnthropic(resp, w)
	} else {
		r.translateAnthropicResponse(resp, w)
	}
}

// translateAnthropicResponse converts a non-streaming Anthropic response to OpenAI format.
func (r *Router) translateAnthropicResponse(resp *http.Response, w http.ResponseWriter) {
	var aResp struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		Model string `json:"model"`
	}
	rawBody, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(rawBody, &aResp); err != nil || len(aResp.Content) == 0 {
		// Pass through as-is on error
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		w.Write(rawBody)
		return
	}

	oaiResp := map[string]any{
		"choices": []map[string]any{
			{
				"message": map[string]string{
					"role":    "assistant",
					"content": aResp.Content[0].Text,
				},
				"finish_reason": "stop",
				"index":         0,
			},
		},
		"model":  aResp.Model,
		"object": "chat.completion",
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(oaiResp)
}

// streamAnthropic converts Anthropic SSE events to OpenAI SSE format.
func (r *Router) streamAnthropic(resp *http.Response, w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	flusher, canFlush := w.(http.Flusher)

	scanner := newLineScanner(resp.Body)
	var eventType string
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "event:") {
			eventType = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))

		switch eventType {
		case "content_block_delta":
			var delta struct {
				Delta struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"delta"`
			}
			if err := json.Unmarshal([]byte(data), &delta); err != nil || delta.Delta.Type != "text_delta" {
				continue
			}
			chunk := map[string]any{
				"choices": []map[string]any{
					{"delta": map[string]string{"content": delta.Delta.Text}},
				},
			}
			chunkJSON, _ := json.Marshal(chunk)
			fmt.Fprintf(w, "data: %s\n\n", chunkJSON)
			if canFlush {
				flusher.Flush()
			}
		case "message_stop":
			fmt.Fprintf(w, "data: [DONE]\n\n")
			if canFlush {
				flusher.Flush()
			}
		}
	}
	if scanner.Err() != nil {
		log.Printf("anthropic stream scan error: %v", scanner.Err())
	}
}

// newLineScanner returns a bufio.Scanner configured for line scanning.
func newLineScanner(r io.Reader) *bufioScanner {
	return &bufioScanner{r: r, buf: make([]byte, 0, 4096)}
}

type bufioScanner struct {
	r   io.Reader
	buf []byte
	cur string
	err error
}

func (s *bufioScanner) Scan() bool {
	for {
		// Look for \n in buf
		for i, b := range s.buf {
			if b == '\n' {
				s.cur = string(s.buf[:i])
				s.buf = s.buf[i+1:]
				return true
			}
		}
		// Read more data
		tmp := make([]byte, 4096)
		n, err := s.r.Read(tmp)
		if n > 0 {
			s.buf = append(s.buf, tmp[:n]...)
		}
		if err != nil {
			if len(s.buf) > 0 {
				s.cur = string(s.buf)
				s.buf = nil
				return true
			}
			s.err = err
			return false
		}
	}
}

func (s *bufioScanner) Text() string { return s.cur }
func (s *bufioScanner) Err() error {
	if s.err == io.EOF {
		return nil
	}
	return s.err
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
