//go:build linux

package main

import (
	"os"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"github.com/vishvananda/netlink"
)

func TestMacvlanIfaceName(t *testing.T) {
	tests := map[string]struct {
		uid     string
		want    string
		wantErr bool
	}{
		"typical pod UID":       {uid: "550e8400-e29b-41d4-a716-446655440000", want: "mv550e8400e29b4"},
		"dashes are stripped":   {uid: "5-5-0-e-8-4-0-0-e-2-9-b-4", want: "mv550e8400e29b4"},
		"exactly 13 hex chars":  {uid: "550e8400-e29b4", want: "mv550e8400e29b4"},
		"unset":                 {uid: "", wantErr: true},
		"too short after strip": {uid: "550e-8400", wantErr: true},
		"dashes only":           {uid: "-------------", wantErr: true},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Setenv(podUIDEnv, tt.uid)
			if tt.uid == "" {
				// t.Setenv cannot unset, so remove it explicitly.
				t.Setenv(podUIDEnv, "")
			}

			got, err := macvlanIfaceName()
			if tt.wantErr {
				if err == nil {
					t.Fatalf("macvlanIfaceName() = %q, want error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("macvlanIfaceName() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("macvlanIfaceName() = %q, want %q", got, tt.want)
			}
		})
	}
}

// The generated name must match the pattern purgeStaleMacvlans uses to find and
// delete orphans; a mismatch would leak an interface in the host netns on every
// restart.
func TestMacvlanIfaceNameMatchesPurgePattern(t *testing.T) {
	t.Setenv(podUIDEnv, "550e8400-e29b-41d4-a716-446655440000")

	got, err := macvlanIfaceName()
	if err != nil {
		t.Fatalf("macvlanIfaceName() error = %v", err)
	}
	if !macvlanNameRe.MatchString(got) {
		t.Fatalf("macvlanIfaceName() = %q, which macvlanNameRe does not match", got)
	}
	// Linux caps interface names at IFNAMSIZ-1 = 15 bytes.
	if len(got) > 15 {
		t.Fatalf("macvlanIfaceName() = %q is %d bytes, over the 15 byte limit", got, len(got))
	}
}

// TestSetIPv4DevconfAppliesOverNetlink exercises the real netlink message
// against a real interface. The message format is easy to get subtly wrong —
// omitting NLA_F_NESTED, for instance, makes the kernel reject it with EINVAL —
// and nothing else in the test suite would catch that.
//
// Skipped unless the process can create a network interface, so it is a no-op
// in an ordinary unprivileged test run and does real work under `unshare -rn`.
func TestSetIPv4DevconfAppliesOverNetlink(t *testing.T) {
	const name = "tinkdevconf0"

	if err := netlink.LinkAdd(&netlink.Dummy{
		LinkAttrs: netlink.LinkAttrs{Name: name},
	}); err != nil {
		t.Skipf("cannot create a test interface (needs CAP_NET_ADMIN in a private netns): %v", err)
	}
	link, err := netlink.LinkByName(name)
	if err != nil {
		t.Fatalf("LinkByName(%q) error = %v", name, err)
	}
	t.Cleanup(func() { _ = netlink.LinkDel(link) })

	if err := setIPv4Devconf(link, macvlanDevconf); err != nil {
		t.Fatalf("setIPv4Devconf() error = %v", err)
	}

	// Read the result back through /proc/sys, which is the same devconf the
	// netlink attributes address.
	for path, want := range map[string]string{
		"arp_ignore":       "8",
		"arp_announce":     "2",
		"accept_redirects": "0",
		"send_redirects":   "0",
	} {
		full := "/proc/sys/net/ipv4/conf/" + name + "/" + path
		b, err := os.ReadFile(full)
		if err != nil {
			t.Errorf("reading %s: %v", full, err)
			continue
		}
		if got := strings.TrimSpace(string(b)); got != want {
			t.Errorf("%s = %q, want %q", full, got, want)
		}
	}
}

