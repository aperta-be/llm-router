package middleware

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"llm-router/store"
)

const sessionCookie = "llmr_session"

// APIKeyAuth enforces API key validation on routes that require it.
// If no active keys exist in the DB, the endpoint is open (first-run convenience).
func APIKeyAuth(s *store.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		hasKeys, err := s.HasActiveKeys()
		if err != nil || !hasKeys {
			c.Next()
			return
		}

		rawKey := extractBearerToken(c)
		if rawKey == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing API key"})
			return
		}

		valid, err := s.ValidateAPIKey(rawKey)
		if err != nil || !valid {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid API key"})
			return
		}

		c.Next()
	}
}

// AdminAuth enforces session cookie validation on admin routes.
func AdminAuth(s *store.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		token, err := c.Cookie(sessionCookie)
		if err != nil || !s.ValidateSession(token) {
			c.Redirect(http.StatusFound, "/admin/login")
			c.Abort()
			return
		}
		c.Set("session_token", token)
		c.Next()
	}
}

func extractBearerToken(c *gin.Context) string {
	auth := c.GetHeader("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimPrefix(auth, "Bearer ")
	}
	// Also support X-API-Key header
	return c.GetHeader("X-API-Key")
}
