package tinkerbell

import (
	"encoding/json"
	"fmt"
	"net"
	"sort"
	"strings"

	v2 "github.com/tinkerbell/tinkerbell/api/v1alpha2/tinkerbell"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/conversion"
)

// ConversionAnnotation is the annotation key that holds a JSON-encoded
// preservedV1Alpha1 blob carrying fields with no native v1alpha2 equivalent.
// The blob is set on ConvertTo and consumed on ConvertFrom so round-trips
// (v1alpha1 → v1alpha2 → v1alpha1) are non-destructive.
const ConversionAnnotation = "tinkerbell.org/conversion-data-v1alpha1"

// preservedV1Alpha1 carries Hardware v1alpha1 fields that cannot be mapped
// onto a v1alpha2 field. Each section is omitempty so the annotation
// disappears entirely when no preservation is needed.
//
// Per-interface entries are keyed by MAC (lowercased). When an interface
// has no DHCP MAC, we synthesize a key of the form "_synth_<index>".
type preservedV1Alpha1 struct {
	TinkVersion  int64                        `json:"tinkVersion,omitempty"`
	Resources    map[string]resource.Quantity `json:"resources,omitempty"`
	BMCRefExtras *bmcRefExtras                `json:"bmcRefExtras,omitempty"`
	Metadata     *HardwareMetadata            `json:"metadata,omitempty"`
	Status       *HardwareStatus              `json:"status,omitempty"`
	UserData     *string                      `json:"userData,omitempty"`   // present if v1 had it AND we couldn't put it on v2.Instance.Userdata losslessly
	VendorData   *string                      `json:"vendorData,omitempty"` // same as above
	PerInterface map[string]ifaceExtras       `json:"perInterface,omitempty"`
	// InterfaceOrder records the original v1alpha1 Interfaces[] ordering so
	// that v2→v1 conversion produces a stable slice. v1alpha2 stores
	// interfaces as a map, which has no inherent ordering, so without this
	// list a round-trip would reorder by sorted MAC key.
	InterfaceOrder []string `json:"interfaceOrder,omitempty"`
}

// bmcRefExtras carries the parts of corev1.TypedLocalObjectReference that
// v1alpha2's SimpleReference (name+namespace only) can't represent.
type bmcRefExtras struct {
	APIGroup *string `json:"apiGroup,omitempty"`
	Kind     string  `json:"kind,omitempty"`
}

// ifaceExtras carries per-interface v1alpha1 fields with no v1alpha2 home.
type ifaceExtras struct {
	IfaceName     string   `json:"ifaceName,omitempty"`
	DisableDHCP   *bool    `json:"disableDHCP,omitempty"`
	AllowWorkflow *bool    `json:"allowWorkflow,omitempty"`
	Isoboot       *Isoboot `json:"isoboot,omitempty"`
	IPFamily      int64    `json:"ipFamily,omitempty"`     // v1 DHCP.IP.Family — v2 IPAM splits ipv4/ipv6 by struct membership only
	Arch          string   `json:"arch,omitempty"`         // when interfaces disagree on arch; otherwise hoisted to spec.attributes
	UEFI          *bool    `json:"uefi,omitempty"`         // same as above
	OSIE          *OSIE    `json:"osie,omitempty"`         // when interfaces disagree on OSIE; otherwise hoisted to spec.instance.osie
	NetmaskExtra  string   `json:"netmaskExtra,omitempty"` // when v1 Netmask wasn't a /prefix-convertible mask we preserve original
}

