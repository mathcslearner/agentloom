package config

import "time"

// Environment variables read by APIConfig.
const (
	EnvAPIAddr            = "AGENTLOOM_API_ADDR"
	EnvAPIReadTimeout     = "AGENTLOOM_API_READ_TIMEOUT"
	EnvAPIWriteTimeout    = "AGENTLOOM_API_WRITE_TIMEOUT"
	EnvAPIIdleTimeout     = "AGENTLOOM_API_IDLE_TIMEOUT"
	EnvAPIShutdownTimeout = "AGENTLOOM_API_SHUTDOWN_TIMEOUT"
)

// API server defaults (ticket 4.6, dev mode). The timeouts are generous
// because v1 request handling is a handful of Postgres round trips;
// they exist so a stuck client can never pin a connection forever.
const (
	DefaultAPIAddr            = ":8080"
	DefaultAPIReadTimeout     = 10 * time.Second
	DefaultAPIWriteTimeout    = 30 * time.Second
	DefaultAPIIdleTimeout     = 2 * time.Minute
	DefaultAPIShutdownTimeout = 15 * time.Second
)

// APIConfig configures the API deployable (cmd/api) beyond what the shared
// component configs already carry: Postgres via PostgresConfig. The API
// holds no Redis client — dispatch is the worker fleet's job (ADR-002).
type APIConfig struct {
	// Addr is the listen address, in net.Listen "host:port" form.
	Addr string
	// ReadTimeout bounds reading one request (headers + body). Must be
	// positive.
	ReadTimeout time.Duration
	// WriteTimeout bounds writing one response. Must be positive.
	WriteTimeout time.Duration
	// IdleTimeout bounds how long a keep-alive connection may sit idle.
	// Must be positive.
	IdleTimeout time.Duration
	// ShutdownTimeout bounds the graceful drain on SIGINT/SIGTERM; after
	// it, remaining connections are closed hard. Must be positive.
	ShutdownTimeout time.Duration
}

func defaultAPIConfig() APIConfig {
	return APIConfig{
		Addr:            DefaultAPIAddr,
		ReadTimeout:     DefaultAPIReadTimeout,
		WriteTimeout:    DefaultAPIWriteTimeout,
		IdleTimeout:     DefaultAPIIdleTimeout,
		ShutdownTimeout: DefaultAPIShutdownTimeout,
	}
}

// applyEnv overrides fields from the environment, returning one error per
// invalid variable. Addr is passed through opaquely — a malformed address
// fails at listen time with a clearer error than any pre-check here.
func (c *APIConfig) applyEnv(fn LookupFunc) []error {
	var errs []error
	if raw, ok := lookup(fn, EnvAPIAddr); ok {
		c.Addr = raw
	}
	errs = applyPositiveDuration(errs, fn, EnvAPIReadTimeout, &c.ReadTimeout)
	errs = applyPositiveDuration(errs, fn, EnvAPIWriteTimeout, &c.WriteTimeout)
	errs = applyPositiveDuration(errs, fn, EnvAPIIdleTimeout, &c.IdleTimeout)
	errs = applyPositiveDuration(errs, fn, EnvAPIShutdownTimeout, &c.ShutdownTimeout)
	return errs
}
