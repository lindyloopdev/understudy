//go:build unix

package understudy

import "syscall"

// readFDSoftLimit reports the process's soft RLIMIT_NOFILE. The bool is false
// when the limit cannot be read, so the caller can size the budget from
// defaultFDSoftLimitFallback instead.
func readFDSoftLimit() (uint64, bool) {
	var rlim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &rlim); err != nil {
		return 0, false
	}
	return rlim.Cur, true
}