// ConvertTo converts this v1alpha1 Hardware to the conversion hub (v1alpha2 Hardware).
//
// The mapping is best-effort: fields with direct v1alpha2 equivalents are
// translated; fields with no equivalent are JSON-encoded into the
// ConversionAnnotation so a subsequent ConvertFrom restores them. The
// conversion is round-trippable for the lossless subset and non-destructive
// for the rest.
func (src *Hardware) ConvertTo(dstRaw conversion.Hub) error {
	dst, ok := dstRaw.(*v2.Hardware)
	if !ok {
		return fmt.Errorf("ConvertTo: expected *v1alpha2.Hardware, got %T", dstRaw)
	}

	dst.ObjectMeta = *src.ObjectMeta.DeepCopy()
	dst.Spec = v2.HardwareSpec{}

	// Lossy preservation collected as we go; emitted as annotation at the end.
	preserved := preservedV1Alpha1{}

	// --- Direct/renamed scalars ----------------------------------------------
	dst.Spec.AgentID = src.Spec.AgentID
	dst.Spec.Auto.EnrollmentEnabled = src.Spec.Auto.EnrollmentEnabled

	if src.Spec.TinkVersion != 0 {
		preserved.TinkVersion = src.Spec.TinkVersion
	}
	if len(src.Spec.Resources) > 0 {
		preserved.Resources = src.Spec.Resources
	}

	// --- BMCRef → references.builtin.bmc -------------------------------------
	if src.Spec.BMCRef != nil {
		ensureReferences(&dst.Spec)
		dst.Spec.References.Builtin.BMC = v2.SimpleReference{
			Name:      src.Spec.BMCRef.Name,
			Namespace: src.GetNamespace(), // TypedLocalObjectReference has no namespace; same ns as owner
		}
		// Preserve APIGroup/Kind if non-default ("Machine" was the conventional v1 kind).
		if src.Spec.BMCRef.APIGroup != nil || src.Spec.BMCRef.Kind != "" {
			extras := &bmcRefExtras{Kind: src.Spec.BMCRef.Kind}
			if src.Spec.BMCRef.APIGroup != nil {
				v := *src.Spec.BMCRef.APIGroup
				extras.APIGroup = &v
			}
			// Default Kind is "Machine"; only preserve if non-default to keep annotations clean.
			if extras.Kind == "Machine" && (extras.APIGroup == nil || *extras.APIGroup == "bmc.tinkerbell.org") {
				extras = nil
			}
			if extras != nil {
				preserved.BMCRefExtras = extras
			}
		}
	}

	// --- References (top-level map) → references.additional ------------------
	if len(src.Spec.References) > 0 {
		ensureReferences(&dst.Spec)
		dst.Spec.References.Additional = make(map[string]v2.Reference, len(src.Spec.References))
		for k, r := range src.Spec.References {
			dst.Spec.References.Additional[k] = v2.Reference{
				Group:     r.Group,
				Version:   r.Version,
				Resource:  r.Resource,
				Name:      r.Name,
				Namespace: r.Namespace,
			}
		}
	}

	// --- Interfaces[] → NetworkInterfaces[MAC] + hoisted attributes/OSIE -----
	convertInterfacesToV2(src, dst, &preserved)

	// --- Metadata.Instance + Userdata + Vendordata → spec.instance -----------
	convertInstanceToV2(src, dst, &preserved)

	// --- Disks → StorageDevices ----------------------------------------------
	if len(src.Spec.Disks) > 0 {
		dst.Spec.StorageDevices = make([]v2.StorageDevice, 0, len(src.Spec.Disks))
		for _, d := range src.Spec.Disks {
			dst.Spec.StorageDevices = append(dst.Spec.StorageDevices, v2.StorageDevice{Name: d.Device})
		}
	}

	// --- Status preservation (v2 has no status subresource) ------------------
	if src.Status.State != "" {
		s := src.Status
		preserved.Status = &s
	}

	// --- Emit annotation if anything was preserved ---------------------------
	if err := writePreserved(&dst.ObjectMeta, &preserved); err != nil {
		return err
	}
	return nil
}

