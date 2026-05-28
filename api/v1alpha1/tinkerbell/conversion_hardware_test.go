package tinkerbell

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	v2 "github.com/tinkerbell/tinkerbell/api/v1alpha2/tinkerbell"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func ptr[T any](v T) *T { return &v }

// roundTrip verifies that converting v1 → v2 → v1 yields the original.
// This is the primary contract: lossless round-tripping via the annotation
// preservation mechanism.
func roundTrip(t *testing.T, name string, in *Hardware) {
	t.Helper()
	t.Run(name, func(t *testing.T) {
		hub := &v2.Hardware{}
		if err := in.ConvertTo(hub); err != nil {
			t.Fatalf("ConvertTo: %v", err)
		}
		got := &Hardware{}
		if err := got.ConvertFrom(hub); err != nil {
			t.Fatalf("ConvertFrom: %v", err)
		}
		if diff := cmp.Diff(in, got); diff != "" {
			t.Errorf("round-trip mismatch (-want +got):\n%s", diff)
		}
	})
}

func TestHardwareConvert_Empty(t *testing.T) {
	roundTrip(t, "empty", &Hardware{
		ObjectMeta: metav1.ObjectMeta{Name: "hw", Namespace: "ns"},
	})
}

func TestHardwareConvert_Direct(t *testing.T) {
	roundTrip(t, "scalars-and-auto", &Hardware{
		ObjectMeta: metav1.ObjectMeta{Name: "hw", Namespace: "ns"},
		Spec: HardwareSpec{
			AgentID: "agent-1",
			Auto:    AutoCapabilities{EnrollmentEnabled: true},
		},
	})
}

func TestHardwareConvert_TinkVersionAndResources(t *testing.T) {
	roundTrip(t, "preserved-scalars", &Hardware{
		ObjectMeta: metav1.ObjectMeta{Name: "hw", Namespace: "ns"},
		Spec: HardwareSpec{
			TinkVersion: 42,
			Resources: map[string]resource.Quantity{
				"cpu":    resource.MustParse("4"),
				"memory": resource.MustParse("8Gi"),
			},
		},
	})
}

func TestHardwareConvert_BMCRef_Default(t *testing.T) {
	roundTrip(t, "bmcref-default-machine", &Hardware{
		ObjectMeta: metav1.ObjectMeta{Name: "hw", Namespace: "ns"},
		Spec: HardwareSpec{
			BMCRef: &corev1.TypedLocalObjectReference{
				APIGroup: ptr("bmc.tinkerbell.org"),
				Kind:     "Machine",
				Name:     "bmc-1",
			},
		},
	})
}

func TestHardwareConvert_BMCRef_NonDefault(t *testing.T) {
	roundTrip(t, "bmcref-custom-kind", &Hardware{
		ObjectMeta: metav1.ObjectMeta{Name: "hw", Namespace: "ns"},
		Spec: HardwareSpec{
			BMCRef: &corev1.TypedLocalObjectReference{
				APIGroup: ptr("custom.example.com"),
				Kind:     "CustomMachine",
				Name:     "bmc-1",
			},
		},
	})
}

func TestHardwareConvert_References(t *testing.T) {
	roundTrip(t, "references-map", &Hardware{
		ObjectMeta: metav1.ObjectMeta{Name: "hw", Namespace: "ns"},
		Spec: HardwareSpec{
			References: map[string]Reference{
				"lvm": {
					Group:     "storage.example.com",
					Version:   "v1",
					Resource:  "lvms",
					Namespace: "ns",
					Name:      "lvm-1",
				},
			},
		},
	})
}

func TestHardwareConvert_Interfaces_Single(t *testing.T) {
	roundTrip(t, "single-iface", &Hardware{
		ObjectMeta: metav1.ObjectMeta{Name: "hw", Namespace: "ns"},
		Spec: HardwareSpec{
			Interfaces: []Interface{{
				DHCP: &DHCP{
					MAC:            "52:54:00:41:05:c6",
					Hostname:       "node-1",
					DomainName:     "example.com",
					LeaseTime:      3600,
					NameServers:    []string{"8.8.8.8", "1.1.1.1"},
					TimeServers:    []string{"pool.ntp.org"},
					Arch:           "x86_64",
					UEFI:           true,
					IfaceName:      "eth0",
					VLANID:         "100",
					TFTPServerName: "tftp.example.com",
					BootFileName:   "ipxe.efi",
					IP: &IP{
						Address: "10.0.0.5",
						Netmask: "255.255.255.0",
						Gateway: "10.0.0.1",
						Family:  4,
					},
					ClasslessStaticRoutes: []ClasslessStaticRoute{
						{DestinationDescriptor: "192.168.1.0/24", Router: "10.0.0.1"},
					},
				},
				DisableDHCP: false,
				Netboot: &Netboot{
					AllowPXE:      ptr(true),
					AllowWorkflow: ptr(true),
					IPXE: &IPXE{
						URL:      "http://example.com/ipxe",
						Contents: "#!ipxe\necho hello",
						Binary:   "ipxe.efi",
					},
				},
			}},
		},
	})
}

