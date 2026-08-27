package main

import (
	"fmt"
	"strings"
	"time"
)

const DefaultAlertThrottleInterval = 6 * time.Hour

var alertThrottleInterval = DefaultAlertThrottleInterval

func effectiveAlertThrottleInterval() time.Duration {
	return alertThrottleInterval
}

func applyAlertThrottleInterval(d time.Duration) {
	alertThrottleInterval = d
}

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
