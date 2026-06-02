package config

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/viper"
)

// defaultServiceName is the canonical service name used in defaults.
const defaultServiceName = "code-runtime"

// Config is the root configuration structure for the application.
type Config struct {
	Server    ServerConfig    `mapstructure:"server"`
	Database  DatabaseConfig  `mapstructure:"database"`
	Redis     RedisConfig     `mapstructure:"redis"`
	NATS      NATSConfig      `mapstructure:"nats"`
	Docker    DockerConfig    `mapstructure:"docker"`
	JWT       JWTConfig       `mapstructure:"jwt"`
	Worker    WorkerConfig    `mapstructure:"worker"`
	Metrics   MetricsConfig   `mapstructure:"metrics"`
	Tracing   TracingConfig   `mapstructure:"tracing"`
	RateLimit RateLimitConfig `mapstructure:"rate_limit"`
}

// ServerConfig holds HTTP server settings.
type ServerConfig struct {
	Host            string        `mapstructure:"host"`
	Port            int           `mapstructure:"port"`
	ReadTimeout     time.Duration `mapstructure:"read_timeout"`
	WriteTimeout    time.Duration `mapstructure:"write_timeout"`
	IdleTimeout     time.Duration `mapstructure:"idle_timeout"`
	ShutdownTimeout time.Duration `mapstructure:"shutdown_timeout"`
	Mode            string        `mapstructure:"mode"` // "debug" | "release" | "test"
	TrustedProxies  []string      `mapstructure:"trusted_proxies"`
	AllowedOrigins  []string      `mapstructure:"allowed_origins"`
}

// Address returns the host:port string.
func (s ServerConfig) Address() string {
	return fmt.Sprintf("%s:%d", s.Host, s.Port)
}

// DatabaseConfig holds PostgreSQL connection settings.
type DatabaseConfig struct {
	Host            string        `mapstructure:"host"`
	Port            int           `mapstructure:"port"`
	Name            string        `mapstructure:"name"`
	User            string        `mapstructure:"user"`
	Password        string        `mapstructure:"password"`
	SSLMode         string        `mapstructure:"ssl_mode"`
	MaxOpenConns    int           `mapstructure:"max_open_conns"`
	MaxIdleConns    int           `mapstructure:"max_idle_conns"`
	ConnMaxLifetime time.Duration `mapstructure:"conn_max_lifetime"`
	ConnMaxIdleTime time.Duration `mapstructure:"conn_max_idle_time"`
	MigrateOnStart  bool          `mapstructure:"migrate_on_start"`
}

// DSN returns the PostgreSQL connection string.
func (d DatabaseConfig) DSN() string {
	return fmt.Sprintf(
		"host=%s port=%d user=%s password=%s dbname=%s sslmode=%s",
		d.Host, d.Port, d.User, d.Password, d.Name, d.SSLMode,
	)
}

// RedisConfig holds Redis connection settings.
type RedisConfig struct {
	Host         string        `mapstructure:"host"`
	Port         int           `mapstructure:"port"`
	Password     string        `mapstructure:"password"`
	DB           int           `mapstructure:"db"`
	PoolSize     int           `mapstructure:"pool_size"`
	MinIdleConns int           `mapstructure:"min_idle_conns"`
	DialTimeout  time.Duration `mapstructure:"dial_timeout"`
	ReadTimeout  time.Duration `mapstructure:"read_timeout"`
	WriteTimeout time.Duration `mapstructure:"write_timeout"`
	KeyPrefix    string        `mapstructure:"key_prefix"`
}

// Addr returns the host:port string for Redis.
func (r RedisConfig) Addr() string {
	return fmt.Sprintf("%s:%d", r.Host, r.Port)
}

