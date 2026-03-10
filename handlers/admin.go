package handlers

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
	"github.com/gin-gonic/gin"
	"golang.org/x/oauth2"

	"github.com/aperta-be/llm-router/config"
	"github.com/aperta-be/llm-router/store"
)

//go:embed templates/*.html
var templateFS embed.FS

const sessionCookie = "llmr_session"
const perPage = 25

type AdminHandler struct {
	store         *store.Store
	cfg           *config.Config
	mu            sync.Mutex
	provider      *gooidc.Provider
	lastIssuerURL string
}

func NewAdmin(s *store.Store, cfg *config.Config) *AdminHandler {
	return &AdminHandler{store: s, cfg: cfg}
}

// getOAuthClients returns a cached (or freshly initialised) OIDC provider and
// oauth2.Config built from the current DB config. Thread-safe.
func (h *AdminHandler) getOAuthClients(dbCfg store.AppConfig) (*gooidc.Provider, *oauth2.Config, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.provider == nil || h.lastIssuerURL != dbCfg.OAuthIssuerURL {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		p, err := gooidc.NewProvider(ctx, dbCfg.OAuthIssuerURL)
		if err != nil {
			return nil, nil, fmt.Errorf("oidc discovery failed for %s: %w", dbCfg.OAuthIssuerURL, err)
		}
		h.provider = p
		h.lastIssuerURL = dbCfg.OAuthIssuerURL
	}

	scopes := strings.Fields(dbCfg.OAuthScopes)
	if len(scopes) == 0 {
		scopes = []string{"openid", "email", "profile"}
	}

	oa2 := &oauth2.Config{
		ClientID:     dbCfg.OAuthClientID,
		ClientSecret: dbCfg.OAuthClientSecret,
		RedirectURL:  dbCfg.OAuthRedirectURL,
		Endpoint:     h.provider.Endpoint(),
		Scopes:       scopes,
	}
	return h.provider, oa2, nil
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
		"maskKey": func(key string) string {
			if key == "" {
				return ""
			}
			if len(key) <= 8 {
				return "••••••••"
			}
			return key[:7] + "…"
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
	dbCfg, _ := h.store.GetConfig()
	tmpl, _ := template.ParseFS(templateFS, "templates/login.html")
	c.Header("Content-Type", "text/html; charset=utf-8")
	tmpl.Execute(c.Writer, gin.H{
		"Error":           c.Query("error"),
		"OAuthEnabled":    dbCfg.OAuthEnabled,
		"PasswordEnabled": !dbCfg.OAuthEnabled || dbCfg.OAuthPasswordFallback,
	})
}

func (h *AdminHandler) LoginSubmit(c *gin.Context) {
	dbCfg, _ := h.store.GetConfig()
	if dbCfg.OAuthEnabled && !dbCfg.OAuthPasswordFallback {
		c.Status(http.StatusNotFound)
		return
	}
	username := c.PostForm("username")
	password := c.PostForm("password")
	ip := c.ClientIP()
	lockKey := "user:" + username

	// Check lockout by username
	if locked, _ := h.store.IsLoginLocked(lockKey); locked {
		tmpl, _ := template.ParseFS(templateFS, "templates/login.html")
		c.Header("Content-Type", "text/html; charset=utf-8")
		c.Status(http.StatusTooManyRequests)
		tmpl.Execute(c.Writer, gin.H{"Error": "Too many failed attempts. Try again in 15 minutes.", "OAuthEnabled": h.cfg.OAuthEnabled})
		return
	}
	// Also check by IP
	ipKey := "ip:" + ip
	if locked, _ := h.store.IsLoginLocked(ipKey); locked {
		tmpl, _ := template.ParseFS(templateFS, "templates/login.html")
		c.Header("Content-Type", "text/html; charset=utf-8")
		c.Status(http.StatusTooManyRequests)
		tmpl.Execute(c.Writer, gin.H{"Error": "Too many failed attempts. Try again in 15 minutes.", "OAuthEnabled": h.cfg.OAuthEnabled})
		return
	}

	userID, role, ok, err := h.store.AuthenticateUser(username, password)
	if err != nil || !ok {
		h.store.RecordLoginFailure(lockKey)
		h.store.RecordLoginFailure(ipKey)
		tmpl, _ := template.ParseFS(templateFS, "templates/login.html")
		c.Header("Content-Type", "text/html; charset=utf-8")
		c.Status(http.StatusUnauthorized)
		tmpl.Execute(c.Writer, gin.H{"Error": "Invalid username or password.", "OAuthEnabled": h.cfg.OAuthEnabled})
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

// --- OAuth2/OIDC ---

func (h *AdminHandler) OAuthLogin(c *gin.Context) {
	dbCfg, err := h.store.GetConfig()
	if err != nil || !dbCfg.OAuthEnabled {
		c.Redirect(http.StatusFound, "/admin/login?error=SSO+not+enabled")
		return
	}

	_, oa2, err := h.getOAuthClients(dbCfg)
	if err != nil {
		log.Printf("oauth init error: %v", err)
		c.Redirect(http.StatusFound, "/admin/login?error=SSO+configuration+error")
		return
	}

	stateBytes := make([]byte, 16)
	if _, err := rand.Read(stateBytes); err != nil {
		c.Redirect(http.StatusFound, "/admin/login?error=Login+failed")
		return
	}
	state := hex.EncodeToString(stateBytes)

	verifierBytes := make([]byte, 64)
	if _, err := rand.Read(verifierBytes); err != nil {
		c.Redirect(http.StatusFound, "/admin/login?error=Login+failed")
		return
	}
	verifier := hex.EncodeToString(verifierBytes)

	cookieAttrs := &http.Cookie{
		MaxAge:   600,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Path:     "/admin/oauth/",
	}
	cookieAttrs.Name = "llmr_oauth_state"
	cookieAttrs.Value = state
	http.SetCookie(c.Writer, cookieAttrs)
	cookieAttrs.Name = "llmr_oauth_pkce"
	cookieAttrs.Value = verifier
	http.SetCookie(c.Writer, cookieAttrs)

	c.Redirect(http.StatusFound, oa2.AuthCodeURL(state, oauth2.S256ChallengeOption(verifier)))
}

func (h *AdminHandler) OAuthCallback(c *gin.Context) {
	loginFail := func(msg string) {
		c.Redirect(http.StatusFound, "/admin/login?error="+url.QueryEscape(msg))
	}

	dbCfg, err := h.store.GetConfig()
	if err != nil || !dbCfg.OAuthEnabled {
		loginFail("SSO not enabled")
		return
	}

	provider, oa2, err := h.getOAuthClients(dbCfg)
	if err != nil {
		log.Printf("oauth init error: %v", err)
		loginFail("Login failed")
		return
	}

	// Validate state
	stateCookie, err := c.Cookie("llmr_oauth_state")
	if err != nil || stateCookie != c.Query("state") {
		loginFail("Login failed")
		return
	}

	// Check for IdP error
	if errParam := c.Query("error"); errParam != "" {
		loginFail("Login cancelled")
		return
	}

	pkceVerifier, err := c.Cookie("llmr_oauth_pkce")
	if err != nil {
		loginFail("Login failed")
		return
	}

	ctx := context.Background()

	token, err := oa2.Exchange(ctx, c.Query("code"), oauth2.VerifierOption(pkceVerifier))
	if err != nil {
		log.Printf("oauth2 exchange error: %v", err)
		loginFail("Login failed")
		return
	}

	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok {
		loginFail("Login failed")
		return
	}
	idToken, err := provider.Verifier(&gooidc.Config{ClientID: dbCfg.OAuthClientID}).Verify(ctx, rawIDToken)
	if err != nil {
		log.Printf("oidc verify error: %v", err)
		loginFail("Login failed")
		return
	}

	var claims map[string]interface{}
	if err := idToken.Claims(&claims); err != nil {
		loginFail("Login failed")
		return
	}

	sub, _ := claims["sub"].(string)
	email, _ := claims["email"].(string)
	preferredUsername, _ := claims["preferred_username"].(string)

	username := email
	if username == "" {
		username = preferredUsername
	}
	if username == "" {
		username = sub
	}

	adminValues := strings.Split(dbCfg.OAuthAdminValues, ",")
	role := mapRole(claims, dbCfg.OAuthRoleClaim, adminValues)

	user, err := h.store.FindOrCreateOAuthUser(sub, email, username, role)
	if err != nil {
		log.Printf("oauth find/create user error: %v", err)
		loginFail("Login failed")
		return
	}

	sessionToken, err := h.store.CreateSession(user.ID)
	if err != nil {
		loginFail("Login failed")
		return
	}

	http.SetCookie(c.Writer, &http.Cookie{
		Name:     sessionCookie,
		Value:    sessionToken,
		MaxAge:   86400,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})

	for _, name := range []string{"llmr_oauth_state", "llmr_oauth_pkce"} {
		http.SetCookie(c.Writer, &http.Cookie{
			Name:     name,
			Value:    "",
			MaxAge:   -1,
			Path:     "/admin/oauth/",
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
		})
	}

	if user.Role == "admin" {
		c.Redirect(http.StatusFound, "/admin/dashboard")
	} else {
		c.Redirect(http.StatusFound, "/admin/keys")
	}
}

// mapRole determines a user's role based on OIDC claims.
// claimName supports dot-notation for nested claims (e.g. "realm_access.roles").
func mapRole(claims map[string]interface{}, claimName string, adminValues []string) string {
	// Resolve dot-notation
	parts := strings.SplitN(claimName, ".", 2)
	var claimVal interface{}
	if len(parts) == 2 {
		if nested, ok := claims[parts[0]].(map[string]interface{}); ok {
			claimVal = nested[parts[1]]
		}
	} else {
		claimVal = claims[claimName]
	}

	adminSet := make(map[string]struct{}, len(adminValues))
	for _, v := range adminValues {
		adminSet[strings.TrimSpace(v)] = struct{}{}
	}

	check := func(s string) bool {
		_, ok := adminSet[s]
		return ok
	}

	switch v := claimVal.(type) {
	case string:
		if check(v) {
			return "admin"
		}
	case []interface{}:
		for _, item := range v {
			if s, ok := item.(string); ok && check(s) {
				return "admin"
			}
		}
	}
	return "user"
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
	Page         string
	Role         string
	Cfg          store.AppConfig
	Providers    []store.Provider
	Saved        bool
	Errors       []string
	OAuthEnabled bool
}

func (h *AdminHandler) ConfigPage(c *gin.Context) {
	cfg, err := h.store.GetConfig()
	if err != nil {
		log.Printf("get config: %v", err)
	}
	providers, _ := h.store.ListProviders()
	h.render(c, "config.html", configData{
		Page:         "config",
		Role:         c.GetString("user_role"),
		Cfg:          cfg,
		Providers:    providers,
		OAuthEnabled: cfg.OAuthEnabled,
	})
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
		"thinking_provider_id":   c.PostForm("thinking_provider_id"),
		"coding_provider_id":     c.PostForm("coding_provider_id"),
		"simple_provider_id":     c.PostForm("simple_provider_id"),
		"default_provider_id":    c.PostForm("default_provider_id"),
		// OAuth text fields (secret handled separately below)
		"oauth_issuer_url":   c.PostForm("oauth_issuer_url"),
		"oauth_client_id":    c.PostForm("oauth_client_id"),
		"oauth_redirect_url": c.PostForm("oauth_redirect_url"),
		"oauth_scopes":       c.PostForm("oauth_scopes"),
		"oauth_role_claim":   c.PostForm("oauth_role_claim"),
		"oauth_admin_values": c.PostForm("oauth_admin_values"),
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
		providers, _ := h.store.ListProviders()
		h.render(c, "config.html", configData{Page: "config", Role: role, Cfg: cfg, Providers: providers, Errors: errs})
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

	// Checkboxes: only present in POST when checked; must always be saved explicitly.
	oauthEnabled := "false"
	if c.PostForm("oauth_enabled") == "on" {
		oauthEnabled = "true"
	}
	if err := h.store.SetConfigValue("oauth_enabled", oauthEnabled); err != nil {
		log.Printf("set config oauth_enabled: %v", err)
		errs = append(errs, fmt.Sprintf("Failed to save oauth_enabled: %v", err))
	}
	// Invalidate cached OIDC provider whenever OAuth settings are saved so next
	// request picks up any issuer URL change.
	h.mu.Lock()
	h.provider = nil
	h.lastIssuerURL = ""
	h.mu.Unlock()

	oauthPwFallback := "false"
	if c.PostForm("oauth_password_fallback") == "on" {
		oauthPwFallback = "true"
	}
	if err := h.store.SetConfigValue("oauth_password_fallback", oauthPwFallback); err != nil {
		log.Printf("set config oauth_password_fallback: %v", err)
		errs = append(errs, fmt.Sprintf("Failed to save oauth_password_fallback: %v", err))
	}

	// Client secret: only overwrite if a new value was provided.
	if secret := c.PostForm("oauth_client_secret"); secret != "" {
		if err := h.store.SetConfigValue("oauth_client_secret", secret); err != nil {
			log.Printf("set config oauth_client_secret: %v", err)
			errs = append(errs, fmt.Sprintf("Failed to save oauth_client_secret: %v", err))
		}
	}

	cfg, _ := h.store.GetConfig()
	providers, _ := h.store.ListProviders()
	if len(errs) > 0 {
		h.render(c, "config.html", configData{Page: "config", Role: role, Cfg: cfg, Providers: providers, OAuthEnabled: cfg.OAuthEnabled, Errors: errs})
		return
	}
	h.render(c, "config.html", configData{Page: "config", Role: role, Cfg: cfg, Providers: providers, OAuthEnabled: cfg.OAuthEnabled, Saved: true})
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

// --- Providers ---

type providersData struct {
	Page      string
	Role      string
	Providers []store.Provider
	Error     string
	Saved     bool
}

func (h *AdminHandler) ProvidersPage(c *gin.Context) {
	providers, err := h.store.ListProviders()
	if err != nil {
		log.Printf("list providers: %v", err)
	}
	h.render(c, "providers.html", providersData{
		Page:      "providers",
		Role:      c.GetString("user_role"),
		Providers: providers,
	})
}

func (h *AdminHandler) ProviderCreate(c *gin.Context) {
	name := c.PostForm("name")
	provType := c.PostForm("type")
	baseURL := c.PostForm("base_url")
	apiKey := c.PostForm("api_key")

	providers, _ := h.store.ListProviders()
	role := c.GetString("user_role")

	if name == "" || baseURL == "" {
		h.render(c, "providers.html", providersData{
			Page: "providers", Role: role, Providers: providers,
			Error: "Name and Base URL are required.",
		})
		return
	}
	if provType != "ollama" && provType != "openai" && provType != "anthropic" {
		h.render(c, "providers.html", providersData{
			Page: "providers", Role: role, Providers: providers,
			Error: "Type must be ollama, openai, or anthropic.",
		})
		return
	}
	if u, err := url.Parse(baseURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		h.render(c, "providers.html", providersData{
			Page: "providers", Role: role, Providers: providers,
			Error: "Base URL must be a valid http(s) URL.",
		})
		return
	}

	if _, err := h.store.CreateProvider(name, provType, baseURL, apiKey); err != nil {
		log.Printf("create provider: %v", err)
		h.render(c, "providers.html", providersData{
			Page: "providers", Role: role, Providers: providers,
			Error: "Failed to create provider (name may already exist).",
		})
		return
	}
	c.Redirect(http.StatusFound, "/admin/providers")
}

func (h *AdminHandler) ProviderUpdate(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		c.Redirect(http.StatusFound, "/admin/providers")
		return
	}
	name := c.PostForm("name")
	provType := c.PostForm("type")
	baseURL := c.PostForm("base_url")
	apiKey := c.PostForm("api_key")

	providers, _ := h.store.ListProviders()
	role := c.GetString("user_role")

	if name == "" || baseURL == "" {
		h.render(c, "providers.html", providersData{
			Page: "providers", Role: role, Providers: providers,
			Error: "Name and Base URL are required.",
		})
		return
	}

	// If api_key field left blank, keep existing key.
	if apiKey == "" {
		existing, err := h.store.GetProvider(id)
		if err == nil {
			apiKey = existing.APIKey
		}
	}

	if err := h.store.UpdateProvider(id, name, provType, baseURL, apiKey); err != nil {
		log.Printf("update provider %d: %v", id, err)
		h.render(c, "providers.html", providersData{
			Page: "providers", Role: role, Providers: providers,
			Error: fmt.Sprintf("Failed to update provider: %v", err),
		})
		return
	}
	c.Redirect(http.StatusFound, "/admin/providers")
}

func (h *AdminHandler) ProviderDelete(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		c.Redirect(http.StatusFound, "/admin/providers")
		return
	}
	if err := h.store.DeleteProvider(id); err != nil {
		log.Printf("delete provider %d: %v", id, err)
	}
	c.Redirect(http.StatusFound, "/admin/providers")
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
