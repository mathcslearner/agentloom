// Package config loads agentloom configuration from the environment.
//
// Every setting has a default and may be overridden by an environment
// variable named AGENTLOOM_<COMPONENT>_<FIELD> (e.g. AGENTLOOM_LOG_LEVEL).
// A variable that is unset or set to the empty string leaves the default in
// place. Loading fails fast: every invalid value is reported (all of them,
// joined into one error), and no partially-valid Config is ever returned.
//
// The environment is injected as a lookup function so tests can supply a
// plain map instead of mutating the process environment.
package config

import (
	"errors"
	"fmt"
	"os"
)

// LookupFunc reports the value of an environment variable and whether it is
// set. os.LookupEnv satisfies this signature.
type LookupFunc func(key string) (string, bool)

// Config is the root configuration for agentloom binaries, composed of one
// typed sub-config per component.
type Config struct {
	Log      LogConfig
	Postgres PostgresConfig
	Redis    RedisConfig
	Queue    QueueConfig
	Worker   WorkerConfig
	API      APIConfig
	Obs      ObsConfig
	LLM      LLMConfig
}

// Load builds a Config by applying environment overrides from lookup on top
// of the defaults. It returns an error joining every invalid value found;
// the returned Config is the zero value in that case.
func Load(lookup LookupFunc) (Config, error) {
	cfg := Config{
		Log:      defaultLogConfig(),
		Postgres: defaultPostgresConfig(),
		Redis:    defaultRedisConfig(),
		Queue:    defaultQueueConfig(),
		Worker:   defaultWorkerConfig(),
		API:      defaultAPIConfig(),
		Obs:      defaultObsConfig(),
		LLM:      defaultLLMConfig(),
	}
	var errs []error
	errs = append(errs, cfg.Log.applyEnv(lookup)...)
	errs = append(errs, cfg.Postgres.applyEnv(lookup)...)
	errs = append(errs, cfg.Redis.applyEnv(lookup)...)
	errs = append(errs, cfg.Queue.applyEnv(lookup)...)
	errs = append(errs, cfg.Worker.applyEnv(lookup)...)
	errs = append(errs, cfg.API.applyEnv(lookup)...)
	errs = append(errs, cfg.Obs.applyEnv(lookup)...)
	errs = append(errs, cfg.LLM.applyEnv(lookup)...)
	if err := errors.Join(errs...); err != nil {
		return Config{}, fmt.Errorf("invalid configuration:\n%w", err)
	}
	return cfg, nil
}

// LoadFromEnv is Load backed by the process environment.
func LoadFromEnv() (Config, error) {
	return Load(os.LookupEnv)
}

// lookup returns the value of key if it is set to a non-empty string.
func lookup(fn LookupFunc, key string) (string, bool) {
	v, ok := fn(key)
	if !ok || v == "" {
		return "", false
	}
	return v, true
}
