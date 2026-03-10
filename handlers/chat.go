package handlers

import (
	"log"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/aperta-be/llm-router/router"
	"github.com/aperta-be/llm-router/store"
)

// Version is set at build time via -ldflags.
var Version = "dev"

type Handler struct {
	router *router.Router
	store  *store.Store
}

func New(r *router.Router, s *store.Store) *Handler {
	return &Handler{router: r, store: s}
}

func (h *Handler) Chat(c *gin.Context) {
	var req router.ChatRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	var lastUserMsg string
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" {
			lastUserMsg = req.Messages[i].Content
			break
		}
	}

	start := time.Now()

	var taskType string
	var model string
	var provider store.Provider
	var cacheHit bool

	if req.Model != "" && req.Model != "auto" {
		// Client specified a model explicitly — skip routing logic.
		cfg, err := h.store.GetConfig()
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "config unavailable"})
			return
		}
		taskType = "manual"
		model = req.Model
		provider = router.FindProviderForModel(req.Model, cfg)
	} else {
		var cfg store.AppConfig
		var err error
		taskType, cfg, cacheHit, err = h.router.ClassifyTask(req.Messages)
		if err != nil {
			log.Printf("classification error: %v — falling back to default model", err)
			taskType = "general"
			if cfg.DefaultModel == "" {
				if cfg, err = h.store.GetConfig(); err != nil {
					c.JSON(http.StatusInternalServerError, gin.H{"error": "config unavailable"})
					return
				}
			}
		}
		model, provider = router.SelectModelAndProvider(taskType, cfg)
	}
	log.Printf("classification=%s model=%s provider=%s stream=%v", taskType, model, provider.Name, req.Stream)

	c.Header("X-Router-Classification", taskType)
	c.Header("X-Router-Model", model)

	requestID, _ := c.Get("request_id")
	reqID, _ := requestID.(string)
	h.router.ForwardRequest(provider, model, req, c.Writer, req.Stream, reqID)

	latency := time.Since(start).Milliseconds()
	go func() {
		if err := h.store.RecordRequest(taskType, model, lastUserMsg, latency, c.Writer.Status(), cacheHit); err != nil {
			log.Printf("record request: %v", err)
		}
	}()
}

func (h *Handler) Health(c *gin.Context) {
	result := gin.H{
		"status":  "ok",
		"version": Version,
	}

	// Check DB
	if err := h.store.Ping(); err != nil {
		result["status"] = "degraded"
		result["db"] = "unreachable"
	} else {
		result["db"] = "ok"
	}

	// Check Ollama
	cfg, err := h.store.GetConfig()
	if err == nil {
		client := &http.Client{Timeout: 3 * time.Second}
		resp, err := client.Get(cfg.OllamaBaseURL + "/api/tags")
		if err != nil || resp.StatusCode != http.StatusOK {
			result["status"] = "degraded"
			result["ollama"] = "unreachable"
		} else {
			resp.Body.Close()
			result["ollama"] = "ok"
		}
	} else {
		result["status"] = "degraded"
		result["ollama"] = "unknown"
	}

	c.JSON(http.StatusOK, result)
}

func (h *Handler) Models(c *gin.Context) {
	cfg, err := h.store.GetConfig()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get config"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"models": []gin.H{
			{"role": "classifier", "model": cfg.ClassifierModel},
			{"role": "thinking", "model": cfg.ThinkingModel},
			{"role": "coding", "model": cfg.CodingModel},
			{"role": "simple", "model": cfg.SimpleModel},
			{"role": "general (default)", "model": cfg.DefaultModel},
		},
	})
}

// Classify runs only the classification step and returns the result immediately,
// without forwarding to any model. Useful for benchmarking and debugging routing.
func (h *Handler) Classify(c *gin.Context) {
	var req router.ChatRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	start := time.Now()
	taskType, cfg, cacheHit, err := h.router.ClassifyTask(req.Messages)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	model := router.SelectModel(taskType, cfg)
	c.JSON(http.StatusOK, gin.H{
		"classification": taskType,
		"model":          model,
		"cache_hit":      cacheHit,
		"latency_ms":     latency,
	})
}