// NATSConfig holds NATS JetStream connection settings.
type NATSConfig struct {
	URL              string        `mapstructure:"url"`
	ClusterID        string        `mapstructure:"cluster_id"`
	ClientID         string        `mapstructure:"client_id"`
	ConnectTimeout   time.Duration `mapstructure:"connect_timeout"`
	ReconnectWait    time.Duration `mapstructure:"reconnect_wait"`
	MaxReconnects    int           `mapstructure:"max_reconnects"`
	SubmissionStream string        `mapstructure:"submission_stream"`
	ResultStream     string        `mapstructure:"result_stream"`
	WorkerSubject    string        `mapstructure:"worker_subject"`
	HeartbeatSubject string        `mapstructure:"heartbeat_subject"`
	QueueGroup       string        `mapstructure:"queue_group"`
	AckWait          time.Duration `mapstructure:"ack_wait"`
	MaxDeliver       int           `mapstructure:"max_deliver"`
	MaxAckPending    int           `mapstructure:"max_ack_pending"`
}

// DockerConfig holds Docker daemon and container execution settings.
type DockerConfig struct {
	Host               string        `mapstructure:"host"`
	TLSVerify          bool          `mapstructure:"tls_verify"`
	CertPath           string        `mapstructure:"cert_path"`
	NetworkMode        string        `mapstructure:"network_mode"`
	PullPolicy         string        `mapstructure:"pull_policy"` // "always" | "missing" | "never"
	ContainerTimeout   time.Duration `mapstructure:"container_timeout"`
	MemorySwap         int64         `mapstructure:"memory_swap"`
	CPUShares          int64         `mapstructure:"cpu_shares"`
	CPUPeriod          int64         `mapstructure:"cpu_period"`
	CPUQuota           int64         `mapstructure:"cpu_quota"`
	PidsLimit          int64         `mapstructure:"pids_limit"`
	ReadOnly           bool          `mapstructure:"read_only"`
	NoNewPrivileges    bool          `mapstructure:"no_new_privileges"`
	CapDrop            []string      `mapstructure:"cap_drop"`
	SeccompProfile     string        `mapstructure:"seccomp_profile"`
	WorkDir            string        `mapstructure:"work_dir"`
	MaxContainers      int           `mapstructure:"max_containers"`
	ContainerNamespace string        `mapstructure:"container_namespace"`
	Registry           string        `mapstructure:"registry"`      // prefix for on-demand runtime image pulls
	RegistryAuth       string        `mapstructure:"registry_auth"` // base64 X-Registry-Auth for a private registry
}

// JWTConfig holds JWT signing settings.
type JWTConfig struct {
	Secret          string        `mapstructure:"secret"`
	AccessTokenExp  time.Duration `mapstructure:"access_token_exp"`
	RefreshTokenExp time.Duration `mapstructure:"refresh_token_exp"`
	Issuer          string        `mapstructure:"issuer"`
	Audience        string        `mapstructure:"audience"`
}

// WorkerConfig holds execution worker pool settings.
type WorkerConfig struct {
	Count           int           `mapstructure:"count"`
	Concurrency     int           `mapstructure:"concurrency"`   // max concurrent jobs per worker
	NATSSubject     string        `mapstructure:"nats_subject"`  // NATS subject to consume
	MaxRetries      int           `mapstructure:"max_retries"`
	RetryDelay      time.Duration `mapstructure:"retry_delay"`
	PollInterval    time.Duration `mapstructure:"poll_interval"`
	HeartbeatTick   time.Duration `mapstructure:"heartbeat_tick"`
	JobTimeout      time.Duration `mapstructure:"job_timeout"`
	CleanupInterval time.Duration `mapstructure:"cleanup_interval"`
	TmpDir          string        `mapstructure:"tmp_dir"`
	PrewarmImages   []string      `mapstructure:"prewarm_images"` // Docker images to pull on start
}

// MetricsConfig holds Prometheus metrics settings.
type MetricsConfig struct {
	Enabled   bool   `mapstructure:"enabled"`
	Path      string `mapstructure:"path"`
	Namespace string `mapstructure:"namespace"`
	Subsystem string `mapstructure:"subsystem"`
}

// TracingConfig holds OpenTelemetry tracing settings.
type TracingConfig struct {
	Enabled     bool    `mapstructure:"enabled"`
	ServiceName string  `mapstructure:"service_name"`
	Endpoint    string  `mapstructure:"endpoint"`
	SampleRate  float64 `mapstructure:"sample_rate"`
	Insecure    bool    `mapstructure:"insecure"`
}

