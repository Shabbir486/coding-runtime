package cache

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/mdshabbir-ali/code-runtime/internal/config"
	"github.com/mdshabbir-ali/code-runtime/internal/models"
)

// Client is an alias for Cache so that callers that use "cache.Client" continue
// to compile without modification.  Both names refer to the same type.
type Client = Cache

// NewClient builds a Cache from the application-level Config.  It maps the
// config.RedisConfig fields to the cache-internal RedisConfig and delegates to
// NewRedis so that all connection logic (retry, cluster, sentinel) lives in one
// place.
func NewClient(cfg *config.Config, log *zap.Logger) (*Client, error) {
	r := cfg.Redis
	redisCfg := RedisConfig{
		Host:     r.Host,
		Port:     r.Port,
		Password: r.Password,
		DB:       r.DB,
		PoolSize: r.PoolSize,
	}
	return NewRedis(redisCfg, log)
}

// ---- LanguageCache ----------------------------------------------------------

const (
	languageTTL  = 1 * time.Hour
	languagesKey = "languages:all"
)

func languageKey(id int) string {
	return fmt.Sprintf("language:%d", id)
}

// LanguageCache provides language-list caching on top of Cache.
type LanguageCache struct {
	cache *Cache
}

// NewLanguageCache wraps a Cache with language-specific helpers.
func NewLanguageCache(c *Cache) *LanguageCache {
	return &LanguageCache{cache: c}
}

// SetAll caches the full language list.
func (lc *LanguageCache) SetAll(ctx context.Context, langs []*models.Language) error {
	if err := lc.cache.Set(ctx, languagesKey, langs, languageTTL); err != nil {
		return fmt.Errorf("languageCache.SetAll: %w", err)
	}
	return nil
}

// GetAll retrieves the full language list. Returns (nil, nil) on cache miss.
func (lc *LanguageCache) GetAll(ctx context.Context) ([]*models.Language, error) {
	var langs []*models.Language
	if err := lc.cache.Get(ctx, languagesKey, &langs); err != nil {
		if IsCacheMiss(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("languageCache.GetAll: %w", err)
	}
	return langs, nil
}

// Set caches a single language by ID.
func (lc *LanguageCache) Set(ctx context.Context, lang *models.Language) error {
	if err := lc.cache.Set(ctx, languageKey(lang.ID), lang, languageTTL); err != nil {
		return fmt.Errorf("languageCache.Set %d: %w", lang.ID, err)
	}
	return nil
}

// GetByID retrieves a single language. Returns (nil, nil) on cache miss.
func (lc *LanguageCache) GetByID(ctx context.Context, id int) (*models.Language, error) {
	var lang models.Language
	if err := lc.cache.Get(ctx, languageKey(id), &lang); err != nil {
		if IsCacheMiss(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("languageCache.GetByID %d: %w", id, err)
	}
	return &lang, nil
}

// Invalidate removes the language list and the individual language entry.
func (lc *LanguageCache) Invalidate(ctx context.Context, id int) error {
	_ = lc.cache.Delete(ctx, languagesKey)
	_ = lc.cache.Delete(ctx, languageKey(id))
	return nil
}