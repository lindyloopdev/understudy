//go:build !unix

package understudy

// readFDSoftLimit reports the FD soft limit as unavailable on platforms without
// RLIMIT_NOFILE (Windows, Plan 9, WebAssembly), so the caller sizes the budget
// from defaultFDSoftLimitFallback.
func readFDSoftLimit() (uint64, bool) {
	return 0, false
}