// TestHardenMacvlanAppliesIPv4WithoutWritableProcfs is the check behind running
// unprivileged: the ARP settings must still be applied when /proc/sys is
// read-only, and an unwritable IPv6 knob must not stop startup.
//
// Run it under `unshare -rnmpf --mount-proc` with /proc remounted read-only to
// exercise the unprivileged-container case; otherwise it still verifies that
// hardening succeeds and that the IPv4 half took effect.
func TestHardenMacvlanAppliesIPv4WithoutWritableProcfs(t *testing.T) {
	const name = "mv0123456789abc"

	if !macvlanNameRe.MatchString(name) {
		t.Fatalf("test interface name %q does not match the expected pattern", name)
	}
	if err := netlink.LinkAdd(&netlink.Dummy{
		LinkAttrs: netlink.LinkAttrs{Name: name},
	}); err != nil {
		t.Skipf("cannot create a test interface (needs CAP_NET_ADMIN in a private netns): %v", err)
	}
	link, err := netlink.LinkByName(name)
	if err != nil {
		t.Fatalf("LinkByName(%q) error = %v", name, err)
	}
	t.Cleanup(func() { _ = netlink.LinkDel(link) })

	var logged strings.Builder
	log := funcr.New(func(prefix, args string) {
		logged.WriteString(prefix + args + "\n")
	}, funcr.Options{Verbosity: 1})

	if err := hardenMacvlan(log, link, name); err != nil {
		t.Fatalf("hardenMacvlan() error = %v, want nil even when /proc/sys is read-only", err)
	}

	got, err := os.ReadFile("/proc/sys/net/ipv4/conf/" + name + "/arp_ignore")
	if err != nil {
		t.Fatalf("reading arp_ignore: %v", err)
	}
	if strings.TrimSpace(string(got)) != "8" {
		t.Fatalf("arp_ignore = %q, want \"8\"; the netlink path did not take effect", strings.TrimSpace(string(got)))
	}

	// When the IPv6 knobs could not be written, the operator must be told what
	// is left unprotected rather than the interface coming up quietly.
	ipv6, err := os.ReadFile("/proc/sys/net/ipv6/conf/" + name + "/disable_ipv6")
	if err != nil {
		t.Skipf("no IPv6 support on this kernel: %v", err)
	}
	if strings.TrimSpace(string(ipv6)) == "1" {
		return // procfs was writable, so there is nothing to warn about
	}
	if !strings.Contains(logged.String(), "router advertisement") {
		t.Fatalf("hardenMacvlan() did not warn that IPv6 is unprotected; logged:\n%s", logged.String())
	}
}

func TestHardenMacvlanRejectsUnexpectedName(t *testing.T) {
	// The name is interpolated into a /proc/sys path, so anything that is not a
	// name this binary generated must be refused rather than written through.
	for _, name := range []string{"", "eth0", "../../../etc/passwd", "mv550e8400e29b/../all", "MV550E8400E29B"} {
		if err := hardenMacvlan(logr.Discard(), nil, name); err == nil {
			t.Fatalf("hardenMacvlan(%q) = nil, want error", name)
		}
	}
}

func TestMacvlanIPv6SysctlsAreWellFormed(t *testing.T) {
	if len(macvlanIPv6Sysctls) == 0 {
		t.Fatal("macvlanIPv6Sysctls is empty")
	}
	for _, s := range macvlanIPv6Sysctls {
		if strings.Count(s.path, "%s") != 1 {
			t.Errorf("macvlanIPv6Sysctls entry %q must contain exactly one %%s", s.path)
		}
		if strings.HasPrefix(s.path, "/") {
			t.Errorf("macvlanIPv6Sysctls entry %q must be relative to /proc/sys", s.path)
		}
		if !strings.HasPrefix(s.path, "net/ipv6/") {
			t.Errorf("macvlanIPv6Sysctls entry %q is not an IPv6 setting; IPv4 goes through netlink", s.path)
		}
	}

	// Disabling IPv6 is what stops a router advertisement installing a default
	// route in the pod, so it must not be dropped silently.
	var found bool
	for _, s := range macvlanIPv6Sysctls {
		if s.path == "net/ipv6/conf/%s/disable_ipv6" && s.value == "1" {
			found = true
		}
	}
	if !found {
		t.Error("macvlanIPv6Sysctls must set disable_ipv6=1")
	}
}

// TestMacvlanDevconfUsesKernelABIIndexes pins the IPv4 devconf attribute types.
// They are array indexes into the kernel's ipv4_devconf, not names, so a wrong
// value silently configures a different setting instead of failing.
func TestMacvlanDevconfUsesKernelABIIndexes(t *testing.T) {
	// From include/uapi/linux/ip.h.
	want := map[int]uint32{
		4:  0, // IPV4_DEVCONF_ACCEPT_REDIRECTS
		6:  0, // IPV4_DEVCONF_SEND_REDIRECTS
		18: 2, // IPV4_DEVCONF_ARP_ANNOUNCE
		19: 8, // IPV4_DEVCONF_ARP_IGNORE
	}

	if len(macvlanDevconf) != len(want) {
		t.Fatalf("macvlanDevconf has %d entries, want %d", len(macvlanDevconf), len(want))
	}
	for cfg, value := range want {
		got, ok := macvlanDevconf[cfg]
		if !ok {
			t.Errorf("macvlanDevconf is missing devconf index %d", cfg)
			continue
		}
		if got != value {
			t.Errorf("macvlanDevconf[%d] = %d, want %d", cfg, got, value)
		}
	}

	// arp_ignore=8 means "do not reply for any local address"; anything lower
	// still answers ARP on the physical segment for some addresses.
	if macvlanDevconf[devconfArpIgnore] != 8 {
		t.Errorf("arp_ignore = %d, want 8", macvlanDevconf[devconfArpIgnore])
	}
}
