package handlers

import (
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/aperta-be/llm-router/store"
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
	ip := c.ClientIP()
	lockKey := "user:" + username

	// Check lockout by username
	if locked, _ := h.store.IsLoginLocked(lockKey); locked {
		tmpl, _ := template.ParseFS(templateFS, "templates/login.html")
		c.Header("Content-Type", "text/html; charset=utf-8")
		c.Status(http.StatusTooManyRequests)
		tmpl.Execute(c.Writer, gin.H{"Error": "Too many failed attempts. Try again in 15 minutes."})
		return
	}
	// Also check by IP
	ipKey := "ip:" + ip
	if locked, _ := h.store.IsLoginLocked(ipKey); locked {
		tmpl, _ := template.ParseFS(templateFS, "templates/login.html")
		c.Header("Content-Type", "text/html; charset=utf-8")
		c.Status(http.StatusTooManyRequests)
		tmpl.Execute(c.Writer, gin.H{"Error": "Too many failed attempts. Try again in 15 minutes."})
		return
	}

	userID, role, ok, err := h.store.AuthenticateUser(username, password)
	if err != nil || !ok {
		h.store.RecordLoginFailure(lockKey)
		h.store.RecordLoginFailure(ipKey)
		tmpl, _ := template.ParseFS(templateFS, "templates/login.html")
		c.Header("Content-Type", "text/html; charset=utf-8")
		c.Status(http.StatusUnauthorized)
		tmpl.Execute(c.Writer, gin.H{"Error": "Invalid username or password."})
		return
	}

	h.store.ClearLoginFailures(lockKey)
	h.store.ClearLoginFailures(ipKey)

	token, err := h.store.CreateSession(userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "session error"})
		return
	}
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		MaxAge:   86400,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	if role == "admin" {
		c.Redirect(http.StatusFound, "/admin/dashboard")
	} else {
		c.Redirect(http.StatusFound, "/admin/keys")
	}
}

func (h *AdminHandler) Logout(c *gin.Context) {
	if token, err := c.Cookie(sessionCookie); err == nil {
		h.store.DeleteSession(token)
	}
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		MaxAge:   -1,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	c.Redirect(http.StatusFound, "/admin/login")
}

// --- Dashboard ---

type periodOption struct {
	Value string
	Label string
}

type dashboardData struct {
	Page    string
	Role    string
	Stats   store.Stats
	Recent  []store.RequestRecord
	Period  string
	Periods []periodOption
}

var periodOptions = []periodOption{
	{"1h", "1h"},
	{"24h", "24h"},
	{"7d", "7d"},
	{"30d", "30d"},
	{"all", "All time"},
}

func (h *AdminHandler) Dashboard(c *gin.Context) {
	period := c.DefaultQuery("period", "all")
	since := periodToTime(period)

	stats, err := h.store.GetStats(since)
	if err != nil {
		log.Printf("get stats: %v", err)
	}
	recent, err := h.store.ListRequests(10, 0)
	if err != nil {
		log.Printf("list recent: %v", err)
	}
	h.render(c, "dashboard.html", dashboardData{
		Page:    "dashboard",
		Role:    c.GetString("user_role"),
		Stats:   stats,
		Recent:  recent,
		Period:  period,
		Periods: periodOptions,
	})
}

func periodToTime(period string) time.Time {
	now := time.Now()
	switch period {
	case "1h":
		return now.Add(-time.Hour)
	case "24h":
		return now.Add(-24 * time.Hour)
	case "7d":
		return now.Add(-7 * 24 * time.Hour)
	case "30d":
		return now.Add(-30 * 24 * time.Hour)
	default:
		return time.Time{} // zero time = no filter
	}
}

// --- Config ---

type configData struct {
	Page   string
	Role   string
	Cfg    store.AppConfig
	Saved  bool
	Errors []string
}

