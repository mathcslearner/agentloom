package config

import (
	"fmt"
	"math"
	"strconv"
	"time"
)

// Environment variables read by APIConfig.
const (
	EnvAPIAddr            = "AGENTLOOM_API_ADDR"
	EnvAPIReadTimeout     = "AGENTLOOM_API_READ_TIMEOUT"
	EnvAPIWriteTimeout    = "AGENTLOOM_API_WRITE_TIMEOUT"
	EnvAPIIdleTimeout     = "AGENTLOOM_API_IDLE_TIMEOUT"
	EnvAPIShutdownTimeout = "AGENTLOOM_API_SHUTDOWN_TIMEOUT"
	EnvAPIRootKey         = "AGENTLOOM_API_ROOT_KEY"
	EnvAPITestExecutors   = "AGENTLOOM_API_TEST_EXECUTORS"
)

// Environment variables read by APIRateLimitConfig (ticket 6.4, ADR-007).
const (
	EnvAPIRateLimitEnabled        = "AGENTLOOM_API_RATELIMIT_ENABLED"
	EnvAPIRateLimitKeyPrefix      = "AGENTLOOM_API_RATELIMIT_KEY_PREFIX"
	EnvAPIRateLimitSubmitCapacity = "AGENTLOOM_API_RATELIMIT_SUBMIT_CAPACITY"
	EnvAPIRateLimitSubmitRefill   = "AGENTLOOM_API_RATELIMIT_SUBMIT_REFILL_PER_SEC"
	EnvAPIRateLimitReadCapacity   = "AGENTLOOM_API_RATELIMIT_READ_CAPACITY"
	EnvAPIRateLimitReadRefill     = "AGENTLOOM_API_RATELIMIT_READ_REFILL_PER_SEC"
	EnvAPIRateLimitAdminCapacity  = "AGENTLOOM_API_RATELIMIT_ADMIN_CAPACITY"
	EnvAPIRateLimitAdminRefill    = "AGENTLOOM_API_RATELIMIT_ADMIN_REFILL_PER_SEC"
	EnvAPIRateLimitGlobalCapacity = "AGENTLOOM_API_RATELIMIT_GLOBAL_CAPACITY"
	EnvAPIRateLimitGlobalRefill   = "AGENTLOOM_API_RATELIMIT_GLOBAL_REFILL_PER_SEC"
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

// Rate-limit defaults (ticket 6.4, ADR-007): per-key class buckets with
// submits stricter than reads and admin/key-management strictest, plus the
// global safety bucket sized for a small fleet of clients. Capacity is the
// burst; refill is the sustained tokens/second. All dev-sane starting
// points, overridable per class via the env knobs above.
const (
	DefaultAPIRateLimitKeyPrefix = "ratelimit:api"

	DefaultAPIRateLimitSubmitCapacity = 20
	DefaultAPIRateLimitSubmitRefill   = 10
	DefaultAPIRateLimitReadCapacity   = 100
	DefaultAPIRateLimitReadRefill     = 50
	DefaultAPIRateLimitAdminCapacity  = 10
	DefaultAPIRateLimitAdminRefill    = 2
	DefaultAPIRateLimitGlobalCapacity = 500
	DefaultAPIRateLimitGlobalRefill   = 250
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
	// RootKey is the optional bootstrap admin credential (ticket 6.1,
	// ADR-007): a plaintext sk_-shaped key that authenticates as an
	// implicit admin for key management, meant to mint the first stored
	// admin key and then be unset. Empty disables the root path. It is a
	// secret: it must never be logged, and internal/api validates its
	// shape without ever echoing the value.
	RootKey string
	// RateLimit configures per-client API rate limiting (ticket 6.4).
	RateLimit APIRateLimitConfig
	// TestExecutors lists the full exec.Builtins catalog on
	// GET /v1/plugins instead of exec.CoreBuiltins (ticket 8.1, ADR-009)
	// — the API-side mirror of AGENTLOOM_WORKER_TEST_EXECUTORS, so a
	// deployment sets both knobs alike and the plugin listing matches
	// what the worker fleet executes. Default false, like the worker's.
	TestExecutors bool
}

// APIRateLimitClass is one token bucket's parameters: Capacity is the burst
// size, RefillPerSec the sustained rate. Both must be positive — a
// never-refilling API bucket would permanently brick a key (the library's
// rate-zero fixed quotas are an M9 shape, not a sane API limit).
type APIRateLimitClass struct {
	Capacity     int64
	RefillPerSec float64
}

// APIRateLimitConfig configures ticket 6.4's per-client rate limiting:
// per-key_id token buckets per route class (Submit/Read/Admin) plus one
// global safety bucket protecting the API even when every individual key is
// under its own limit. Buckets live in Redis via internal/ratelimit; the
// middleware fails open when Redis is unreachable (ADR-007), so enabling
// this never makes Redis a hard availability dependency of the API.
type APIRateLimitConfig struct {
	// Enabled turns the middleware on. When false, cmd/api opens no Redis
	// client at all.
	Enabled bool
	// KeyPrefix prefixes every bucket key (`<prefix>:<key_id>:<class>` and
	// `<prefix>:global`). The default suits production; tests override it
	// for per-test isolation, the same discipline as the queue key knobs.
	KeyPrefix string
	// Submit, Read, Admin are the per-key_id buckets for the three route
	// classes; Global is the API-wide safety bucket.
	Submit APIRateLimitClass
	Read   APIRateLimitClass
	Admin  APIRateLimitClass
	Global APIRateLimitClass
}

func defaultAPIConfig() APIConfig {
	return APIConfig{
		Addr:            DefaultAPIAddr,
		ReadTimeout:     DefaultAPIReadTimeout,
		WriteTimeout:    DefaultAPIWriteTimeout,
		IdleTimeout:     DefaultAPIIdleTimeout,
		ShutdownTimeout: DefaultAPIShutdownTimeout,
		RateLimit: APIRateLimitConfig{
			Enabled:   true,
			KeyPrefix: DefaultAPIRateLimitKeyPrefix,
			Submit:    APIRateLimitClass{Capacity: DefaultAPIRateLimitSubmitCapacity, RefillPerSec: DefaultAPIRateLimitSubmitRefill},
			Read:      APIRateLimitClass{Capacity: DefaultAPIRateLimitReadCapacity, RefillPerSec: DefaultAPIRateLimitReadRefill},
			Admin:     APIRateLimitClass{Capacity: DefaultAPIRateLimitAdminCapacity, RefillPerSec: DefaultAPIRateLimitAdminRefill},
			Global:    APIRateLimitClass{Capacity: DefaultAPIRateLimitGlobalCapacity, RefillPerSec: DefaultAPIRateLimitGlobalRefill},
		},
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
	if raw, ok := lookup(fn, EnvAPIRootKey); ok {
		// Passed through opaquely: shape validation happens in internal/api,
		// which knows the key format and is careful never to echo the value.
		c.RootKey = raw
	}
	errs = applyBool(errs, fn, EnvAPITestExecutors, &c.TestExecutors)
	errs = applyBool(errs, fn, EnvAPIRateLimitEnabled, &c.RateLimit.Enabled)
	applyString(fn, EnvAPIRateLimitKeyPrefix, &c.RateLimit.KeyPrefix)
	errs = applyPositiveInt64(errs, fn, EnvAPIRateLimitSubmitCapacity, &c.RateLimit.Submit.Capacity)
	errs = applyPositiveFloat(errs, fn, EnvAPIRateLimitSubmitRefill, &c.RateLimit.Submit.RefillPerSec)
	errs = applyPositiveInt64(errs, fn, EnvAPIRateLimitReadCapacity, &c.RateLimit.Read.Capacity)
	errs = applyPositiveFloat(errs, fn, EnvAPIRateLimitReadRefill, &c.RateLimit.Read.RefillPerSec)
	errs = applyPositiveInt64(errs, fn, EnvAPIRateLimitAdminCapacity, &c.RateLimit.Admin.Capacity)
	errs = applyPositiveFloat(errs, fn, EnvAPIRateLimitAdminRefill, &c.RateLimit.Admin.RefillPerSec)
	errs = applyPositiveInt64(errs, fn, EnvAPIRateLimitGlobalCapacity, &c.RateLimit.Global.Capacity)
	errs = applyPositiveFloat(errs, fn, EnvAPIRateLimitGlobalRefill, &c.RateLimit.Global.RefillPerSec)
	return errs
}

// applyPositiveInt64 is applyPositiveInt for int64 destinations (bucket
// capacities).
func applyPositiveInt64(errs []error, fn LookupFunc, key string, dst *int64) []error {
	raw, ok := lookup(fn, key)
	if !ok {
		return errs
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n <= 0 {
		return append(errs, fmt.Errorf("%s: invalid value %q (want a positive integer)", key, raw))
	}
	*dst = n
	return errs
}

// applyPositiveFloat overrides *dst from the environment when the variable
// is set, appending an error for a non-positive, non-finite, or
// unparseable value (bucket refill rates).
func applyPositiveFloat(errs []error, fn LookupFunc, key string, dst *float64) []error {
	raw, ok := lookup(fn, key)
	if !ok {
		return errs
	}
	f, err := strconv.ParseFloat(raw, 64)
	if err != nil || f <= 0 || math.IsInf(f, 0) || math.IsNaN(f) {
		return append(errs, fmt.Errorf("%s: invalid value %q (want a positive number)", key, raw))
	}
	*dst = f
	return errs
}
