//go:build !darwin

package version

// Translated always reports false off macOS: Rosetta 2 is a macOS-only
// translation layer.
func Translated() bool { return false }
