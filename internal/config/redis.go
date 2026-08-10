package config

// Environment variables read by RedisConfig.
const (
	EnvRedisAddr = "AGENTLOOM_REDIS_ADDR"
)

// DefaultRedisAddr matches the docker-compose.yml dev stack. The dev stack
// runs Redis without authentication; deployed environments layer credentials
// in via their own configuration (M18).
const DefaultRedisAddr = "localhost:6379"

// RedisConfig configures the connection to Redis, the redeliverable
// transport and coordination layer (streams/leases, delayed delivery, rate
// limits, cache, pub/sub — ADR-005).
type RedisConfig struct {
	// Addr is the host:port of the Redis server.
	Addr string
}

func defaultRedisConfig() RedisConfig {
	return RedisConfig{
		Addr: DefaultRedisAddr,
	}
}

// applyEnv overrides fields from the environment, returning one error per
// invalid variable. The address is passed through opaquely — malformed
// values surface at dial time, where the client produces the better error —
// so today this never fails; the signature matches the other sub-configs.
//
//nolint:unparam // see above: uniform sub-config signature
func (c *RedisConfig) applyEnv(fn LookupFunc) []error {
	if raw, ok := lookup(fn, EnvRedisAddr); ok {
		c.Addr = raw
	}
	return nil
}
