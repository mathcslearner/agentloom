package config

import (
	"testing"
	"time"
)

// lookupMap adapts a map to LookupFunc for the internal-package tests.
func lookupMap(env map[string]string) LookupFunc {
	return func(key string) (string, bool) { v, ok := env[key]; return v, ok }
}

func TestNotifyConfigDefaults(t *testing.T) {
	c := defaultNotifyConfig()
	if c.Enabled() {
		t.Error("notifications must be disabled by default (no webhook URL)")
	}
	if c.WebhookTimeout != defaultNotifyWebhookTimeout || c.WebhookMaxAttempts != defaultNotifyWebhookMaxAttempts {
		t.Errorf("defaults = %+v", c)
	}
}

func TestNotifyConfigApplyEnv(t *testing.T) {
	env := map[string]string{
		EnvNotifyWebhookURL:         "https://hooks.example.com/approvals",
		EnvNotifyWebhookSecret:      "shh",
		EnvNotifyWebhookTimeout:     "8s",
		EnvNotifyWebhookMaxAttempts: "5",
	}
	c := defaultNotifyConfig()
	if errs := c.applyEnv(lookupMap(env)); len(errs) != 0 {
		t.Fatalf("applyEnv errors: %v", errs)
	}
	if !c.Enabled() || c.WebhookURL != env[EnvNotifyWebhookURL] || c.WebhookSecret != "shh" {
		t.Errorf("config = %+v", c)
	}
	if c.WebhookTimeout != 8*time.Second || c.WebhookMaxAttempts != 5 {
		t.Errorf("config = %+v", c)
	}
}

func TestNotifyConfigURLWithoutSecret(t *testing.T) {
	c := defaultNotifyConfig()
	errs := c.applyEnv(lookupMap(map[string]string{EnvNotifyWebhookURL: "https://x/y"}))
	if len(errs) == 0 {
		t.Fatal("a URL without a secret must be a config error")
	}
}

func TestNotifyConfigSecretWithoutURL(t *testing.T) {
	c := defaultNotifyConfig()
	errs := c.applyEnv(lookupMap(map[string]string{EnvNotifyWebhookSecret: "shh"}))
	if len(errs) == 0 {
		t.Fatal("a secret without a URL must be a config error")
	}
}

func TestNotifyConfigBadMaxAttempts(t *testing.T) {
	c := defaultNotifyConfig()
	errs := c.applyEnv(lookupMap(map[string]string{
		EnvNotifyWebhookURL: "https://x/y", EnvNotifyWebhookSecret: "s",
		EnvNotifyWebhookMaxAttempts: "0",
	}))
	if len(errs) == 0 {
		t.Fatal("max_attempts=0 must be a config error (applyPositiveInt)")
	}
}
