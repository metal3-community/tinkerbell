//go:build linux

package dhcpredirect

import (
	"net"
	"net/netip"
	"testing"
)

func TestWireOrderFromAddr(t *testing.T) {
	tests := map[string]struct {
		addr netip.Addr
		want uint32
	}{
		// The eBPF side compares this against the address bytes as they sit in
		// the packet, so the first byte of the address has to end up in the low
		// bits on a little-endian host.
		"typical":        {addr: netip.MustParseAddr("10.244.0.5"), want: 0x0500f40a},
		"broadcast":      {addr: netip.MustParseAddr("255.255.255.255"), want: 0xffffffff},
		"zero":           {addr: netip.MustParseAddr("0.0.0.0"), want: 0},
		"invalid is 0":   {addr: netip.Addr{}, want: 0},
		"ipv6 is 0":      {addr: netip.MustParseAddr("2001:db8::1"), want: 0},
		"v4-in-v6 is v4": {addr: netip.MustParseAddr("::ffff:192.0.2.1"), want: 0x010200c0},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if got := wireOrderFromAddr(tt.addr); got != tt.want {
				t.Fatalf("wireOrderFromAddr(%v) = %#08x, want %#08x", tt.addr, got, tt.want)
			}
		})
	}
}

// The counter slots are array indexes shared with the C source. A mismatch
// silently reports one counter's value under another's name, so pin them.
func TestStatCountMatchesTheCSource(t *testing.T) {
	want := map[string]int{
		"STAT_TO_POD_MATCHED":     statToPodMatched,
		"STAT_TO_POD_REDIRECTED":  statToPodRedirected,
		"STAT_TO_WIRE_MATCHED":    statToWireMatched,
		"STAT_TO_WIRE_REDIRECTED": statToWireRedirected,
		"STAT_TO_WIRE_ERROR":      statToWireError,
		"STAT_UNCONFIGURED":       statUnconfigured,
		"STAT_OTHER_SERVER_REPLY": statOtherServerReply,
	}
	for i, name := range []string{
		"STAT_TO_POD_MATCHED", "STAT_TO_POD_REDIRECTED",
		"STAT_TO_WIRE_MATCHED", "STAT_TO_WIRE_REDIRECTED", "STAT_TO_WIRE_ERROR",
		"STAT_UNCONFIGURED", "STAT_OTHER_SERVER_REPLY",
	} {
		if want[name] != i {
			t.Errorf("%s = %d, want %d", name, want[name], i)
		}
	}
	if statCount != len(want) {
		t.Errorf("statCount = %d, want %d", statCount, len(want))
	}
}

func TestStatsLogValues(t *testing.T) {
	s := Stats{ToPodMatched: 1, ToPodRedirected: 2, ToWireMatched: 4, ToWireRedirected: 5, ToWireError: 6, Unconfigured: 7}
	values := s.LogValues()
	if len(values)%2 != 0 {
		t.Fatalf("LogValues() returned %d values, which cannot be key/value pairs", len(values))
	}
	for i := 0; i < len(values); i += 2 {
		if _, ok := values[i].(string); !ok {
			t.Fatalf("LogValues()[%d] = %v, want a string key", i, values[i])
		}
	}
}

func TestInfoLogValues(t *testing.T) {
	i := Info{
		PhysicalInterface: "eth0",
		PhysicalAddr:      netip.MustParseAddr("192.0.2.1"),
		PhysicalMAC:       net.HardwareAddr{0xde, 0xad, 0xbe, 0xef, 0x00, 0x01},
		PodInterface:      "eth0",
		PodAddr:           netip.MustParseAddr("10.244.0.5"),
		PeerInterface:     "lxc1234",
		Attach:            "tcx",
	}
	values := i.LogValues()
	if len(values)%2 != 0 {
		t.Fatalf("LogValues() returned %d values, which cannot be key/value pairs", len(values))
	}

	// A missing priority must not appear at all: printing "priority 0" would
	// read as "first on the hook", which is the opposite of what 0 means here.
	for _, v := range values {
		if v == "priority" {
			t.Fatal("LogValues() reported a priority for a TCX attachment, which has none")
		}
	}

	i.Attach = "tc"
	i.FallbackPriority = 3
	if !containsKey(i.LogValues(), "priority") {
		t.Fatal("LogValues() omitted the priority of a classic tc attachment")
	}
}

// An empty MAC or address must render as an empty string rather than as Go's
// zero value spelling, which in logs reads like a real address.
func TestInfoLogValuesRendersMissingFieldsEmpty(t *testing.T) {
	values := Info{}.LogValues()
	for i := 0; i < len(values); i += 2 {
		key, _ := values[i].(string)
		switch key {
		case "physicalAddr", "podAddr", "physicalMAC":
			if values[i+1] != "" {
				t.Errorf("%s = %q, want an empty string", key, values[i+1])
			}
		}
	}
}

func containsKey(values []any, key string) bool {
	for i := 0; i < len(values); i += 2 {
		if values[i] == key {
			return true
		}
	}
	return false
}

// DefaultRouteInterface has to agree with the kernel about which interface it
// named; a name that does not resolve would send Setup looking for the wrong
// link. A machine with no default route is a legitimate result.
func TestDefaultRouteInterface(t *testing.T) {
	name, err := DefaultRouteInterface()
	if err != nil {
		t.Skipf("no default IPv4 route in this network namespace: %v", err)
	}
	if name == "" {
		t.Fatal("DefaultRouteInterface() returned an empty name and no error")
	}
	if _, err := net.InterfaceByName(name); err != nil {
		t.Fatalf("DefaultRouteInterface() = %q, which does not resolve: %v", name, err)
	}
}
