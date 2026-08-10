package config

import "time"

// Environment variables read by WorkerConfig.
const (
	EnvWorkerHealthInterval = "AGENTLOOM_WORKER_HEALTH_INTERVAL"
)

// DefaultWorkerHealthInterval spaces the worker's periodic health log —
// a liveness line with queue depth and PEL size. A minute keeps the log
// quiet while making a wedged worker visible within one scrape-ish
// interval; M7 replaces the numbers with real metrics.
const DefaultWorkerHealthInterval = time.Minute

// WorkerConfig configures the worker deployable (cmd/worker) beyond what
// the shared component configs already carry: Postgres via
// PostgresConfig, Redis via RedisConfig, queue tuning via QueueConfig.
type WorkerConfig struct {
	// HealthInterval is the period between health log lines (worker
	// liveness + queue stats). Must be positive.
	HealthInterval time.Duration
}

func defaultWorkerConfig() WorkerConfig {
	return WorkerConfig{
		HealthInterval: DefaultWorkerHealthInterval,
	}
}

// applyEnv overrides fields from the environment, returning one error per
// invalid variable.
func (c *WorkerConfig) applyEnv(fn LookupFunc) []error {
	var errs []error
	errs = applyPositiveDuration(errs, fn, EnvWorkerHealthInterval, &c.HealthInterval)
	return errs
}