func TestHardwareConvert_Interfaces_MultipleSameArch(t *testing.T) {
	roundTrip(t, "two-ifaces-same-arch", &Hardware{
		ObjectMeta: metav1.ObjectMeta{Name: "hw", Namespace: "ns"},
		Spec: HardwareSpec{
			Interfaces: []Interface{
				{DHCP: &DHCP{MAC: "52:54:00:00:00:01", Arch: "x86_64", UEFI: true}},
				{DHCP: &DHCP{MAC: "52:54:00:00:00:02", Arch: "x86_64", UEFI: true}},
			},
		},
	})
}

func TestHardwareConvert_Interfaces_MultipleDifferentArch(t *testing.T) {
	roundTrip(t, "two-ifaces-different-arch", &Hardware{
		ObjectMeta: metav1.ObjectMeta{Name: "hw", Namespace: "ns"},
		Spec: HardwareSpec{
			Interfaces: []Interface{
				{DHCP: &DHCP{MAC: "52:54:00:00:00:01", Arch: "x86_64", UEFI: true}},
				{DHCP: &DHCP{MAC: "52:54:00:00:00:02", Arch: "aarch64", UEFI: false}},
			},
		},
	})
}

func TestHardwareConvert_Interfaces_DisableDHCP(t *testing.T) {
	roundTrip(t, "disable-dhcp", &Hardware{
		ObjectMeta: metav1.ObjectMeta{Name: "hw", Namespace: "ns"},
		Spec: HardwareSpec{
			Interfaces: []Interface{
				{DHCP: &DHCP{MAC: "aa:bb:cc:dd:ee:01"}, DisableDHCP: true},
			},
		},
	})
}

func TestHardwareConvert_Interfaces_Isoboot(t *testing.T) {
	roundTrip(t, "isoboot", &Hardware{
		ObjectMeta: metav1.ObjectMeta{Name: "hw", Namespace: "ns"},
		Spec: HardwareSpec{
			Interfaces: []Interface{
				{
					DHCP:    &DHCP{MAC: "aa:bb:cc:dd:ee:01"},
					Isoboot: &Isoboot{SourceISO: "http://example.com/osie.iso"},
				},
			},
		},
	})
}

func TestHardwareConvert_Disks(t *testing.T) {
	roundTrip(t, "disks", &Hardware{
		ObjectMeta: metav1.ObjectMeta{Name: "hw", Namespace: "ns"},
		Spec: HardwareSpec{
			Disks: []Disk{{Device: "/dev/sda"}, {Device: "/dev/nvme0n1"}},
		},
	})
}

func TestHardwareConvert_UserAndVendorData(t *testing.T) {
	roundTrip(t, "user-vendor-data", &Hardware{
		ObjectMeta: metav1.ObjectMeta{Name: "hw", Namespace: "ns"},
		Spec: HardwareSpec{
			UserData:   ptr("#cloud-config\nhostname: foo"),
			VendorData: ptr("vendor-data-blob"),
		},
	})
}

func TestHardwareConvert_Metadata(t *testing.T) {
	roundTrip(t, "metadata-full", &Hardware{
		ObjectMeta: metav1.ObjectMeta{Name: "hw", Namespace: "ns"},
		Spec: HardwareSpec{
			Metadata: &HardwareMetadata{
				State:       "active",
				BondingMode: 4,
				Manufacturer: &MetadataManufacturer{
					ID:   "mfr-1",
					Slug: "dell",
				},
				Instance: &MetadataInstance{
					ID:       "inst-1",
					Hostname: "node-1",
					AllowPxe: true,
					Userdata: "#cloud-config",
					SSHKeys:  []string{"ssh-rsa AAA..."},
					Tags:     []string{"role:worker"},
					OperatingSystem: &MetadataInstanceOperatingSystem{
						Slug:   "ubuntu-22.04",
						Distro: "ubuntu",
					},
				},
				Facility: &MetadataFacility{
					PlanSlug:     "plan-1",
					FacilityCode: "fac-1",
				},
			},
		},
	})
}

func TestHardwareConvert_Status(t *testing.T) {
	roundTrip(t, "status", &Hardware{
		ObjectMeta: metav1.ObjectMeta{Name: "hw", Namespace: "ns"},
		Status:     HardwareStatus{State: HardwareReady},
	})
}

