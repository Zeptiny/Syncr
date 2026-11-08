package config

import (
	"errors"
	"fmt"
	"time"

	"github.com/Zeptiny/Syncr/internal/platform/netguard"
)

// Schedules configures the scheduler (internal/controller/scheduler).
type Schedules struct {
	// Tick is the time between two scheduler passes when nothing wakes it
	// earlier (saving a schedule, Run now and finished jobs do). It bounds
	// how late an occurrence fires and how long a run waits after its
	// worker came back (default 10s).
	Tick time.Duration `yaml:"tick" env:"SYNCR_SCHEDULES_TICK"`
}

// Limits of schedules.tick.
const (
	minSchedulesTick = time.Second
	maxSchedulesTick = 5 * time.Minute
)

func (s Schedules) validate() error {
	if s.Tick < minSchedulesTick || s.Tick > maxSchedulesTick {
		return fmt.Errorf("schedules.tick must be between %v and %v", minSchedulesTick, maxSchedulesTick)
	}
	return nil
}

// Notify configures notifications: the outbox that delivers them
// (internal/app/notifications) and the network policy of their targets
// (internal/platform/netguard). Links in messages use http.public_url.
type Notify struct {
	// NetworkPolicy decides which addresses notification targets (Discord,
	// webhook and SMTP servers) may connect to: "private" (the default:
	// loopback, link-local, cloud metadata and other special addresses are
	// denied, LAN addresses are allowed), "public" (private networks are
	// denied too) or "any" (nothing is denied). The connected address is
	// checked, so DNS names cannot get around it. Proxies are not used.
	NetworkPolicy string `yaml:"network_policy" env:"SYNCR_NOTIFY_NETWORK_POLICY"`
	// AllowedNetworks are CIDRs or single IPs that are allowed whatever
	// NetworkPolicy denies, e.g. "127.0.0.1" for a relay on the
	// controller's host (comma-separated in the environment).
	AllowedNetworks []string `yaml:"allowed_networks" env:"SYNCR_NOTIFY_ALLOWED_NETWORKS"`
	// MaxAttempts is how often a delivery is tried before it is dead
	// (default 8: retries back off from 30 s to an hour, about an hour in
	// all).
	MaxAttempts int `yaml:"max_attempts" env:"SYNCR_NOTIFY_MAX_ATTEMPTS"`
	// Concurrency is how many deliveries are sent at once (default 4).
	Concurrency int `yaml:"concurrency" env:"SYNCR_NOTIFY_CONCURRENCY"`
	// SendTimeout bounds one send, and a contact's test send (default
	// 15s).
	SendTimeout time.Duration `yaml:"send_timeout" env:"SYNCR_NOTIFY_SEND_TIMEOUT"`
	// Retention is how long sent and dead deliveries are kept in the
	// delivery log (default 720h, 30 days).
	Retention time.Duration `yaml:"retention" env:"SYNCR_NOTIFY_RETENTION"`
}

// Limits of the notify settings. A send must end well within the
// delivery lease (2 minutes), or another pass could send it again.
const (
	maxNotifyAttempts    = 50
	maxNotifyConcurrency = 64
	minNotifySendTimeout = time.Second
	maxNotifySendTimeout = time.Minute
	minNotifyRetention   = time.Hour
)

// Policy returns the network policy of notification targets.
func (n Notify) Policy() (netguard.Policy, error) {
	mode, err := netguard.ParseMode(n.NetworkPolicy)
	if err != nil {
		return netguard.Policy{}, fmt.Errorf("notify.network_policy: %w", err)
	}
	allow, err := netguard.ParsePrefixes(n.AllowedNetworks)
	if err != nil {
		return netguard.Policy{}, fmt.Errorf("notify.allowed_networks: %w", err)
	}
	return netguard.Policy{Mode: mode, Allow: allow}, nil
}

func (n Notify) validate() error {
	var errs []error
	if _, err := n.Policy(); err != nil {
		errs = append(errs, err)
	}
	if n.MaxAttempts < 1 || n.MaxAttempts > maxNotifyAttempts {
		errs = append(errs, fmt.Errorf("notify.max_attempts must be between 1 and %d", maxNotifyAttempts))
	}
	if n.Concurrency < 1 || n.Concurrency > maxNotifyConcurrency {
		errs = append(errs, fmt.Errorf("notify.concurrency must be between 1 and %d", maxNotifyConcurrency))
	}
	if n.SendTimeout < minNotifySendTimeout || n.SendTimeout > maxNotifySendTimeout {
		errs = append(errs, fmt.Errorf("notify.send_timeout must be between %v and %v", minNotifySendTimeout, maxNotifySendTimeout))
	}
	if n.Retention < minNotifyRetention {
		errs = append(errs, fmt.Errorf("notify.retention must be at least %v", minNotifyRetention))
	}
	return errors.Join(errs...)
}

// defaultNotify returns the default notify settings.
func defaultNotify() Notify {
	return Notify{
		NetworkPolicy: string(netguard.ModePrivate),
		MaxAttempts:   8,
		Concurrency:   4,
		SendTimeout:   15 * time.Second,
		Retention:     30 * 24 * time.Hour,
	}
}