func (h *AdminHandler) ConfigPage(c *gin.Context) {
	cfg, err := h.store.GetConfig()
	if err != nil {
		log.Printf("get config: %v", err)
	}
	h.render(c, "config.html", configData{Page: "config", Role: c.GetString("user_role"), Cfg: cfg})
}

func (h *AdminHandler) ConfigSave(c *gin.Context) {
	fields := map[string]string{
		"ollama_base_url":        c.PostForm("ollama_base_url"),
		"classifier_model":       c.PostForm("classifier_model"),
		"thinking_model":         c.PostForm("thinking_model"),
		"coding_model":           c.PostForm("coding_model"),
		"simple_model":           c.PostForm("simple_model"),
		"default_model":          c.PostForm("default_model"),
		"classification_prompt":  c.PostForm("classification_prompt"),
		"classifier_timeout_s":   c.PostForm("classifier_timeout_s"),
		"cache_ttl_s":            c.PostForm("cache_ttl_s"),
		"cache_max_size":         c.PostForm("cache_max_size"),
	}

	var errs []string

	// Validate URL format for ollama_base_url
	if v := fields["ollama_base_url"]; v != "" {
		if u, err := url.Parse(v); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			errs = append(errs, "Ollama Base URL must be a valid http(s) URL")
		}
	}

	// Validate model names are non-empty
	for _, key := range []string{"classifier_model", "thinking_model", "coding_model", "simple_model", "default_model"} {
		if fields[key] == "" {
			errs = append(errs, fmt.Sprintf("%s cannot be empty", key))
		}
	}

	// Validate positive integers for numeric fields
	for _, key := range []string{"classifier_timeout_s", "cache_ttl_s", "cache_max_size"} {
		if v := fields[key]; v != "" {
			if n, err := strconv.Atoi(v); err != nil || n <= 0 {
				errs = append(errs, fmt.Sprintf("%s must be a positive integer", key))
			}
		}
	}

	role := c.GetString("user_role")
	if len(errs) > 0 {
		cfg, _ := h.store.GetConfig()
		h.render(c, "config.html", configData{Page: "config", Role: role, Cfg: cfg, Errors: errs})
		return
	}

	for k, v := range fields {
		if v == "" {
			continue
		}
		if err := h.store.SetConfigValue(k, v); err != nil {
			log.Printf("set config %s: %v", k, err)
			errs = append(errs, fmt.Sprintf("Failed to save %s: %v", k, err))
		}
	}

	cfg, _ := h.store.GetConfig()
	if len(errs) > 0 {
		h.render(c, "config.html", configData{Page: "config", Role: role, Cfg: cfg, Errors: errs})
		return
	}
	h.render(c, "config.html", configData{Page: "config", Role: role, Cfg: cfg, Saved: true})
}

// --- API Keys ---

type keysData struct {
	Page   string
	Role   string
	Keys   []store.APIKey
	NewKey string
	Error  string
}

type usersData struct {
	Page  string
	Role  string
	Users []store.User
	Error string
	Saved bool
}

func (h *AdminHandler) KeysPage(c *gin.Context) {
	role := c.GetString("user_role")
	uid, _ := c.Get("user_id")
	userID, _ := uid.(int64)

	var keys []store.APIKey
	var err error
	if role == "admin" {
		keys, err = h.store.ListAPIKeys()
	} else {
		keys, err = h.store.ListAPIKeysByUser(userID)
	}
	if err != nil {
		log.Printf("list keys: %v", err)
	}
	h.render(c, "keys.html", keysData{
		Page:   "keys",
		Role:   role,
		Keys:   keys,
		NewKey: c.Query("new_key"),
	})
}

