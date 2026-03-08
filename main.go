package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

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
		authed.GET("/test-connection", admin.TestConnection)
		authed.GET("/keys", admin.KeysPage)
		authed.POST("/keys", admin.KeyCreate)
		authed.POST("/keys/:id/revoke", admin.KeyRevoke)
		authed.GET("/prompts", admin.PromptsPage)
		authed.GET("/prompts/export", admin.PromptsExport)
	}

	srv := &http.Server{
		Addr:    ":" + cfg.Port,
		Handler: r,
	}

	go func() {
		log.Printf("llm-router starting on :%s (ollama: %s, db: %s)", cfg.Port, cfg.OllamaBaseURL, cfg.DBPath)
		log.Printf("admin panel: http://localhost:%s/admin", cfg.Port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server error: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("shutting down server...")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Fatalf("forced shutdown: %v", err)
	}
	log.Println("server stopped")
}
