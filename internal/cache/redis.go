package cache

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

// RedisConfig holds all configuration needed to open a Redis connection.
type RedisConfig struct {
	Host       string
	Port       int
	Password   string
	DB         int
	PoolSize   int
	TLSEnabled bool

	// Sentinel mode – set MasterName and at least one SentinelAddr to enable.
	MasterName    string
	SentinelAddrs []string

	// Cluster mode – set ClusterAddrs with two or more addresses to enable.
	ClusterAddrs []string
}

func (cfg *RedisConfig) defaults() {
	if cfg.Host == "" {
		cfg.Host = "localhost"
	}
	if cfg.Port == 0 {
		cfg.Port = 6379
	}
	if cfg.PoolSize == 0 {
		cfg.PoolSize = 10
	}
}

// addr returns the host:port string for standalone / sentinel mode.
func (cfg RedisConfig) addr() string {
	return fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
}

// tlsConfig returns a *tls.Config when TLS is enabled, otherwise nil.
func (cfg RedisConfig) tlsConfig() *tls.Config {
	if !cfg.TLSEnabled {
		return nil
	}
	return &tls.Config{MinVersion: tls.VersionTLS12}
}

// Cache wraps a redis.UniversalClient (standalone, sentinel, or cluster).
type Cache struct {
	client redis.UniversalClient
	log    *zap.Logger
}

// NewRedis creates and validates a Redis client. It supports three modes:
//   - Cluster  – when cfg.ClusterAddrs has two or more entries
//   - Sentinel – when cfg.MasterName and cfg.SentinelAddrs are set
//   - Standalone – default
//
// The function retries the initial PING up to 5 times with exponential back-off.
func NewRedis(cfg RedisConfig, log *zap.Logger) (*Cache, error) {
	cfg.defaults()

	if log == nil {
		log, _ = zap.NewProduction()
	}

	var client redis.UniversalClient

	switch {
	case len(cfg.ClusterAddrs) >= 2:
		log.Info("redis: connecting in cluster mode", zap.Strings("addrs", cfg.ClusterAddrs))
		client = redis.NewClusterClient(&redis.ClusterOptions{
			Addrs:     cfg.ClusterAddrs,
			Password:  cfg.Password,
			PoolSize:  cfg.PoolSize,
			TLSConfig: cfg.tlsConfig(),
		})

	case cfg.MasterName != "" && len(cfg.SentinelAddrs) > 0:
		log.Info("redis: connecting in sentinel mode",
			zap.String("master", cfg.MasterName),
			zap.Strings("sentinels", cfg.SentinelAddrs))
		client = redis.NewFailoverClient(&redis.FailoverOptions{
			MasterName:    cfg.MasterName,
			SentinelAddrs: cfg.SentinelAddrs,
			Password:      cfg.Password,
			DB:            cfg.DB,
			PoolSize:      cfg.PoolSize,
			TLSConfig:     cfg.tlsConfig(),
		})

	default:
		log.Info("redis: connecting in standalone mode", zap.String("addr", cfg.addr()))
		client = redis.NewClient(&redis.Options{
			Addr:      cfg.addr(),
			Password:  cfg.Password,
			DB:        cfg.DB,
			PoolSize:  cfg.PoolSize,
			TLSConfig: cfg.tlsConfig(),
		})
	}

	const maxRetries = 5
	var pingErr error
	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(math.Pow(2, float64(attempt-1))) * time.Second
			log.Warn("redis: ping failed, retrying",
				zap.Int("attempt", attempt),
				zap.Duration("backoff", backoff),
				zap.Error(pingErr),
			)
			time.Sleep(backoff)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		pingErr = client.Ping(ctx).Err()
		cancel()

		if pingErr == nil {
			break
		}
	}

	if pingErr != nil {
		_ = client.Close()
		return nil, fmt.Errorf("cache: failed to connect to redis after %d attempts: %w", maxRetries, pingErr)
	}

	log.Info("redis connected")
	return &Cache{client: client, log: log}, nil
}