// RateLimitConfig holds API rate limiting settings.
type RateLimitConfig struct {
	Enabled      bool          `mapstructure:"enabled"`
	Limit        int64         `mapstructure:"limit"`
	Period       time.Duration `mapstructure:"period"`
	StoreType    string        `mapstructure:"store_type"` // "memory" | "redis"
	TrustForward bool          `mapstructure:"trust_forward"`
}

// Validate checks that required configuration fields are non-zero.
func (c *Config) Validate() error {
	if c.Database.Host == "" {
		return fmt.Errorf("database.host is required")
	}
	if c.Database.Name == "" {
		return fmt.Errorf("database.name is required")
	}
	if c.JWT.Secret == "" {
		return fmt.Errorf("jwt.secret is required")
	}
	if c.Server.Port <= 0 || c.Server.Port > 65535 {
		return fmt.Errorf("server.port must be between 1 and 65535")
	}
	if c.Worker.Count <= 0 {
		return fmt.Errorf("worker.count must be positive")
	}
	return nil
}

// LoadConfig reads configuration from a YAML file and environment variables,
// applies defaults, and returns a validated Config.
func LoadConfig(configPath string) (*Config, error) {
	v := viper.New()

	// Set defaults.
	setDefaults(v)

	// Read configuration file if provided.
	if configPath != "" {
		v.SetConfigFile(configPath)
		if err := v.ReadInConfig(); err != nil {
			// Non-fatal: fall back to environment variables and defaults.
			_ = err
		}
	} else {
		v.SetConfigName("config")
		v.SetConfigType("yaml")
		v.AddConfigPath(".")
		v.AddConfigPath("./configs")
		v.AddConfigPath("/etc/code-runtime")
		_ = v.ReadInConfig()
	}

	// Environment variable overrides.
	v.SetEnvPrefix("CODERUNTIME")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("failed to unmarshal config: %w", err)
	}

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("config validation failed: %w", err)
	}

	return &cfg, nil
}

