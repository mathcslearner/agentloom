package config

import (
	"fmt"
	"time"
)

// Environment variables read by NotifyConfig.
const (
	EnvNotifyWebhookURL         = "AGENTLOOM_NOTIFY_WEBHOOK_URL"
	EnvNotifyWebhookSecret      = "AGENTLOOM_NOTIFY_WEBHOOK_SECRET"
	EnvNotifyWebhookTimeout     = "AGENTLOOM_NOTIFY_WEBHOOK_TIMEOUT"
	EnvNotifyWebhookMaxAttempts = "AGENTLOOM_NOTIFY_WEBHOOK_MAX_ATTEMPTS"
)

// Notification defaults (ticket 15.5, ADR-017). Mirror internal/notify's
// DefaultTimeout / DefaultMaxAttempts, kept local so config stays a leaf (it
// never imports notify).
const (
	defaultNotifyWebhookTimeout     = 5 * time.Second
	defaultNotifyWebhookMaxAttempts = 3
)

// NotifyConfig configures the optional approval-notification webhook (ticket
// 15.5, internal/notify). It is off by default: with no URL set, the worker
// wires no notifier and a parked human_approval step emits no notification.
// When enabled, a signed (HMAC) payload is POSTed on each new pending
// approval — best-effort, so a webhook failure never affects run correctness.
type NotifyConfig struct {
	// WebhookURL is the endpoint approval notifications are POSTed to. Empty
	// disables notifications. Must be http or https when set.
	WebhookURL string
	// WebhookSecret keys the HMAC signature. Required when WebhookURL is set —
	// an unsigned webhook cannot be authenticated by the receiver.
	WebhookSecret string
	// WebhookTimeout bounds a single delivery attempt.
	WebhookTimeout time.Duration
	// WebhookMaxAttempts is the total number of delivery attempts before a
	// notification is given up on (and a warning event recorded).
	WebhookMaxAttempts int
}

func defaultNotifyConfig() NotifyConfig {
	return NotifyConfig{
		WebhookURL:         "",
		WebhookSecret:      "",
		WebhookTimeout:     defaultNotifyWebhookTimeout,
		WebhookMaxAttempts: defaultNotifyWebhookMaxAttempts,
	}
}

// Enabled reports whether a webhook is configured.
func (c NotifyConfig) Enabled() bool { return c.WebhookURL != "" }

// applyEnv overrides fields from the environment, reporting one error per
// invalid variable. A URL without a secret (or a secret without a URL) is a
// misconfiguration: a webhook must be signable, and a secret with nowhere to
// send is almost certainly a mistake.
func (c *NotifyConfig) applyEnv(fn LookupFunc) []error {
	applyString(fn, EnvNotifyWebhookURL, &c.WebhookURL)
	applyString(fn, EnvNotifyWebhookSecret, &c.WebhookSecret)
	var errs []error
	errs = applyPositiveDuration(errs, fn, EnvNotifyWebhookTimeout, &c.WebhookTimeout)
	errs = applyPositiveInt(errs, fn, EnvNotifyWebhookMaxAttempts, &c.WebhookMaxAttempts)
	if c.WebhookURL != "" && c.WebhookSecret == "" {
		errs = append(errs, fmt.Errorf("%s is set but %s is empty (a webhook must be signable)", EnvNotifyWebhookURL, EnvNotifyWebhookSecret))
	}
	if c.WebhookURL == "" && c.WebhookSecret != "" {
		errs = append(errs, fmt.Errorf("%s is set but %s is empty (a secret with no webhook URL)", EnvNotifyWebhookSecret, EnvNotifyWebhookURL))
	}
	return errs
}
