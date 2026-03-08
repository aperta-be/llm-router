package middleware

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/aperta-be/llm-router/store"
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

// UserAuth validates the session cookie and sets user_id and user_role in gin context.
func UserAuth(s *store.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		token, err := c.Cookie(sessionCookie)
		if err != nil {
			c.Redirect(http.StatusFound, "/admin/login")
			c.Abort()
			return
		}
		userID, role, err := s.GetUserIDFromSession(token)
		if err != nil {
			c.Redirect(http.StatusFound, "/admin/login")
			c.Abort()
			return
		}
		c.Set("session_token", token)
		c.Set("user_id", userID)
		c.Set("user_role", role)
		c.Next()
	}
}

// RequireAdmin aborts with a redirect if the user is not an admin.
// Must be used after UserAuth.
func RequireAdmin() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.GetString("user_role") != "admin" {
			c.Redirect(http.StatusFound, "/admin/keys")
			c.Abort()
			return
		}
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
