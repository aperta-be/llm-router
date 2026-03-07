package main

import (
	"log"

	"github.com/gin-gonic/gin"

	"llm-router/config"
	"llm-router/handlers"
	"llm-router/middleware"
	"llm-router/router"
	"llm-router/store"
)

func main() {
	cfg := config.Load()

	db, err := store.New(cfg.DBPath)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}

	if err := db.SeedDefaults(cfg); err != nil {
		log.Fatalf("seed defaults: %v", err)
	}

	db.CleanupSessions()

	if err := db.SeedUser(cfg.AdminUsername, cfg.AdminPassword); err != nil {
		log.Fatalf("seed admin user: %v", err)
	}

	rt := router.New(db)
	h := handlers.New(rt, db)
	admin := handlers.NewAdmin(db)

	r := gin.New()
	r.Use(middleware.Logger())
	r.Use(gin.Recovery())

	// API routes
	r.GET("/health", h.Health)
	r.GET("/models", h.Models)
	r.POST("/v1/chat/completions", middleware.APIKeyAuth(db), h.Chat)
	r.POST("/v1/classify", middleware.APIKeyAuth(db), h.Classify)

	// Admin: unauthenticated
	r.GET("/admin/login", admin.LoginPage)
	r.POST("/admin/login", admin.LoginSubmit)

	// Admin: authenticated
	authed := r.Group("/admin", middleware.AdminAuth(db))
	{
		authed.GET("/", func(c *gin.Context) {
			c.Redirect(302, "/admin/dashboard")
		})
		authed.GET("/logout", admin.Logout)
		authed.GET("/dashboard", admin.Dashboard)
		authed.GET("/config", admin.ConfigPage)
		authed.POST("/config", admin.ConfigSave)
		authed.GET("/keys", admin.KeysPage)
		authed.POST("/keys", admin.KeyCreate)
		authed.POST("/keys/:id/revoke", admin.KeyRevoke)
		authed.GET("/prompts", admin.PromptsPage)
	}

	log.Printf("llm-router starting on :%s (ollama: %s, db: %s)", cfg.Port, cfg.OllamaBaseURL, cfg.DBPath)
	log.Printf("admin panel: http://localhost:%s/admin", cfg.Port)
	if err := r.Run(":" + cfg.Port); err != nil {
		log.Fatalf("server error: %v", err)
	}
}