func (h *AdminHandler) KeyCreate(c *gin.Context) {
	role := c.GetString("user_role")
	uid, _ := c.Get("user_id")
	userID, _ := uid.(int64)

	name := c.PostForm("name")
	if name == "" {
		var keys []store.APIKey
		if role == "admin" {
			keys, _ = h.store.ListAPIKeys()
		} else {
			keys, _ = h.store.ListAPIKeysByUser(userID)
		}
		h.render(c, "keys.html", keysData{Page: "keys", Role: role, Keys: keys, Error: "Key name is required."})
		return
	}

	expiryDays, _ := strconv.Atoi(c.PostForm("expiry_days"))

	rawKey, err := h.store.CreateAPIKeyForUser(name, expiryDays, userID)
	if err != nil {
		log.Printf("create key: %v", err)
		var keys []store.APIKey
		if role == "admin" {
			keys, _ = h.store.ListAPIKeys()
		} else {
			keys, _ = h.store.ListAPIKeysByUser(userID)
		}
		h.render(c, "keys.html", keysData{Page: "keys", Role: role, Keys: keys, Error: "Failed to create key."})
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
	role := c.GetString("user_role")
	uid, _ := c.Get("user_id")
	userID, _ := uid.(int64)

	var revokeUserID int64
	if role != "admin" {
		revokeUserID = userID
	}
	if err := h.store.RevokeAPIKeyForUser(id, revokeUserID); err != nil {
		log.Printf("revoke key %d: %v", id, err)
	}
	c.Redirect(http.StatusFound, "/admin/keys")
}

// --- Prompts ---

type promptsData struct {
	Page           string
	Role           string
	Requests       []store.RequestRecord
	Total          int64
	Pages          int
	PageNum        int
	Search         string
	Classification string
	Model          string
	Models         []string
}

func (h *AdminHandler) PromptsPage(c *gin.Context) {
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	if page < 1 {
		page = 1
	}
	offset := (page - 1) * perPage

	search := c.Query("search")
	if len(search) > 500 {
		search = search[:500]
	}
	f := store.RequestFilter{
		Search:         search,
		Classification: c.Query("classification"),
		Model:          c.Query("model"),
	}

	requests, err := h.store.ListRequestsFiltered(perPage, offset, f)
	if err != nil {
		log.Printf("list requests: %v", err)
	}
	total, _ := h.store.CountRequestsFiltered(f)
	pages := int(total) / perPage
	if int(total)%perPage > 0 {
		pages++
	}
	models, _ := h.store.DistinctModels()

	h.render(c, "prompts.html", promptsData{
		Page:           "prompts",
		Role:           c.GetString("user_role"),
		Requests:       requests,
		Total:          total,
		Pages:          pages,
		PageNum:        page,
		Search:         f.Search,
		Classification: f.Classification,
		Model:          f.Model,
		Models:         models,
	})
}

// --- Export ---

func parseExportTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t
	}
	return time.Time{}
}

func (h *AdminHandler) PromptsExport(c *gin.Context) {
	format := c.DefaultQuery("format", "json")
	f := store.RequestFilter{
		Search:         c.Query("search"),
		Classification: c.Query("classification"),
		Model:          c.Query("model"),
		Since:          parseExportTime(c.Query("from")),
		Until:          parseExportTime(c.Query("to")),
	}

	records, err := h.store.AllRequests(f)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	switch format {
	case "csv":
		c.Header("Content-Disposition", `attachment; filename="requests.csv"`)
		c.Header("Content-Type", "text/csv")
		w := c.Writer
		w.WriteString("id,timestamp,classification,model,prompt,latency_ms,status_code,cache_hit\n")
		for _, r := range records {
			cacheHit := "0"
			if r.CacheHit {
				cacheHit = "1"
			}
			// Escape prompt for CSV (replace " with "")
			prompt := "\"" + strings.ReplaceAll(r.Prompt, "\"", "\"\"") + "\""
			w.WriteString(fmt.Sprintf("%d,%s,%s,%s,%s,%d,%d,%s\n",
				r.ID,
				r.Timestamp.Format("2006-01-02T15:04:05Z"),
				r.Classification,
				r.Model,
				prompt,
				r.LatencyMS,
				r.StatusCode,
				cacheHit,
			))
		}
	default: // json
		c.Header("Content-Disposition", `attachment; filename="requests.json"`)
		c.JSON(http.StatusOK, records)
	}
}