// setDefaults registers sane default values with Viper.
func setDefaults(v *viper.Viper) {
	// Server
	v.SetDefault("server.host", "0.0.0.0")
	v.SetDefault("server.port", 8002)
	v.SetDefault("server.read_timeout", 30*time.Second)
	v.SetDefault("server.write_timeout", 30*time.Second)
	v.SetDefault("server.idle_timeout", 60*time.Second)
	v.SetDefault("server.shutdown_timeout", 10*time.Second)
	v.SetDefault("server.mode", "release")
	v.SetDefault("server.trusted_proxies", []string{})
	v.SetDefault("server.allowed_origins", []string{"*"})

	// Database
	v.SetDefault("database.host", "localhost")
	v.SetDefault("database.port", 5432)
	v.SetDefault("database.name", "coderuntime")
	v.SetDefault("database.user", "postgres")
	v.SetDefault("database.password", "")
	v.SetDefault("database.ssl_mode", "disable")
	v.SetDefault("database.max_open_conns", 25)
	v.SetDefault("database.max_idle_conns", 10)
	v.SetDefault("database.conn_max_lifetime", 30*time.Minute)
	v.SetDefault("database.conn_max_idle_time", 10*time.Minute)
	v.SetDefault("database.migrate_on_start", true)

	// Redis
	v.SetDefault("redis.host", "localhost")
	v.SetDefault("redis.port", 6379)
	v.SetDefault("redis.password", "")
	v.SetDefault("redis.db", 0)
	v.SetDefault("redis.pool_size", 20)
	v.SetDefault("redis.min_idle_conns", 5)
	v.SetDefault("redis.dial_timeout", 5*time.Second)
	v.SetDefault("redis.read_timeout", 3*time.Second)
	v.SetDefault("redis.write_timeout", 3*time.Second)
	v.SetDefault("redis.key_prefix", "cr:")

	// NATS
	v.SetDefault("nats.url", "nats://localhost:4222")
	v.SetDefault("nats.cluster_id", "code-runtime-cluster")
	v.SetDefault("nats.client_id", "code-runtime-server")
	v.SetDefault("nats.connect_timeout", 10*time.Second)
	v.SetDefault("nats.reconnect_wait", 2*time.Second)
	v.SetDefault("nats.max_reconnects", -1)
	v.SetDefault("nats.submission_stream", "SUBMISSIONS")
	v.SetDefault("nats.result_stream", "RESULTS")
	v.SetDefault("nats.worker_subject", "worker.execute")
	v.SetDefault("nats.heartbeat_subject", "worker.heartbeat")
	v.SetDefault("nats.queue_group", "workers")
	v.SetDefault("nats.ack_wait", 60*time.Second)
	v.SetDefault("nats.max_deliver", 3)
	v.SetDefault("nats.max_ack_pending", 100)

	// Docker
	v.SetDefault("docker.host", "unix:///var/run/docker.sock")
	v.SetDefault("docker.registry", "")
	v.SetDefault("docker.registry_auth", "")
	v.SetDefault("docker.tls_verify", false)
	v.SetDefault("docker.network_mode", "none")
	v.SetDefault("docker.pull_policy", "missing")
	v.SetDefault("docker.container_timeout", 30*time.Second)
	v.SetDefault("docker.memory_swap", int64(-1))
	v.SetDefault("docker.cpu_shares", int64(512))
	v.SetDefault("docker.cpu_period", int64(100000))
	v.SetDefault("docker.cpu_quota", int64(50000))
	v.SetDefault("docker.pids_limit", int64(64))
	v.SetDefault("docker.read_only", true)
	v.SetDefault("docker.no_new_privileges", true)
	v.SetDefault("docker.cap_drop", []string{"ALL"})
	v.SetDefault("docker.work_dir", "/sandbox")
	v.SetDefault("docker.max_containers", 50)
	v.SetDefault("docker.container_namespace", defaultServiceName)

	// JWT
	v.SetDefault("jwt.secret", "changeme-use-a-strong-secret-in-production")
	v.SetDefault("jwt.access_token_exp", 15*time.Minute)
	v.SetDefault("jwt.refresh_token_exp", 7*24*time.Hour)
	v.SetDefault("jwt.issuer", defaultServiceName)
	v.SetDefault("jwt.audience", "code-runtime-api")

	// Worker
	v.SetDefault("worker.count", 4)
	v.SetDefault("worker.concurrency", 4)
	v.SetDefault("worker.nats_subject", "submissions.execute")
	v.SetDefault("worker.max_retries", 3)
	v.SetDefault("worker.retry_delay", 5*time.Second)
	v.SetDefault("worker.poll_interval", 1*time.Second)
	v.SetDefault("worker.heartbeat_tick", 10*time.Second)
	v.SetDefault("worker.job_timeout", 60*time.Second)
	v.SetDefault("worker.cleanup_interval", 5*time.Minute)
	v.SetDefault("worker.tmp_dir", "/tmp/code-runtime")

	// Metrics
	v.SetDefault("metrics.enabled", true)
	v.SetDefault("metrics.path", "/metrics")
	v.SetDefault("metrics.namespace", "code_runtime")
	v.SetDefault("metrics.subsystem", "api")

	// Tracing
	v.SetDefault("tracing.enabled", false)
	v.SetDefault("tracing.service_name", defaultServiceName)
	v.SetDefault("tracing.endpoint", "http://localhost:4318")
	v.SetDefault("tracing.sample_rate", 1.0)
	v.SetDefault("tracing.insecure", true)

	// Rate limiting
	v.SetDefault("rate_limit.enabled", true)
	v.SetDefault("rate_limit.limit", int64(100))
	v.SetDefault("rate_limit.period", time.Minute)
	v.SetDefault("rate_limit.store_type", "redis")
	v.SetDefault("rate_limit.trust_forward", false)
}

// Load is a convenience wrapper that calls LoadConfig with the CONFIG_PATH
// environment variable, defaulting to an empty string (uses built-in defaults
// and environment variable overrides only).
func Load() (*Config, error) {
	return LoadConfig("")
}
