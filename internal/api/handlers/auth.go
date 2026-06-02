package handlers

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"go.uber.org/zap"
	"golang.org/x/crypto/bcrypt"

	"github.com/mdshabbir-ali/code-runtime/internal/api/middleware"
	"github.com/mdshabbir-ali/code-runtime/internal/config"
	"github.com/mdshabbir-ali/code-runtime/internal/database"
	"github.com/mdshabbir-ali/code-runtime/internal/models"
)

// AuthHandler handles /auth/* routes.
type AuthHandler struct {
	userRepo database.UserRepository
	cfg      *config.Config
	log      *zap.Logger
}

// NewAuthHandler constructs an AuthHandler.
func NewAuthHandler(userRepo database.UserRepository, cfg *config.Config, log *zap.Logger) *AuthHandler {
	return &AuthHandler{userRepo: userRepo, cfg: cfg, log: log}
}

// GenerateToken handles POST /auth/token — validates credentials, issues a JWT.
func (h *AuthHandler) GenerateToken(c *gin.Context) {
	var req models.LoginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()

	user, err := h.userRepo.GetByEmail(ctx, req.Email)
	if err != nil {
		// Generic error — do not reveal whether the user exists.
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid credentials"})
		return
	}

	if !user.IsActive {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "account is inactive"})
		return
	}

	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(req.Password)); err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid credentials"})
		return
	}

	tokenStr, expiresIn, err := h.issueJWT(user)
	if err != nil {
		h.log.Error("failed to sign JWT", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate token"})
		return
	}

	c.JSON(http.StatusOK, models.TokenResponse{
		AccessToken: tokenStr,
		TokenType:   "Bearer",
		ExpiresIn:   expiresIn,
	})
}

// GenerateAPIKey handles POST /auth/apikey — creates a new API key for the
// currently-authenticated user.
func (h *AuthHandler) GenerateAPIKey(c *gin.Context) {
	var req models.CreateAPIKeyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	userID, ok := c.Get(string(middleware.ContextKeyUserID))
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}

	uid, ok := userID.(uint)
	if !ok {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "invalid user context"})
		return
	}

	rawKey, prefix, keyHash, err := generateAPIKey()
	if err != nil {
		h.log.Error("failed to generate API key", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate API key"})
		return
	}

	apiKey := &models.APIKey{
		UserID:   &uid,
		Name:     req.Name,
		KeyHash:  keyHash,
		Prefix:   prefix,
		IsActive: true,
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()

	if err := h.userRepo.CreateAPIKey(ctx, apiKey); err != nil {
		h.log.Error("failed to save API key", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save API key"})
		return
	}

	c.JSON(http.StatusCreated, models.APIKeyResponse{
		ID:     apiKey.ID,
		Name:   apiKey.Name,
		Prefix: prefix,
		Key:    rawKey, // returned only once
	})
}

// ---- helpers ----------------------------------------------------------------

// issueJWT creates and signs a JWT for the given user.
// Returns the token string and its TTL in seconds.
func (h *AuthHandler) issueJWT(user *models.User) (string, int64, error) {
	expiry := h.cfg.JWT.AccessTokenExp
	now := time.Now()

	claims := middleware.Claims{
		UserID:  user.ID,
		Email:   user.Email,
		IsAdmin: user.IsAdmin,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    h.cfg.JWT.Issuer,
			Subject:   user.Email,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(expiry)),
		},
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString([]byte(h.cfg.JWT.Secret))
	if err != nil {
		return "", 0, err
	}

	return signed, int64(expiry.Seconds()), nil
}

// generateAPIKey generates a cryptographically random API key.
// Returns (rawKey, prefix, sha256Hash, error).
func generateAPIKey() (rawKey, prefix, hash string, err error) {
	b := make([]byte, 32)
	if _, err = rand.Read(b); err != nil {
		return
	}
	rawKey = hex.EncodeToString(b)
	prefix = rawKey[:8]
	sum := sha256.Sum256([]byte(rawKey))
	hash = hex.EncodeToString(sum[:])
	return
}