// --- User Management ---

func (h *AdminHandler) UsersPage(c *gin.Context) {
	users, err := h.store.ListUsers()
	if err != nil {
		log.Printf("list users: %v", err)
	}
	h.render(c, "users.html", usersData{
		Page:  "users",
		Role:  c.GetString("user_role"),
		Users: users,
	})
}

func (h *AdminHandler) UserCreate(c *gin.Context) {
	username := c.PostForm("username")
	password := c.PostForm("password")
	role := c.PostForm("role")

	if role != "admin" && role != "user" {
		role = "user"
	}

	if username == "" || password == "" {
		users, _ := h.store.ListUsers()
		h.render(c, "users.html", usersData{
			Page:  "users",
			Role:  c.GetString("user_role"),
			Users: users,
			Error: "Username and password are required.",
		})
		return
	}

	if err := h.store.CreateUser(username, password, role); err != nil {
		log.Printf("create user: %v", err)
		users, _ := h.store.ListUsers()
		h.render(c, "users.html", usersData{
			Page:  "users",
			Role:  c.GetString("user_role"),
			Users: users,
			Error: "Failed to create user (username may already exist).",
		})
		return
	}

	users, _ := h.store.ListUsers()
	h.render(c, "users.html", usersData{
		Page:  "users",
		Role:  c.GetString("user_role"),
		Users: users,
		Saved: true,
	})
}

func (h *AdminHandler) UserToggle(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		c.Redirect(http.StatusFound, "/admin/users")
		return
	}
	user, err := h.store.GetUserByID(id)
	if err != nil {
		log.Printf("get user %d: %v", id, err)
		c.Redirect(http.StatusFound, "/admin/users")
		return
	}
	if err := h.store.SetUserActive(id, !user.Active); err != nil {
		log.Printf("toggle user %d: %v", id, err)
	}
	c.Redirect(http.StatusFound, "/admin/users")
}

// --- Test Connection ---

type connectionResult struct {
	Reachable      bool     `json:"reachable"`
	AvailableModels []string `json:"available_models"`
	ConfiguredModels []configuredModel `json:"configured_models"`
}

type configuredModel struct {
	Role      string `json:"role"`
	Name      string `json:"name"`
	Available bool   `json:"available"`
}

func (h *AdminHandler) TestConnection(c *gin.Context) {
	cfg, err := h.store.GetConfig()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	type tagsResponse struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(cfg.OllamaBaseURL + "/api/tags")
	result := connectionResult{}
	if err != nil || resp.StatusCode != http.StatusOK {
		c.JSON(http.StatusOK, result)
		return
	}
	defer resp.Body.Close()

	var tags tagsResponse
	if err := json.NewDecoder(resp.Body).Decode(&tags); err != nil {
		c.JSON(http.StatusOK, result)
		return
	}

	result.Reachable = true
	for _, m := range tags.Models {
		result.AvailableModels = append(result.AvailableModels, m.Name)
	}

	availSet := make(map[string]bool)
	for _, m := range result.AvailableModels {
		availSet[m] = true
	}

	for _, cm := range []configuredModel{
		{Role: "Classifier", Name: cfg.ClassifierModel},
		{Role: "Thinking", Name: cfg.ThinkingModel},
		{Role: "Coding", Name: cfg.CodingModel},
		{Role: "Simple", Name: cfg.SimpleModel},
		{Role: "Default", Name: cfg.DefaultModel},
	} {
		cm.Available = availSet[cm.Name]
		result.ConfiguredModels = append(result.ConfiguredModels, cm)
	}

	c.JSON(http.StatusOK, result)
}
