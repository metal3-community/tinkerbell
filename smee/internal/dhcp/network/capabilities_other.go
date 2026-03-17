//go:build !linux

package network

// hasNetAdminCapability on non-Linux platforms always returns true so that
// development builds on macOS/Windows are not blocked by the privilege check.
// The check is only meaningful when running on Linux (production).
func hasNetAdminCapability() bool {
	return true
}
