package config

// Environment variables read by PostgresConfig.
const (
	EnvPostgresDSN = "AGENTLOOM_POSTGRES_DSN"
)

// DefaultPostgresDSN matches the docker-compose.yml dev-stack defaults. It is
// a throwaway local-dev credential (same values the compose file inlines),
// not a secret; deployed environments must always override it.
const DefaultPostgresDSN = "postgres://agentloom:agentloom@localhost:5432/agentloom?sslmode=disable" //nolint:gosec // G101: dev-stack default, not a secret

// PostgresConfig configures the connection to Postgres, the source of truth
// for all durable state.
type PostgresConfig struct {
	// DSN is the connection string, in URL or keyword/value form.
	DSN string
}

func defaultPostgresConfig() PostgresConfig {
	return PostgresConfig{
		DSN: DefaultPostgresDSN,
	}
}

// applyEnv overrides fields from the environment, returning one error per
// invalid variable. The DSN is passed through opaquely — malformed values
// surface at connect time, where the driver produces the better error — so
// today this never fails; the signature matches the other sub-configs.
//
//nolint:unparam // see above: uniform sub-config signature
func (c *PostgresConfig) applyEnv(fn LookupFunc) []error {
	if raw, ok := lookup(fn, EnvPostgresDSN); ok {
		c.DSN = raw
	}
	return nil
}