// HealthCheck pings Redis and returns an error if it is unreachable.
func (c *Cache) HealthCheck(ctx context.Context) error {
	if err := c.client.Ping(ctx).Err(); err != nil {
		return fmt.Errorf("cache: health check ping failed: %w", err)
	}
	return nil
}

// Set serialises value to JSON and stores it under key with the given TTL.
// A TTL of 0 stores the key without expiry.
func (c *Cache) Set(ctx context.Context, key string, value interface{}, ttl time.Duration) error {
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("cache.Set marshal %q: %w", key, err)
	}
	if err = c.client.Set(ctx, key, data, ttl).Err(); err != nil {
		return fmt.Errorf("cache.Set %q: %w", key, err)
	}
	return nil
}

// Get retrieves a JSON-encoded value from Redis and deserialises it into dest.
// Returns ErrCacheMiss (wrapping redis.Nil) when the key does not exist.
func (c *Cache) Get(ctx context.Context, key string, dest interface{}) error {
	data, err := c.client.Get(ctx, key).Bytes()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return fmt.Errorf("cache.Get %q: %w", key, ErrCacheMiss)
		}
		return fmt.Errorf("cache.Get %q: %w", key, err)
	}
	if err = json.Unmarshal(data, dest); err != nil {
		return fmt.Errorf("cache.Get unmarshal %q: %w", key, err)
	}
	return nil
}

// Delete removes a key from Redis. A missing key is not treated as an error.
func (c *Cache) Delete(ctx context.Context, key string) error {
	if err := c.client.Del(ctx, key).Err(); err != nil {
		return fmt.Errorf("cache.Delete %q: %w", key, err)
	}
	return nil
}

// Exists reports whether key is present in Redis.
func (c *Cache) Exists(ctx context.Context, key string) (bool, error) {
	n, err := c.client.Exists(ctx, key).Result()
	if err != nil {
		return false, fmt.Errorf("cache.Exists %q: %w", key, err)
	}
	return n > 0, nil
}

// Incr atomically increments the integer stored at key by 1 and returns the
// resulting value. The key is created with value 0 if it does not yet exist.
func (c *Cache) Incr(ctx context.Context, key string) (int64, error) {
	v, err := c.client.Incr(ctx, key).Result()
	if err != nil {
		return 0, fmt.Errorf("cache.Incr %q: %w", key, err)
	}
	return v, nil
}

// Expire sets (or updates) the TTL on an existing key.
func (c *Cache) Expire(ctx context.Context, key string, ttl time.Duration) error {
	ok, err := c.client.Expire(ctx, key, ttl).Result()
	if err != nil {
		return fmt.Errorf("cache.Expire %q: %w", key, err)
	}
	if !ok {
		// Key did not exist; not treated as a hard error but worth logging.
		c.log.Debug("cache.Expire: key not found", zap.String("key", key))
	}
	return nil
}

// Client exposes the underlying redis.UniversalClient for advanced use-cases
// (e.g. pipelining, Lua scripts).
func (c *Cache) Client() redis.UniversalClient {
	return c.client
}

// RawClient returns the underlying redis.UniversalClient.
// Alias for Client(); provided for compatibility with rate-limit middleware.
func (c *Cache) RawClient() redis.UniversalClient {
	return c.client
}

// Ping checks if Redis is reachable. Alias for HealthCheck.
func (c *Cache) Ping(ctx context.Context) error {
	return c.HealthCheck(ctx)
}

// Close closes all connections in the pool.
func (c *Cache) Close() error {
	c.log.Info("closing redis connection pool")
	return c.client.Close()
}

// ErrCacheMiss is returned by Get when the requested key does not exist.
var ErrCacheMiss = errors.New("cache miss")

// IsCacheMiss returns true if err wraps ErrCacheMiss.
func IsCacheMiss(err error) bool {
	return errors.Is(err, ErrCacheMiss)
}