// TestHardwareConvert_HoistedAttributes verifies that for a single architecture
// across all interfaces, arch/uefi land on spec.attributes rather than
// per-interface annotations.
func TestHardwareConvert_HoistedAttributes(t *testing.T) {
	src := &Hardware{
		ObjectMeta: metav1.ObjectMeta{Name: "hw", Namespace: "ns"},
		Spec: HardwareSpec{
			Interfaces: []Interface{
				{DHCP: &DHCP{MAC: "aa:bb:cc:00:00:01", Arch: "x86_64", UEFI: true}},
				{DHCP: &DHCP{MAC: "aa:bb:cc:00:00:02", Arch: "x86_64", UEFI: true}},
			},
		},
	}
	hub := &v2.Hardware{}
	if err := src.ConvertTo(hub); err != nil {
		t.Fatalf("ConvertTo: %v", err)
	}
	if hub.Spec.Attributes == nil {
		t.Fatal("expected hub.Spec.Attributes to be set when arch/uefi agree across interfaces")
	}
	if hub.Spec.Attributes.Arch != "x86_64" || !hub.Spec.Attributes.UEFI {
		t.Errorf("attribute hoisting mismatch: got %+v", hub.Spec.Attributes)
	}
	// Per-interface arch/uefi annotations should NOT be present when fully hoisted.
	preserved, err := readPreserved(&hub.ObjectMeta)
	if err != nil {
		t.Fatalf("readPreserved: %v", err)
	}
	for k, ex := range preserved.PerInterface {
		if ex.Arch != "" || ex.UEFI != nil {
			t.Errorf("iface %q has arch/uefi in preserved when they should be hoisted: %+v", k, ex)
		}
	}
}

// TestHardwareConvert_SyntheticMACKey verifies an interface with no MAC gets
// a synthesized map key and survives the round-trip with no spurious MAC set.
func TestHardwareConvert_SyntheticMACKey(t *testing.T) {
	roundTrip(t, "no-mac-iface", &Hardware{
		ObjectMeta: metav1.ObjectMeta{Name: "hw", Namespace: "ns"},
		Spec: HardwareSpec{
			Interfaces: []Interface{
				{DHCP: &DHCP{Hostname: "no-mac-host"}},
			},
		},
	})
}

// TestHardwareConvert_NetmaskRoundTrip ensures netmask→prefix→netmask retains
// the exact original dotted-quad mask for the common /24 case.
func TestHardwareConvert_NetmaskRoundTrip(t *testing.T) {
	src := &Hardware{
		ObjectMeta: metav1.ObjectMeta{Name: "hw", Namespace: "ns"},
		Spec: HardwareSpec{
			Interfaces: []Interface{{
				DHCP: &DHCP{
					MAC: "52:54:00:00:00:01",
					IP: &IP{
						Address: "10.0.0.5",
						Netmask: "255.255.255.0",
						Gateway: "10.0.0.1",
						Family:  4,
					},
				},
			}},
		},
	}
	hub := &v2.Hardware{}
	if err := src.ConvertTo(hub); err != nil {
		t.Fatal(err)
	}
	mac := v2.MAC("52:54:00:00:00:01")
	if hub.Spec.NetworkInterfaces[mac].IPAM == nil ||
		hub.Spec.NetworkInterfaces[mac].IPAM.IPv4 == nil ||
		hub.Spec.NetworkInterfaces[mac].IPAM.IPv4.Prefix != "24" {
		t.Fatalf("expected /24 prefix, got %+v", hub.Spec.NetworkInterfaces[mac].IPAM)
	}
	got := &Hardware{}
	if err := got.ConvertFrom(hub); err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(src, got); diff != "" {
		t.Errorf("round-trip mismatch:\n%s", diff)
	}
}

// TestHardwareConvert_AnnotationStripped checks that the conversion annotation
// is removed from the v1alpha1 view on ConvertFrom — it's an internal artifact.
func TestHardwareConvert_AnnotationStripped(t *testing.T) {
	src := &Hardware{
		ObjectMeta: metav1.ObjectMeta{Name: "hw", Namespace: "ns"},
		Spec:       HardwareSpec{TinkVersion: 7},
	}
	hub := &v2.Hardware{}
	if err := src.ConvertTo(hub); err != nil {
		t.Fatal(err)
	}
	if _, ok := hub.ObjectMeta.Annotations[ConversionAnnotation]; !ok {
		t.Fatal("expected hub to carry the conversion annotation")
	}
	got := &Hardware{}
	if err := got.ConvertFrom(hub); err != nil {
		t.Fatal(err)
	}
	if _, ok := got.ObjectMeta.Annotations[ConversionAnnotation]; ok {
		t.Errorf("expected annotation to be stripped from v1alpha1 view; got: %v", got.ObjectMeta.Annotations)
	}
}

