package middleware

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"go.uber.org/zap"

	"github.com/mdshabbir-ali/code-runtime/internal/config"
	"github.com/mdshabbir-ali/code-runtime/internal/database"
)

// ContextKey is the type used for context keys to avoid collisions.
type ContextKey string

const (
	// ContextKeyUserID is the context key for the authenticated user ID.
	ContextKeyUserID ContextKey = "user_id"
	// ContextKeyUserEmail is the context key for the authenticated user email.
	ContextKeyUserEmail ContextKey = "user_email"
	// ContextKeyIsAdmin is the context key for the admin flag.
	ContextKeyIsAdmin ContextKey = "is_admin"
	// ContextKeyAPIKeyID is the context key for the API key ID (if key auth was used).
	ContextKeyAPIKeyID ContextKey = "api_key_id"
)

// Claims holds the JWT payload.
type Claims struct {
	UserID  uint   `json:"user_id"`
	Email   string `json:"email"`
	IsAdmin bool   `json:"is_admin"`
	jwt.RegisteredClaims
}

// authPaths lists URL prefixes that do NOT require authentication.
var authWhitelist = []string{
	"/health",
	"/ready",
	"/metrics",
	"/swagger",
	"/auth/token",
}

// Auth returns a Gin middleware that validates JWT Bearer tokens or
// X-Auth-Token API keys.  Whitelisted paths pass through without auth.
func Auth(cfg *config.Config, userRepo database.UserRepository, log *zap.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		path := c.Request.URL.Path

		// Whitelist check — skip auth for health / metrics / swagger / auth endpoints.
		for _, prefix := range authWhitelist {
			if strings.HasPrefix(path, prefix) {
				c.Next()
				return
			}
		}

		// Try JWT Bearer first.
		if authHeader := c.GetHeader("Authorization"); authHeader != "" {
			if strings.HasPrefix(authHeader, "Bearer ") {
				tokenStr := strings.TrimPrefix(authHeader, "Bearer ")
				claims, err := validateJWT(tokenStr, cfg.JWT.Secret)
				if err != nil {
					log.Debug("invalid JWT", zap.String("path", path), zap.Error(err))
					c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
						"error": "invalid or expired token",
					})
					return
				}
				injectJWTClaims(c, claims)
				c.Next()
				return
			}
		}

		// Try API key header.
		if apiKey := c.GetHeader("X-Auth-Token"); apiKey != "" {
			hash := hashAPIKey(apiKey)
			ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
			defer cancel()

			key, err := userRepo.GetAPIKeyByHash(ctx, hash)
			if err != nil {
				log.Debug("API key not found", zap.String("path", path))
				c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
					"error": "invalid API key",
				})
				return
			}

			// Check expiry.
			if key.ExpiresAt != nil && key.ExpiresAt.Before(time.Now()) {
				c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
					"error": "API key expired",
				})
				return
			}

			// Update last used asynchronously.
			go func() {
				bgCtx, bgCancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer bgCancel()
				_ = userRepo.UpdateAPIKeyLastUsed(bgCtx, key.ID, time.Now())
			}()

			// Inject user info.
			if key.User != nil {
				c.Set(string(ContextKeyUserID), key.User.ID)
				c.Set(string(ContextKeyUserEmail), key.User.Email)
				c.Set(string(ContextKeyIsAdmin), key.User.IsAdmin)
			}
			c.Set(string(ContextKeyAPIKeyID), key.ID)
			c.Next()
			return
		}

		// No valid credentials found.
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
			"error": "authentication required",
		})
	}
}

// validateJWT parses and validates a JWT string, returning the claims on success.
func validateJWT(tokenStr, secret string) (*Claims, error) {
	token, err := jwt.ParseWithClaims(tokenStr, &Claims{}, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return []byte(secret), nil
	})
	if err != nil {
		return nil, err
	}
	if !token.Valid {
		return nil, errors.New("token is not valid")
	}
	claims, ok := token.Claims.(*Claims)
	if !ok {
		return nil, errors.New("invalid claims type")
	}
	return claims, nil
}

// injectJWTClaims stores user info from JWT claims into the Gin context.
func injectJWTClaims(c *gin.Context, claims *Claims) {
	c.Set(string(ContextKeyUserID), claims.UserID)
	c.Set(string(ContextKeyUserEmail), claims.Email)
	c.Set(string(ContextKeyIsAdmin), claims.IsAdmin)
}

// hashAPIKey returns the SHA-256 hex hash of a raw API key string.
func hashAPIKey(key string) string {
	h := sha256.Sum256([]byte(key))
	return fmt.Sprintf("%x", h)
}

// MustBeAdmin is an additional guard that enforces admin-only access.
func MustBeAdmin() gin.HandlerFunc {
	return func(c *gin.Context) {
		isAdmin, _ := c.Get(string(ContextKeyIsAdmin))
		if admin, ok := isAdmin.(bool); !ok || !admin {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"error": "admin access required",
			})
			return
		}
		c.Next()
	}
}
