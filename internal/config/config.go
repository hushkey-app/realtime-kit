// Package config reads the sidecar's boot configuration from the environment.
//
// There is deliberately nothing about LiveKit here. This service holds no
// provider credentials of its own: every request carries the LiveKit URL, key
// and secret of the workspace it is acting for, so one process serves any
// number of tenants on any number of LiveKit deployments. What the environment
// configures is the *sidecar* — where it listens, who may call it, how long it
// waits on LiveKit.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	// Deliberately NOT 7880: that is LiveKit's own default port, and a
	// self-hosted LiveKit beside this service would collide with it.
	defaultAddr           = ":7890"
	defaultUpstreamWait   = 10 * time.Second
	defaultReadTimeout    = 15 * time.Second
	defaultWriteTimeout   = 30 * time.Second
	defaultShutdownGrace  = 10 * time.Second
	defaultMaxRequestSize = 64 << 10 // 64 KiB — every request here is small JSON.

	// The shared secret must be long enough that guessing it is not a strategy.
	// Callers pass provider credentials through this service, so a weak token is
	// a credential leak waiting to happen rather than a mere availability risk.
	minTokenLength = 32
)

// Config is the sidecar's boot configuration.
type Config struct {
	// Addr is the listen address, e.g. ":7890" or "127.0.0.1:7890".
	Addr string
	// Token is the shared secret callers present as `Authorization: Bearer …`.
	// Never logged.
	Token string
	// UpstreamTimeout bounds one call to LiveKit. The caller (pakku) is holding a
	// user's join open behind this, so it is short by design.
	UpstreamTimeout time.Duration
	ReadTimeout     time.Duration
	WriteTimeout    time.Duration
	ShutdownGrace   time.Duration
	MaxRequestSize  int64
	// Debug turns on debug-level logging. It never turns on credential logging:
	// there is no code path that writes a key, a secret or a minted token.
	Debug bool
}

// ErrNoToken is returned when REALTIME_KIT_TOKEN is unset. The service fails to
// boot rather than starting open: an unauthenticated instance would mint LiveKit
// tokens for anyone who can reach the port.
var ErrNoToken = errors.New(
	"REALTIME_KIT_TOKEN is required: refusing to start an unauthenticated token minter",
)

// Load reads the configuration from the environment, applying defaults.
func Load() (Config, error) {
	cfg := Config{
		Addr:            envString("REALTIME_KIT_ADDR", defaultAddr),
		Token:           strings.TrimSpace(os.Getenv("REALTIME_KIT_TOKEN")),
		UpstreamTimeout: envDuration("REALTIME_KIT_UPSTREAM_TIMEOUT", defaultUpstreamWait),
		ReadTimeout:     envDuration("REALTIME_KIT_READ_TIMEOUT", defaultReadTimeout),
		WriteTimeout:    envDuration("REALTIME_KIT_WRITE_TIMEOUT", defaultWriteTimeout),
		ShutdownGrace:   envDuration("REALTIME_KIT_SHUTDOWN_GRACE", defaultShutdownGrace),
		MaxRequestSize:  defaultMaxRequestSize,
		Debug:           os.Getenv("REALTIME_KIT_DEBUG") == "true",
	}

	if cfg.Token == "" {
		return Config{}, ErrNoToken
	}
	if len(cfg.Token) < minTokenLength {
		return Config{}, fmt.Errorf(
			"REALTIME_KIT_TOKEN must be at least %d characters", minTokenLength,
		)
	}
	return cfg, nil
}

func envString(name, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return fallback
}

// envDuration accepts either a Go duration ("5s", "1m") or a bare number of
// seconds, because operators write both. A value that is neither falls back to
// the default rather than failing the boot over a formatting slip.
func envDuration(name string, fallback time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	if d, err := time.ParseDuration(raw); err == nil && d > 0 {
		return d
	}
	if secs, err := strconv.Atoi(raw); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	return fallback
}
