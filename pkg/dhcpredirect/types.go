package dhcpredirect

import (
	"net"
	"net/netip"
	"strings"
)

// DefaultHostNetNSPath is the host network namespace as seen from a pod that
// shares the host PID namespace. PID 1 is the host's init, so its network
// namespace is the host's.
const DefaultHostNetNSPath = "/proc/1/ns/net"

// Config selects the interfaces the redirect is set up between. The zero value
// auto-detects everything, which is what a pod started by the Helm chart uses.
type Config struct {
	// PhysicalInterface is the host interface that DHCP broadcasts arrive on.
	// Empty auto-detects the interface carrying the host's default IPv4 route.
	PhysicalInterface string

	// PodInterface is the pod interface that the DHCP server is reachable on.
	// Empty auto-detects the interface carrying the pod's default IPv4 route,
	// which for every common CNI is eth0.
	PodInterface string

	// HostNetNSPath is the path to the host network namespace.
	// Empty means [DefaultHostNetNSPath].
	HostNetNSPath string
}

// Info records what [Setup] resolved and how it attached, for logging and for
// tests to assert against.
type Info struct {
	// PhysicalInterface, PhysicalIndex, PhysicalAddr and PhysicalMAC describe
	// the host interface DHCP broadcasts arrive on. PhysicalAddr is invalid
	// when the interface has no IPv4 address, which leaves the source address
	// of replies as the pod's.
	PhysicalInterface string
	PhysicalIndex     int
	PhysicalAddr      netip.Addr
	PhysicalMAC       net.HardwareAddr

	// PodInterface, PodIndex and PodAddr describe the pod side of the veth.
	PodInterface string
	PodIndex     int
	PodAddr      netip.Addr

	// PeerInterface and PeerIndex describe the host side of the same veth,
	// which is the interface packets from the pod arrive on. With Cilium this
	// is an lxc* interface.
	PeerInterface string
	PeerIndex     int

	// Attach is "tcx" or "tc", the mechanism the programs were attached with.
	Attach string

	// FallbackPriority is the tc filter priority in use when Attach is "tc".
	// Anything above 1 means another filter is ahead of ours on the ingress
	// hook and may consume DHCP before we see it.
	FallbackPriority int

	// RedirectPeer reports whether packets are delivered into the pod with
	// bpf_redirect_peer, which skips the host side veth's egress hooks.
	RedirectPeer bool
}

// LogValues renders the resolved setup as alternating key/value pairs for logr.
func (i Info) LogValues() []any {
	values := []any{
		"physicalInterface", i.PhysicalInterface,
		"physicalAddr", addrString(i.PhysicalAddr),
		"physicalMAC", hardwareAddrString(i.PhysicalMAC),
		"podInterface", i.PodInterface,
		"podAddr", addrString(i.PodAddr),
		"peerInterface", i.PeerInterface,
		"attach", i.Attach,
		"redirectPeer", i.RedirectPeer,
	}
	if i.FallbackPriority != 0 {
		values = append(values, "priority", i.FallbackPriority)
	}
	return values
}

// Stats are the packet counters the eBPF programs keep, summed across CPUs.
type Stats struct {
	// ToPodMatched counts DHCP broadcasts seen on the physical interface, and
	// ToPodRedirected the subset pushed into the pod. They differ only while
	// the programs are attached but not yet configured. The inbound direction
	// has no error counter because it rewrites nothing and so cannot fail.
	ToPodMatched    uint64
	ToPodRedirected uint64

	// ToWireMatched counts DHCP replies seen leaving the pod, and
	// ToWireRedirected the subset pushed out of the physical interface.
	// ToWireError counts replies dropped because rewriting them failed.
	ToWireMatched    uint64
	ToWireRedirected uint64
	ToWireError      uint64

	// Unconfigured counts packets that matched but found no usable
	// configuration in the map. It should never move after [Setup] returns.
	Unconfigured uint64
}

// LogValues renders the counters as alternating key/value pairs for logr.
func (s Stats) LogValues() []any {
	return []any{
		"toPodMatched", s.ToPodMatched,
		"toPodRedirected", s.ToPodRedirected,
		"toWireMatched", s.ToWireMatched,
		"toWireRedirected", s.ToWireRedirected,
		"toWireError", s.ToWireError,
		"unconfigured", s.Unconfigured,
	}
}

func addrString(addr netip.Addr) string {
	if !addr.IsValid() {
		return ""
	}
	return addr.String()
}

func hardwareAddrString(mac net.HardwareAddr) string {
	if len(mac) == 0 {
		return ""
	}
	return strings.ToLower(mac.String())
}