// ConvertFrom converts a v1alpha2 Hardware (the hub) to this v1alpha1 Hardware.
func (dst *Hardware) ConvertFrom(srcRaw conversion.Hub) error {
	src, ok := srcRaw.(*v2.Hardware)
	if !ok {
		return fmt.Errorf("ConvertFrom: expected *v1alpha2.Hardware, got %T", srcRaw)
	}

	dst.ObjectMeta = *src.ObjectMeta.DeepCopy()
	dst.Spec = HardwareSpec{}
	dst.Status = HardwareStatus{}

	preserved, err := readPreserved(&dst.ObjectMeta)
	if err != nil {
		return err
	}

	// --- Direct/renamed scalars ----------------------------------------------
	dst.Spec.AgentID = src.Spec.AgentID
	dst.Spec.Auto.EnrollmentEnabled = src.Spec.Auto.EnrollmentEnabled

	if preserved.TinkVersion != 0 {
		dst.Spec.TinkVersion = preserved.TinkVersion
	}
	if len(preserved.Resources) > 0 {
		dst.Spec.Resources = preserved.Resources
	}

	// --- references.builtin.bmc → BMCRef -------------------------------------
	if src.Spec.References != nil && src.Spec.References.Builtin.BMC.Name != "" {
		ref := &corev1.TypedLocalObjectReference{
			Name: src.Spec.References.Builtin.BMC.Name,
			Kind: "Machine",
		}
		grp := "bmc.tinkerbell.org"
		ref.APIGroup = &grp
		if preserved.BMCRefExtras != nil {
			if preserved.BMCRefExtras.Kind != "" {
				ref.Kind = preserved.BMCRefExtras.Kind
			}
			if preserved.BMCRefExtras.APIGroup != nil {
				ref.APIGroup = preserved.BMCRefExtras.APIGroup
			}
		}
		dst.Spec.BMCRef = ref
	}

	// --- references.additional → top-level References ------------------------
	if src.Spec.References != nil && len(src.Spec.References.Additional) > 0 {
		dst.Spec.References = make(map[string]Reference, len(src.Spec.References.Additional))
		for k, r := range src.Spec.References.Additional {
			dst.Spec.References[k] = Reference{
				Group:     r.Group,
				Version:   r.Version,
				Resource:  r.Resource,
				Name:      r.Name,
				Namespace: r.Namespace,
			}
		}
	}

	// --- NetworkInterfaces[MAC] → Interfaces[] -------------------------------
	convertInterfacesFromV2(src, dst, preserved)

	// --- spec.instance → Metadata.Instance + UserData/VendorData -------------
	convertInstanceFromV2(src, dst, preserved)

	// --- StorageDevices → Disks ----------------------------------------------
	if len(src.Spec.StorageDevices) > 0 {
		dst.Spec.Disks = make([]Disk, 0, len(src.Spec.StorageDevices))
		for _, sd := range src.Spec.StorageDevices {
			dst.Spec.Disks = append(dst.Spec.Disks, Disk{Device: sd.Name})
		}
	}

	// --- Restore status ------------------------------------------------------
	if preserved.Status != nil {
		dst.Status = *preserved.Status
	}

	// Strip the preservation annotation from the v1alpha1 view — it's an
	// internal conversion artifact, not part of the v1alpha1 API surface.
	delete(dst.ObjectMeta.Annotations, ConversionAnnotation)
	if len(dst.ObjectMeta.Annotations) == 0 {
		dst.ObjectMeta.Annotations = nil
	}

	return nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func ensureReferences(s *v2.HardwareSpec) {
	if s.References == nil {
		s.References = &v2.References{}
	}
}

func writePreserved(meta *metav1.ObjectMeta, preserved *preservedV1Alpha1) error {
	if isEmpty(preserved) {
		return nil
	}
	b, err := json.Marshal(preserved)
	if err != nil {
		return fmt.Errorf("encoding %s: %w", ConversionAnnotation, err)
	}
	if meta.Annotations == nil {
		meta.Annotations = map[string]string{}
	}
	meta.Annotations[ConversionAnnotation] = string(b)
	return nil
}

func readPreserved(meta *metav1.ObjectMeta) (*preservedV1Alpha1, error) {
	out := &preservedV1Alpha1{}
	if meta.Annotations == nil {
		return out, nil
	}
	raw, ok := meta.Annotations[ConversionAnnotation]
	if !ok {
		return out, nil
	}
	if err := json.Unmarshal([]byte(raw), out); err != nil {
		return nil, fmt.Errorf("decoding %s: %w", ConversionAnnotation, err)
	}
	return out, nil
}

func isEmpty(p *preservedV1Alpha1) bool {
	return p.TinkVersion == 0 &&
		len(p.Resources) == 0 &&
		p.BMCRefExtras == nil &&
		p.Metadata == nil &&
		p.Status == nil &&
		p.UserData == nil &&
		p.VendorData == nil &&
		len(p.PerInterface) == 0 &&
		len(p.InterfaceOrder) == 0
}

// ---------------------------------------------------------------------------
// Interface conversion (v1 slice ↔ v2 map)
// ---------------------------------------------------------------------------

func convertInterfacesToV2(src *Hardware, dst *v2.Hardware, preserved *preservedV1Alpha1) {
	if len(src.Spec.Interfaces) == 0 {
		return
	}

	dst.Spec.NetworkInterfaces = make(v2.NetworkInterfaces, len(src.Spec.Interfaces))
	preserved.InterfaceOrder = make([]string, 0, len(src.Spec.Interfaces))

	// First pass: collect arch/uefi to determine if we can hoist a single value.
	// We track presence regardless of value, so disagreement (e.g. one
	// interface has arch="x86_64" and another has arch="") is detected and
	// blocks hoisting — otherwise the round-trip would apply the non-empty
	// arch to the originally-empty interface on ConvertFrom.
	var (
		archSeen    = map[string]struct{}{}
		uefiSeen    = map[bool]struct{}{}
		hoistedArch string
		hoistedUEFI bool
		anyDHCP     bool
	)
	for _, i := range src.Spec.Interfaces {
		if i.DHCP == nil {
			continue
		}
		anyDHCP = true
		archSeen[i.DHCP.Arch] = struct{}{}
		uefiSeen[i.DHCP.UEFI] = struct{}{}
		hoistedArch = i.DHCP.Arch
		hoistedUEFI = i.DHCP.UEFI
	}
	// Only hoist arch when every DHCP-bearing interface agrees AND that
	// shared value is non-empty (an all-empty agreement isn't meaningful).
	canHoistArch := len(archSeen) == 1 && hoistedArch != ""
	canHoistUEFI := anyDHCP && len(uefiSeen) == 1
	if canHoistArch || canHoistUEFI {
		if dst.Spec.Attributes == nil {
			dst.Spec.Attributes = &v2.Attributes{}
		}
		if canHoistArch {
			dst.Spec.Attributes.Arch = hoistedArch
		}
		if canHoistUEFI {
			dst.Spec.Attributes.UEFI = hoistedUEFI
		}
	}

	for idx, i := range src.Spec.Interfaces {
		mac := normalizedMAC(i)
		key := mac
		if key == "" {
			key = fmt.Sprintf("_synth_%d", idx)
		}

		ni := v2.NetworkInterface{}
		extras := ifaceExtras{}

		// DHCP block
		if i.DHCP != nil {
			ni.DHCP = &v2.DHCP{IPv4: convertDHCPv4ToV2(i.DHCP, i.DisableDHCP, &extras)}
			if i.DHCP.IP != nil {
				ipam := convertIPAMToV2(i.DHCP.IP, &extras)
				if ipam != nil {
					ni.IPAM = ipam
				}
			}
			// arch/uefi only stay in annotation if interfaces disagree (multi-value seen)
			if !canHoistArch && i.DHCP.Arch != "" {
				extras.Arch = i.DHCP.Arch
			}
			if !canHoistUEFI {
				u := i.DHCP.UEFI
				extras.UEFI = &u
			}
			if i.DHCP.IfaceName != "" {
				extras.IfaceName = i.DHCP.IfaceName
			}
		}

		// DisableDHCP — only preserve if we couldn't represent it on DHCPv4.Disabled
		// (we DO set DHCPv4.Disabled, so this is for clean restoration on
		// ConvertFrom where DHCPv4 may not have been emitted).
		if i.DisableDHCP {
			b := true
			extras.DisableDHCP = &b
		}

		// Netboot
		if i.Netboot != nil {
			ni.Netboot = &v2.Netboot{}
			// AllowPXE → Disabled (inverse). nil treated as nil (we leave Disabled false).
			if i.Netboot.AllowPXE != nil {
				ni.Netboot.Disabled = !*i.Netboot.AllowPXE
			}
			if i.Netboot.AllowWorkflow != nil {
				v := *i.Netboot.AllowWorkflow
				extras.AllowWorkflow = &v
			}
			if i.Netboot.IPXE != nil {
				ni.Netboot.IPXE = &v2.IPXE{
					URL:    i.Netboot.IPXE.URL,
					Script: i.Netboot.IPXE.Contents,
					Binary: i.Netboot.IPXE.Binary,
				}
			}
			if i.Netboot.OSIE != nil {
				extras.OSIE = i.Netboot.OSIE
			}
		}

		// Isoboot — no v2 equivalent
		if i.Isoboot != nil && i.Isoboot.SourceISO != "" {
			ib := *i.Isoboot
			extras.Isoboot = &ib
		}

		dst.Spec.NetworkInterfaces[v2.MAC(key)] = ni
		preserved.InterfaceOrder = append(preserved.InterfaceOrder, key)

		// Only preserve per-interface extras when non-empty
		if !ifaceExtrasEmpty(&extras) {
			if preserved.PerInterface == nil {
				preserved.PerInterface = map[string]ifaceExtras{}
			}
			preserved.PerInterface[key] = extras
		}
	}

	// If exactly one OSIE was set across interfaces and they all match, hoist
	// it to spec.instance.osie; otherwise it stays in per-interface extras.
	hoistOSIE(src, dst, preserved)
}

func ifaceExtrasEmpty(e *ifaceExtras) bool {
	return e.IfaceName == "" &&
		e.DisableDHCP == nil &&
		e.AllowWorkflow == nil &&
		e.Isoboot == nil &&
		e.IPFamily == 0 &&
		e.Arch == "" &&
		e.UEFI == nil &&
		e.OSIE == nil &&
		e.NetmaskExtra == ""
}

func normalizedMAC(i Interface) string {
	if i.DHCP == nil {
		return ""
	}
	return strings.ToLower(i.DHCP.MAC)
}

func convertDHCPv4ToV2(d *DHCP, disableDHCP bool, extras *ifaceExtras) *v2.DHCPv4 {
	out := &v2.DHCPv4{
		BootFileName:          d.BootFileName,
		ClasslessStaticRoutes: convertClasslessRoutesToV2(d.ClasslessStaticRoutes),
		DomainName:            d.DomainName,
		TFTPServerName:        d.TFTPServerName,
	}
	if d.Hostname != "" {
		h := d.Hostname
		out.Hostname = &h
	}
	if d.LeaseTime != 0 {
		lt := d.LeaseTime
		out.LeaseTimeSeconds = &lt
	}
	if len(d.NameServers) > 0 {
		out.Nameservers = make([]v2.Nameserver, 0, len(d.NameServers))
		for _, n := range d.NameServers {
			out.Nameservers = append(out.Nameservers, v2.Nameserver(n))
		}
	}
	if len(d.TimeServers) > 0 {
		out.NTPServers = make([]v2.Timeserver, 0, len(d.TimeServers))
		for _, t := range d.TimeServers {
			out.NTPServers = append(out.NTPServers, v2.Timeserver(t))
		}
	}
	if d.VLANID != "" {
		v := d.VLANID
		out.VLANID = &v
	}
	if disableDHCP {
		out.Disabled = true
	}
	return out
}

func convertClasslessRoutesToV2(routes []ClasslessStaticRoute) []v2.ClasslessStaticRoute {
	if len(routes) == 0 {
		return nil
	}
	out := make([]v2.ClasslessStaticRoute, 0, len(routes))
	for _, r := range routes {
		out = append(out, v2.ClasslessStaticRoute{
			DestinationDescriptor: r.DestinationDescriptor,
			Router:                r.Router,
		})
	}
	return out
}

func convertIPAMToV2(ip *IP, extras *ifaceExtras) *v2.IPAM {
	if ip == nil || ip.Address == "" {
		return nil
	}
	// v1.Family: 4 → IPv4, 6 → IPv6, 0 (default) → IPv4. Preserve non-default.
	if ip.Family != 0 && ip.Family != 4 && ip.Family != 6 {
		extras.IPFamily = ip.Family
	}
	v4 := &v2.IP{
		Address: ip.Address,
		Gateway: ip.Gateway,
	}
	if p, ok := netmaskToPrefix(ip.Netmask); ok {
		v4.Prefix = p
	} else if ip.Netmask != "" {
		// Non-convertible netmask (unusual but possible) — preserve original.
		extras.NetmaskExtra = ip.Netmask
	}
	if ip.Family == 6 {
		return &v2.IPAM{IPv6: v4}
	}
	return &v2.IPAM{IPv4: v4}
}

// netmaskToPrefix converts a dotted-quad netmask to a CIDR prefix length
// string. Returns ("", false) if the netmask isn't a contiguous-bits mask.
func netmaskToPrefix(mask string) (string, bool) {
	if mask == "" {
		return "", false
	}
	ip := net.ParseIP(mask)
	if ip == nil {
		return "", false
	}
	v4 := ip.To4()
	if v4 == nil {
		return "", false
	}
	ones, bits := net.IPv4Mask(v4[0], v4[1], v4[2], v4[3]).Size()
	if bits == 0 {
		return "", false
	}
	return fmt.Sprintf("%d", ones), true
}

// prefixToNetmask converts a numeric CIDR prefix string to dotted-quad mask.
// Returns "" if prefix is non-numeric or out of range.
func prefixToNetmask(prefix string) string {
	var n int
	if _, err := fmt.Sscanf(prefix, "%d", &n); err != nil {
		return ""
	}
	if n < 0 || n > 32 {
		return ""
	}
	m := net.CIDRMask(n, 32)
	return net.IP(m).String()
}

func hoistOSIE(src *Hardware, dst *v2.Hardware, preserved *preservedV1Alpha1) {
	// If exactly one OSIE struct appears across all interfaces (and they match),
	// hoist it to spec.instance.osie and drop the per-interface extras for it.
	var (
		picked *OSIE
		all    = true
	)
	for _, i := range src.Spec.Interfaces {
		if i.Netboot == nil || i.Netboot.OSIE == nil {
			continue
		}
		o := i.Netboot.OSIE
		if picked == nil {
			picked = o
			continue
		}
		if *picked != *o {
			all = false
			break
		}
	}
	if !all || picked == nil {
		return
	}
	// All interfaces agreed → hoist.
	if dst.Spec.Instance == nil {
		dst.Spec.Instance = &v2.Instance{}
	}
	dst.Spec.Instance.OSIE = &v2.OSIE{
		InitrdURL: picked.Initrd,
		KernelURL: picked.Kernel,
		// v1 OSIE.BaseURL has no direct v2 equivalent. Carry it on the v1 round-trip
		// via per-interface extras (a single interface's worth is enough since they all match).
	}
	if picked.BaseURL != "" {
		// Preserve BaseURL — there's no direct v2 field, attach to any per-interface entry.
		ensurePerIface(preserved)
		for key, ex := range preserved.PerInterface {
			ex.OSIE = picked
			preserved.PerInterface[key] = ex
			return
		}
		// No existing entry — synthesize one keyed on the first iface MAC.
		for _, i := range src.Spec.Interfaces {
			if i.DHCP != nil && i.DHCP.MAC != "" {
				preserved.PerInterface[strings.ToLower(i.DHCP.MAC)] = ifaceExtras{OSIE: picked}
				return
			}
		}
		preserved.PerInterface["_synth_0"] = ifaceExtras{OSIE: picked}
		return
	}
	// Clean per-interface extras' OSIE since it's now hoisted.
	for key, ex := range preserved.PerInterface {
		ex.OSIE = nil
		if ifaceExtrasEmpty(&ex) {
			delete(preserved.PerInterface, key)
		} else {
			preserved.PerInterface[key] = ex
		}
	}
}

func ensurePerIface(p *preservedV1Alpha1) {
	if p.PerInterface == nil {
		p.PerInterface = map[string]ifaceExtras{}
	}
}

func convertInterfacesFromV2(src *v2.Hardware, dst *Hardware, preserved *preservedV1Alpha1) {
	if len(src.Spec.NetworkInterfaces) == 0 {
		return
	}

	// Iteration order over a Go map is non-deterministic. Use the
	// preserved InterfaceOrder when available (round-tripping a previously
	// converted Hardware), otherwise sort by MAC string for stable output.
	var macs []string
	if len(preserved.InterfaceOrder) == len(src.Spec.NetworkInterfaces) {
		macs = preserved.InterfaceOrder
	} else {
		macs = make([]string, 0, len(src.Spec.NetworkInterfaces))
		for k := range src.Spec.NetworkInterfaces {
			macs = append(macs, string(k))
		}
		sort.Strings(macs)
	}

	dst.Spec.Interfaces = make([]Interface, 0, len(macs))
	for _, key := range macs {
		ni := src.Spec.NetworkInterfaces[v2.MAC(key)]
		ex := preserved.PerInterface[key]
		iface := Interface{}

		if ni.DHCP != nil || ni.IPAM != nil || ex.IfaceName != "" || ex.DisableDHCP != nil {
			d := &DHCP{}
			// MAC: only set if the key is a real MAC (synthesized keys start with "_synth_").
			if !strings.HasPrefix(key, "_synth_") {
				d.MAC = key
			}
			d.IfaceName = ex.IfaceName

			if ni.DHCP != nil && ni.DHCP.IPv4 != nil {
				v4 := ni.DHCP.IPv4
				if v4.Hostname != nil {
					d.Hostname = *v4.Hostname
				}
				d.DomainName = v4.DomainName
				if v4.LeaseTimeSeconds != nil {
					d.LeaseTime = *v4.LeaseTimeSeconds
				}
				for _, n := range v4.Nameservers {
					d.NameServers = append(d.NameServers, string(n))
				}
				for _, t := range v4.NTPServers {
					d.TimeServers = append(d.TimeServers, string(t))
				}
				if v4.VLANID != nil {
					d.VLANID = *v4.VLANID
				}
				d.TFTPServerName = v4.TFTPServerName
				d.BootFileName = v4.BootFileName
				for _, r := range v4.ClasslessStaticRoutes {
					d.ClasslessStaticRoutes = append(d.ClasslessStaticRoutes, ClasslessStaticRoute{
						DestinationDescriptor: r.DestinationDescriptor,
						Router:                r.Router,
					})
				}
				if v4.Disabled {
					iface.DisableDHCP = true
				}
			}

			// Arch/UEFI restoration: prefer per-interface extra; fall back to hoisted attributes.
			d.Arch = ex.Arch
			if d.Arch == "" && src.Spec.Attributes != nil {
				d.Arch = src.Spec.Attributes.Arch
			}
			if ex.UEFI != nil {
				d.UEFI = *ex.UEFI
			} else if src.Spec.Attributes != nil {
				d.UEFI = src.Spec.Attributes.UEFI
			}

			if ni.IPAM != nil {
				ip := ipFromV2IPAM(ni.IPAM, ex)
				if ip != nil {
					d.IP = ip
				}
			}
			iface.DHCP = d
		}

		if ex.DisableDHCP != nil {
			iface.DisableDHCP = *ex.DisableDHCP
		}

		// Netboot
		if ni.Netboot != nil || ex.AllowWorkflow != nil || ex.OSIE != nil {
			nb := &Netboot{}
			if ni.Netboot != nil {
				// Disabled → AllowPXE = !Disabled (only if Netboot actually present)
				allow := !ni.Netboot.Disabled
				nb.AllowPXE = &allow
				if ni.Netboot.IPXE != nil {
					nb.IPXE = &IPXE{
						URL:      ni.Netboot.IPXE.URL,
						Contents: ni.Netboot.IPXE.Script,
						Binary:   ni.Netboot.IPXE.Binary,
					}
				}
			}
			if ex.AllowWorkflow != nil {
				nb.AllowWorkflow = ex.AllowWorkflow
			}
			// OSIE: per-interface extras win; otherwise pick up from spec.instance.osie.
			if ex.OSIE != nil {
				nb.OSIE = ex.OSIE
			} else if src.Spec.Instance != nil && src.Spec.Instance.OSIE != nil {
				nb.OSIE = &OSIE{
					Kernel: src.Spec.Instance.OSIE.KernelURL,
					Initrd: src.Spec.Instance.OSIE.InitrdURL,
				}
			}
			if nb.AllowPXE != nil || nb.AllowWorkflow != nil || nb.IPXE != nil || nb.OSIE != nil {
				iface.Netboot = nb
			}
		}

		// Isoboot
		if ex.Isoboot != nil {
			iface.Isoboot = ex.Isoboot
		}

		dst.Spec.Interfaces = append(dst.Spec.Interfaces, iface)
	}
}

func ipFromV2IPAM(ipam *v2.IPAM, ex ifaceExtras) *IP {
	var src *v2.IP
	family := int64(4)
	switch {
	case ipam.IPv4 != nil:
		src = ipam.IPv4
		family = 4
	case ipam.IPv6 != nil:
		src = ipam.IPv6
		family = 6
	default:
		return nil
	}
	out := &IP{
		Address: src.Address,
		Gateway: src.Gateway,
		Family:  family,
	}
	if ex.NetmaskExtra != "" {
		out.Netmask = ex.NetmaskExtra
	} else if src.Prefix != "" {
		out.Netmask = prefixToNetmask(src.Prefix)
	}
	if ex.IPFamily != 0 {
		out.Family = ex.IPFamily
	}
	return out
}

// ---------------------------------------------------------------------------
// Instance / Metadata conversion
// ---------------------------------------------------------------------------

func convertInstanceToV2(src *Hardware, dst *v2.Hardware, preserved *preservedV1Alpha1) {
	hasUser := src.Spec.UserData != nil && *src.Spec.UserData != ""
	hasVendor := src.Spec.VendorData != nil && *src.Spec.VendorData != ""
	hasMeta := src.Spec.Metadata != nil

	if !hasUser && !hasVendor && !hasMeta {
		return
	}
	if dst.Spec.Instance == nil {
		dst.Spec.Instance = &v2.Instance{}
	}

	if hasUser {
		dst.Spec.Instance.Userdata = src.Spec.UserData
	}
	if hasVendor {
		dst.Spec.Instance.Vendordata = src.Spec.VendorData
	}
	if hasMeta {
		// Mirror SSHKeys onto v2.Instance.SSHKeys for native v2 access. The
		// Metadata blob still carries the same value, so round-trip is handled
		// by skipping the v2→v1 SSHKeys path when the preserved Metadata
		// already has them (see convertInstanceFromV2).
		inst := src.Spec.Metadata.Instance
		if inst != nil && len(inst.SSHKeys) > 0 {
			dst.Spec.Instance.SSHKeys = append([]string(nil), inst.SSHKeys...)
		}
		// We deliberately do NOT promote Metadata.Instance.Userdata into
		// v2.Instance.Userdata. The top-level v1.Spec.UserData is the
		// canonical source for v2.Instance.Userdata; Metadata.Instance.Userdata
		// is preserved via the Metadata blob and restored on ConvertFrom.

		// Preserve the full metadata blob for round-trip (the structure is
		// rich and the v2 Instance shape is much thinner).
		m := *src.Spec.Metadata
		preserved.Metadata = &m
	}
}

func convertInstanceFromV2(src *v2.Hardware, dst *Hardware, preserved *preservedV1Alpha1) {
	if preserved.Metadata != nil {
		m := *preserved.Metadata
		dst.Spec.Metadata = &m
	}
	if src.Spec.Instance == nil {
		return
	}
	if src.Spec.Instance.Userdata != nil {
		u := *src.Spec.Instance.Userdata
		dst.Spec.UserData = &u
	}
	if src.Spec.Instance.Vendordata != nil {
		v := *src.Spec.Instance.Vendordata
		dst.Spec.VendorData = &v
	}
	// SSHKeys: if not already represented inside the preserved Metadata.Instance,
	// reattach to a freshly-allocated Metadata.Instance.
	if len(src.Spec.Instance.SSHKeys) > 0 {
		if dst.Spec.Metadata == nil {
			dst.Spec.Metadata = &HardwareMetadata{}
		}
		if dst.Spec.Metadata.Instance == nil {
			dst.Spec.Metadata.Instance = &MetadataInstance{}
		}
		if len(dst.Spec.Metadata.Instance.SSHKeys) == 0 {
			dst.Spec.Metadata.Instance.SSHKeys = append([]string(nil), src.Spec.Instance.SSHKeys...)
		}
	}
}
