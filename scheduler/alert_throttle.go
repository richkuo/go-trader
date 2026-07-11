package main

import (
	"fmt"
	"strings"
	"time"
)

// DefaultAlertThrottleInterval is the fleet-wide re-alert back-off when
// alert_throttle_interval is omitted from config.json.
const DefaultAlertThrottleInterval = 6 * time.Hour

// alertThrottleInterval is the active re-alert back-off for all operator alert
// throttles. Set from config at load and on SIGHUP hot-reload.
var alertThrottleInterval = DefaultAlertThrottleInterval

func effectiveAlertThrottleInterval() time.Duration {
	return alertThrottleInterval
}

func applyAlertThrottleInterval(d time.Duration) {
	alertThrottleInterval = d
}

// applyAlertThrottleFromConfig adopts cfg's alert_throttle_interval into the
// live runtime. Call only when a config is actually adopted (daemon startup or
// an accepted SIGHUP reload) — never from loadConfig/LoadConfigForProbe.
func applyAlertThrottleFromConfig(cfg *Config) error {
	if cfg == nil {
		return nil
	}
	d, err := ParseAlertThrottleInterval(cfg.AlertThrottleInterval)
	if err != nil {
		return err
	}
	applyAlertThrottleInterval(d)
	return nil
}

// ParseAlertThrottleInterval converts alert_throttle_interval config text to a
// duration. Empty/missing → DefaultAlertThrottleInterval (6h).
func ParseAlertThrottleInterval(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return DefaultAlertThrottleInterval, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid alert_throttle_interval %q: %w", s, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("alert_throttle_interval must be > 0, got %s", s)
	}
	return d, nil
}
