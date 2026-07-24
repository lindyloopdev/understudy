//go:build unix

package understudy

import "testing"

// TestReadFDSoftLimitSmoke is a smoke test, not a behavior spec: it confirms the
// unix adapter reaches RLIMIT_NOFILE and reports a plausible soft limit, guarding
// against a wire-up regression (wrong resource, wrong field). The exact value is
// host-dependent, so it asserts only structural invariants.
func TestReadFDSoftLimitSmoke(t *testing.T) {
	t.Parallel()

	soft, ok := readFDSoftLimit()
	if !ok {
		t.Fatal("readFDSoftLimit reported the soft limit unavailable on a unix host")
	}
	if soft == 0 {
		t.Error("readFDSoftLimit reported a zero soft limit")
	}
}
