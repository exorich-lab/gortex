package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestSocketPathClampFallback proves the AF_UNIX length clamp: a short path
// passes through, an over-long one is replaced by a short, stable, distinct
// temp-dir socket.
func TestSocketPathClampFallback(t *testing.T) {
	short := filepath.Join(os.TempDir(), "gortex.sock")
	if got := clampSocketPath(short); got != short {
		t.Errorf("a short path must pass through unchanged; got %q", got)
	}

	long := "/" + strings.Repeat("a", 200) + "/daemon.sock"
	got := clampSocketPath(long)
	if len(got) >= socketAddrMax() {
		t.Errorf("clamped path still too long (%d ≥ %d): %q", len(got), socketAddrMax(), got)
	}
	if !strings.HasSuffix(got, ".sock") {
		t.Errorf("the fallback must be a .sock path; got %q", got)
	}
	if clampSocketPath(long) != got {
		t.Error("the clamp must be deterministic (same input → same socket)")
	}
	long2 := "/" + strings.Repeat("b", 200) + "/daemon.sock"
	if clampSocketPath(long2) == got {
		t.Error("different over-long paths must map to distinct sockets")
	}
}

// TestIdleTimeoutFromEnvParse covers the idle-timeout parsing: default-on
// (DefaultIdleTimeout), an explicit positive duration overrides, an explicit
// non-positive value disables, and garbage falls back to the default rather
// than silently turning the mechanism off.
func TestIdleTimeoutFromEnvParse(t *testing.T) {
	t.Setenv("GORTEX_DAEMON_IDLE_TIMEOUT", "")
	if IdleTimeoutFromEnv() != DefaultIdleTimeout {
		t.Error("unset/empty must fall back to DefaultIdleTimeout")
	}
	t.Setenv("GORTEX_DAEMON_IDLE_TIMEOUT", "garbage")
	if IdleTimeoutFromEnv() != DefaultIdleTimeout {
		t.Error("an unparseable value must fall back to DefaultIdleTimeout")
	}
	t.Setenv("GORTEX_DAEMON_IDLE_TIMEOUT", "0")
	if IdleTimeoutFromEnv() != 0 {
		t.Error("an explicit 0 must disable the idle timeout")
	}
	t.Setenv("GORTEX_DAEMON_IDLE_TIMEOUT", "-5m")
	if IdleTimeoutFromEnv() != 0 {
		t.Error("a non-positive duration must disable the idle timeout")
	}
	t.Setenv("GORTEX_DAEMON_IDLE_TIMEOUT", "45m")
	if got := IdleTimeoutFromEnv(); got != 45*time.Minute {
		t.Errorf("IdleTimeoutFromEnv(45m) = %v, want 45m", got)
	}
}
