package config

import (
	"errors"
	"testing"
	"time"
)

const goodToken = "a-thirty-two-character-shared-secret-value"

// Fail closed: an unauthenticated instance would mint LiveKit tokens for anyone
// who can reach the port, so there is no "no token, no auth" mode.
func TestLoadRequiresAToken(t *testing.T) {
	t.Setenv("REALTIME_KIT_TOKEN", "")

	_, err := Load()
	if !errors.Is(err, ErrNoToken) {
		t.Fatalf("expected ErrNoToken, got %v", err)
	}
}

func TestLoadRejectsAShortToken(t *testing.T) {
	t.Setenv("REALTIME_KIT_TOKEN", "too-short")

	if _, err := Load(); err == nil {
		t.Fatal("a guessable token was accepted")
	}
}

func TestLoadDefaults(t *testing.T) {
	t.Setenv("REALTIME_KIT_TOKEN", goodToken)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Addr != defaultAddr {
		t.Fatalf("addr = %q, want %q", cfg.Addr, defaultAddr)
	}
	if cfg.UpstreamTimeout != defaultUpstreamWait {
		t.Fatalf("upstream timeout = %s, want %s", cfg.UpstreamTimeout, defaultUpstreamWait)
	}
	if cfg.Debug {
		t.Fatal("debug should be off by default")
	}
}

// Operators write both "5s" and "5"; a value that is neither must not take the
// service down over a formatting slip.
func TestDurationsAcceptBothFormsAndFallBack(t *testing.T) {
	t.Setenv("REALTIME_KIT_TOKEN", goodToken)

	for _, tc := range []struct {
		raw  string
		want time.Duration
	}{
		{"5s", 5 * time.Second},
		{"5", 5 * time.Second},
		{"2m", 2 * time.Minute},
		{"nonsense", defaultUpstreamWait},
		{"-3", defaultUpstreamWait},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			t.Setenv("REALTIME_KIT_UPSTREAM_TIMEOUT", tc.raw)
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.UpstreamTimeout != tc.want {
				t.Fatalf("upstream timeout = %s, want %s", cfg.UpstreamTimeout, tc.want)
			}
		})
	}
}
