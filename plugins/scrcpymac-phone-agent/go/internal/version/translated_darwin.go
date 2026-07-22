//go:build darwin

package version

import "syscall"

// Translated reports whether this process is running under Rosetta 2, i.e. an
// amd64 binary on an Apple Silicon host. The sysctl is the documented probe and
// needs no cgo; it is absent on genuinely Intel machines, which read as false.
func Translated() bool {
	v, err := syscall.SysctlUint32("sysctl.proc_translated")
	if err != nil {
		return false
	}
	return v == 1
}
