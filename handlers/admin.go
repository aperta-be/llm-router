package handlers

import (
	"embed"
	"html/template"
	"log"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"llm-router/store"
)

//go:embed templates/*.html
var templateFS embed.FS

const sessionCookie = "llmr_session"
const perPage = 25

type AdminHandler struct {
	store *store.Store
}

func NewAdmin(s *store.Store) *AdminHandler {
	return &AdminHandler{store: s}
}

// render executes base.html + pageName template.
func (h *AdminHandler) render(c *gin.Context, pageName string, data any) {
	funcMap := template.FuncMap{
		"sub":   func(a, b int) int { return a - b },
		"add":   func(a, b int) int { return a + b },
		"percent": func(hits, total int64) int64 {
			if total == 0 { return 0 }
			return hits * 100 / total
		},
		"pages": func(n int) []int {
			s := make([]int, n)
			for i := range s {
				s[i] = i
			}
			return s
		},
	}
	tmpl, err := template.New("").Funcs(funcMap).ParseFS(templateFS, "templates/base.html", "templates/"+pageName)
	if err != nil {
		log.Printf("template parse error: %v", err)
		c.String(http.StatusInternalServerError, "template error: %v", err)
		return
	}
	c.Header("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.ExecuteTemplate(c.Writer, "base", data); err != nil {
		log.Printf("template execute error: %v", err)
	}
}

// --- Login / Logout ---

func (h *AdminHandler) LoginPage(c *gin.Context) {
	tmpl, _ := template.ParseFS(templateFS, "templates/login.html")
	c.Header("Content-Type", "text/html; charset=utf-8")
	tmpl.Execute(c.Writer, gin.H{"Error": ""})
}

func (h *AdminHandler) LoginSubmit(c *gin.Context) {
	username := c.PostForm("username")
	password := c.PostForm("password")

	ok, err := h.store.AuthenticateUser(username, password)
	if err != nil || !ok {
		tmpl, _ := template.ParseFS(templateFS, "templates/login.html")
		c.Header("Content-Type", "text/html; charset=utf-8")
		c.Status(http.StatusUnauthorized)
		tmpl.Execute(c.Writer, gin.H{"Error": "Invalid username or password."})
		return
	}

	token, err := h.store.CreateSession()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "session error"})
		return
	}
	c.SetCookie(sessionCookie, token, 86400, "/", "", false, true)
	c.Redirect(http.StatusFound, "/admin/dashboard")
}

func (h *AdminHandler) Logout(c *gin.Context) {
	if token, err := c.Cookie(sessionCookie); err == nil {
		h.store.DeleteSession(token)
	}
	c.SetCookie(sessionCookie, "", -1, "/", "", false, true)
	c.Redirect(http.StatusFound, "/admin/login")
}

// --- Dashboard ---

type dashboardData struct {
	Page   string
	Stats  store.Stats
	Recent []store.RequestRecord
}

func (h *AdminHandler) Dashboard(c *gin.Context) {
	stats, err := h.store.GetStats()
	if err != nil {
		log.Printf("get stats: %v", err)
	}
	recent, err := h.store.ListRequests(10, 0)
	if err != nil {
		log.Printf("list recent: %v", err)
	}
	h.render(c, "dashboard.html", dashboardData{Page: "dashboard", Stats: stats, Recent: recent})
}

// --- Config ---

type configData struct {
	Page  string
	Cfg   store.AppConfig
	Saved bool
}

func (h *AdminHandler) ConfigPage(c *gin.Context) {
	cfg, err := h.store.GetConfig()
	if err != nil {
		log.Printf("get config: %v", err)
	}
	h.render(c, "config.html", configData{Page: "config", Cfg: cfg})
}

func (h *AdminHandler) ConfigSave(c *gin.Context) {
	fields := map[string]string{
		"ollama_base_url":  c.PostForm("ollama_base_url"),
		"classifier_model": c.PostForm("classifier_model"),
		"thinking_model":   c.PostForm("thinking_model"),
		"coding_model":     c.PostForm("coding_model"),
		"simple_model":     c.PostForm("simple_model"),
		"default_model":         c.PostForm("default_model"),
		"classification_prompt":  c.PostForm("classification_prompt"),
		"classifier_timeout_s":  c.PostForm("classifier_timeout_s"),
		"cache_ttl_s":           c.PostForm("cache_ttl_s"),
		"cache_max_size":        c.PostForm("cache_max_size"),
	}
	for k, v := range fields {
		
		if v == "" {
			continue
		}
		if err := h.store.SetConfigValue(k, v); err != nil {
			log.Printf("set config %s: %v", k, err)
		}
	}

	cfg, _ := h.store.GetConfig()
	h.render(c, "config.html", configData{Page: "config", Cfg: cfg, Saved: true})
}

// --- API Keys ---

type keysData struct {
	Page   string
	Keys   []store.APIKey
	NewKey string
	Error  string
}

func (h *AdminHandler) KeysPage(c *gin.Context) {
	keys, err := h.store.ListAPIKeys()
	if err != nil {
		log.Printf("list keys: %v", err)
	}
	h.render(c, "keys.html", keysData{
		Page:   "keys",
		Keys:   keys,
		NewKey: c.Query("new_key"),
	})
}

func (h *AdminHandler) KeyCreate(c *gin.Context) {
	name := c.PostForm("name")
	if name == "" {
		keys, _ := h.store.ListAPIKeys()
		h.render(c, "keys.html", keysData{Page: "keys", Keys: keys, Error: "Key name is required."})
		return
	}

	rawKey, err := h.store.CreateAPIKey(name)
	if err != nil {
		log.Printf("create key: %v", err)
		keys, _ := h.store.ListAPIKeys()
		h.render(c, "keys.html", keysData{Page: "keys", Keys: keys, Error: "Failed to create key."})
		return
	}

	c.Redirect(http.StatusFound, "/admin/keys?new_key="+rawKey)
}

func (h *AdminHandler) KeyRevoke(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		c.Redirect(http.StatusFound, "/admin/keys")
		return
	}
	if err := h.store.RevokeAPIKey(id); err != nil {
		log.Printf("revoke key %d: %v", id, err)
	}
	c.Redirect(http.StatusFound, "/admin/keys")
}

// --- Prompts ---

type promptsData struct {
	Page     string
	Requests []store.RequestRecord
	Total    int64
	Pages    int
	PageNum  int
}

func (h *AdminHandler) PromptsPage(c *gin.Context) {
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	if page < 1 {
		page = 1
	}
	offset := (page - 1) * perPage

	requests, err := h.store.ListRequests(perPage, offset)
	if err != nil {
		log.Printf("list requests: %v", err)
	}
	total, _ := h.store.CountRequests()
	pages := int(total) / perPage
	if int(total)%perPage > 0 {
		pages++
	}

	h.render(c, "prompts.html", promptsData{
		Page:     "prompts",
		Requests: requests,
		Total:    total,
		Pages:    pages,
		PageNum:  page,
	})
}
