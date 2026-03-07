package handlers

import (
	"log"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"llm-router/router"
	"llm-router/store"
)

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

	taskType, cfg, cacheHit, err := h.router.ClassifyTask(req.Messages)
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

	model := router.SelectModel(taskType, cfg)
	log.Printf("classification=%s model=%s stream=%v", taskType, model, req.Stream)

	c.Header("X-Router-Classification", taskType)
	c.Header("X-Router-Model", model)

	h.router.ForwardRequest(cfg.OllamaBaseURL, model, req, c.Writer, req.Stream)

	latency := time.Since(start).Milliseconds()
	go func() {
		if err := h.store.RecordRequest(taskType, model, lastUserMsg, latency, c.Writer.Status(), cacheHit); err != nil {
			log.Printf("record request: %v", err)
		}
	}()
}

func (h *Handler) Health(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
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