// TestHardwareConvert_Interfaces_MixedEmptyArch verifies that when one
// interface has an arch and another has empty, arch is NOT hoisted (the
// empty value would otherwise be silently overwritten on ConvertFrom).
// This is the regression test for the review's CRITICAL finding #1.
func TestHardwareConvert_Interfaces_MixedEmptyArch(t *testing.T) {
	src := &Hardware{
		ObjectMeta: metav1.ObjectMeta{Name: "hw", Namespace: "ns"},
		Spec: HardwareSpec{
			Interfaces: []Interface{
				{DHCP: &DHCP{MAC: "aa:bb:cc:00:00:01", Arch: "x86_64"}},
				{DHCP: &DHCP{MAC: "aa:bb:cc:00:00:02", Arch: ""}},
			},
		},
	}
	hub := &v2.Hardware{}
	if err := src.ConvertTo(hub); err != nil {
		t.Fatalf("ConvertTo: %v", err)
	}
	if hub.Spec.Attributes != nil && hub.Spec.Attributes.Arch != "" {
		t.Errorf("expected arch NOT hoisted when interfaces disagree (one empty); got %q", hub.Spec.Attributes.Arch)
	}
	roundTrip(t, "mixed-empty-arch", src)
}

// TestHardwareConvert_IPv6 covers the IPv6 IPAM path.
func TestHardwareConvert_IPv6(t *testing.T) {
	roundTrip(t, "ipv6-ipam", &Hardware{
		ObjectMeta: metav1.ObjectMeta{Name: "hw", Namespace: "ns"},
		Spec: HardwareSpec{
			Interfaces: []Interface{{
				DHCP: &DHCP{
					MAC: "aa:bb:cc:00:00:01",
					IP: &IP{
						Address: "2001:db8::1",
						Gateway: "2001:db8::ffff",
						Family:  6,
					},
				},
			}},
		},
	})
}

// TestHardwareConvert_Netmask_Slash23 covers a non-/24 mask to verify
// the netmask ↔ prefix conversion isn't /24-specific.
func TestHardwareConvert_Netmask_Slash23(t *testing.T) {
	roundTrip(t, "netmask-23", &Hardware{
		ObjectMeta: metav1.ObjectMeta{Name: "hw", Namespace: "ns"},
		Spec: HardwareSpec{
			Interfaces: []Interface{{
				DHCP: &DHCP{
					MAC: "aa:bb:cc:00:00:01",
					IP: &IP{
						Address: "10.0.0.5",
						Netmask: "255.255.254.0",
						Gateway: "10.0.0.1",
						Family:  4,
					},
				},
			}},
		},
	})
}

// TestHardwareConvert_Netmask_NonContiguous preserves an unusual netmask
// that isn't a contiguous-bits CIDR via the NetmaskExtra preservation path.
func TestHardwareConvert_Netmask_NonContiguous(t *testing.T) {
	roundTrip(t, "netmask-non-contiguous", &Hardware{
		ObjectMeta: metav1.ObjectMeta{Name: "hw", Namespace: "ns"},
		Spec: HardwareSpec{
			Interfaces: []Interface{{
				DHCP: &DHCP{
					MAC: "aa:bb:cc:00:00:01",
					IP: &IP{
						Address: "10.0.0.5",
						Netmask: "255.255.255.1", // intentionally invalid
						Gateway: "10.0.0.1",
						Family:  4,
					},
				},
			}},
		},
	})
}

// TestHardwareConvert_MixedInterfaceShapes — one interface with DHCP+arch,
// another with no DHCP (so synthesized MAC key) carrying Isoboot.
func TestHardwareConvert_MixedInterfaceShapes(t *testing.T) {
	roundTrip(t, "mixed-iface-shapes", &Hardware{
		ObjectMeta: metav1.ObjectMeta{Name: "hw", Namespace: "ns"},
		Spec: HardwareSpec{
			Interfaces: []Interface{
				{DHCP: &DHCP{MAC: "aa:bb:cc:00:00:01", Arch: "x86_64"}},
				{Isoboot: &Isoboot{SourceISO: "http://example/osie.iso"}},
			},
		},
	})
}

// TestHardwareConvert_TypeAssertionError exercises the negative path on a
// non-Hardware Hub.
func TestHardwareConvert_TypeAssertionError(t *testing.T) {
	src := &Hardware{}
	type fakeHub struct{ v2.Hardware }
	if err := src.ConvertTo((*fakeHub)(nil)); err == nil {
		t.Error("expected ConvertTo to error on wrong Hub type")
	}
	if err := src.ConvertFrom((*fakeHub)(nil)); err == nil {
		t.Error("expected ConvertFrom to error on wrong Hub type")
	}
}
