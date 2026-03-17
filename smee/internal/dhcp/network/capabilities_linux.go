//go:build linux

package network

import "golang.org/x/sys/unix"

// hasNetAdminCapability reports whether the current process has the
// CAP_NET_ADMIN Linux capability in its effective set.
func hasNetAdminCapability() bool {
	hdr := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	var data [2]unix.CapUserData
	if err := unix.Capget(&hdr, &data[0]); err != nil {
		return false
	}
	const cap = unix.CAP_NET_ADMIN
	word := cap / 32
	bit := cap % 32
	if word >= 2 {
		return false
	}
	return data[word].Effective&(1<<bit) != 0
}
