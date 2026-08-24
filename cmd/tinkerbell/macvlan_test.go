//go:build linux

package main

import (
	"strings"
	"testing"

	"github.com/go-logr/logr"
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

func TestHardenMacvlanRejectsUnexpectedName(t *testing.T) {
	// The name is interpolated into a /proc/sys path, so anything that is not a
	// name this binary generated must be refused rather than written through.
	for _, name := range []string{"", "eth0", "../../../etc/passwd", "mv550e8400e29b/../all", "MV550E8400E29B"} {
		if err := hardenMacvlan(logr.Discard(), name); err == nil {
			t.Fatalf("hardenMacvlan(%q) = nil, want error", name)
		}
	}
}

func TestMacvlanSysctlsAreWellFormed(t *testing.T) {
	const iface = "mv550e8400e29b4"

	if len(macvlanSysctls) == 0 {
		t.Fatal("macvlanSysctls is empty")
	}
	for _, s := range macvlanSysctls {
		if strings.Count(s.path, "%s") != 1 {
			t.Errorf("macvlanSysctls entry %q must contain exactly one %%s", s.path)
		}
		if strings.HasPrefix(s.path, "/") {
			t.Errorf("macvlanSysctls entry %q must be relative to /proc/sys", s.path)
		}
	}

	// The two settings that actually keep the interface from disturbing the
	// pod's routing, called out so they cannot be dropped silently.
	want := map[string]string{
		"net/ipv6/conf/%s/disable_ipv6": "1",
		"net/ipv4/conf/%s/arp_ignore":   "8",
	}
	got := make(map[string]string, len(macvlanSysctls))
	for _, s := range macvlanSysctls {
		got[s.path] = s.value
	}
	for path, value := range want {
		if got[path] != value {
			t.Errorf("macvlanSysctls[%q] = %q, want %q", path, got[path], value)
		}
		if strings.Contains(path, "%s") && !strings.Contains(strings.ReplaceAll(path, "%s", iface), iface) {
			t.Errorf("macvlanSysctls entry %q does not interpolate the interface name", path)
		}
	}
}
