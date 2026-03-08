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

	"github.com/aperta-be/llm-router/config"
	"github.com/aperta-be/llm-router/handlers"
	"github.com/aperta-be/llm-router/middleware"
	"github.com/aperta-be/llm-router/router"
	"github.com/aperta-be/llm-router/store"
)

// Version is set at build time via -ldflags "-X main.Version=..."
var Version = "dev"

func main() {
	// Propagate version to handlers
	handlers.Version = Version

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

	// Request body size limit (10 MB)
	r.Use(func(c *gin.Context) {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 10<<20)
		c.Next()
	})

	// API routes
	r.GET("/health", h.Health)
	r.GET("/models", h.Models)
	r.GET("/v1/models", h.Models)
	r.POST("/v1/chat/completions", middleware.APIKeyAuth(db), h.Chat)
	r.POST("/v1/classify", middleware.APIKeyAuth(db), h.Classify)

	// Admin: unauthenticated
	r.GET("/admin/login", admin.LoginPage)
	r.POST("/admin/login", admin.LoginSubmit)

	// Admin: authenticated (any role)
	authed := r.Group("/admin", middleware.UserAuth(db))
	{
		authed.GET("/logout", admin.Logout)
		authed.GET("/keys", admin.KeysPage)
		authed.POST("/keys", admin.KeyCreate)
		authed.POST("/keys/:id/revoke", admin.KeyRevoke)
	}

	// Admin: admin-only routes
	adminOnly := r.Group("/admin", middleware.UserAuth(db), middleware.RequireAdmin())
	{
		adminOnly.GET("/", func(c *gin.Context) {
			c.Redirect(302, "/admin/dashboard")
		})
		adminOnly.GET("/dashboard", admin.Dashboard)
		adminOnly.GET("/config", admin.ConfigPage)
		adminOnly.POST("/config", admin.ConfigSave)
		adminOnly.GET("/test-connection", admin.TestConnection)
		adminOnly.GET("/prompts", admin.PromptsPage)
		adminOnly.GET("/prompts/export", admin.PromptsExport)
		adminOnly.GET("/users", admin.UsersPage)
		adminOnly.POST("/users", admin.UserCreate)
		adminOnly.POST("/users/:id/toggle", admin.UserToggle)
	}

	srv := &http.Server{
		Addr:    ":" + cfg.Port,
		Handler: r,
	}

	// Periodic cleanup goroutine
	cleanupCtx, cleanupCancel := context.WithCancel(context.Background())
	go func() {
		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				db.CleanupSessions()
				db.CleanupLoginAttempts()
			case <-cleanupCtx.Done():
				return
			}
		}
	}()

	go func() {
		log.Printf("llm-router %s starting on :%s (ollama: %s, db: %s)", Version, cfg.Port, cfg.OllamaBaseURL, cfg.DBPath)
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

	cleanupCancel()

	if err := db.Close(); err != nil {
		log.Printf("close db: %v", err)
	}

	log.Println("server stopped")
}
