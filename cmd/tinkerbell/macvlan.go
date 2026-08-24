//go:build linux

package main

import (
	"errors"
	"fmt"
	"io/fs"
	"math"
	"net"
	"os"
	"regexp"
	"runtime"
	"strings"

	"github.com/go-logr/logr"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

// macvlanNameRe matches interface names created by this binary: "mv" + 13 lowercase hex chars.
var macvlanNameRe = regexp.MustCompile(`^mv[0-9a-f]{13}$`)

const (
	macvlanIfacePrefix = "mv"
	podUIDEnv          = "MY_POD_UID"
	hostNetNSPath      = "/proc/1/ns/net"
)

// macvlanIfaceName derives a deterministic, per-pod interface name from the
// Kubernetes pod UID supplied via the MY_POD_UID downward-API env var.
//
// UUID dashes are stripped and the first 13 hex characters are used:
//
//	"mv" + "550e8400e29b4" → "mv550e8400e29b4"  (15 chars)
func macvlanIfaceName() (string, error) {
	uid := os.Getenv(podUIDEnv)
	if uid == "" {
		return "", fmt.Errorf("env var %s is not set; required for per-pod macvlan interface naming", podUIDEnv)
	}
	hex := strings.ReplaceAll(uid, "-", "")
	if len(hex) < 13 {
		return "", fmt.Errorf("pod UID %q is too short after stripping dashes (got %d chars, need ≥13)", uid, len(hex))
	}
	return macvlanIfacePrefix + hex[:13], nil
}

// setupMacvlan creates a macvlan interface (bridge mode) in the host network
// namespace, moves it into the calling process's network namespace, and brings
// it up as a layer 2 receiver only: no IP address, no routes, and hardened by
// hardenMacvlan so it cannot participate in the pod's routing. Smee's DHCP
// server reaches it via SO_BINDTODEVICE, which is all the kernel needs to
// deliver inbound limited broadcasts and to send replies back out of an
// unaddressed interface.
//
// It first purges any stale mv* interfaces found in the host netns (orphans
// from pods that exited without cleanup). The returned cleanup func deletes
// the interface from the pod netns on graceful shutdown.
//
// Required capabilities: CAP_NET_ADMIN (netlink), CAP_SYS_ADMIN (setns).
// Requires hostPID=true so that /proc/1/ns/net is accessible.
//
// sourceIface is the host parent interface; when empty the host's default
// route interface is used.
func setupMacvlan(log logr.Logger, sourceIface, ifaceName string) (cleanup func(), err error) {
	// Netns operations are per OS thread; pin this goroutine for the duration.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	podNS, err := netns.Get()
	if err != nil {
		return nil, fmt.Errorf("get pod netns: %w", err)
	}
	defer podNS.Close()

	hostNS, err := netns.GetFromPath(hostNetNSPath)
	if err != nil {
		return nil, fmt.Errorf("open host netns %s: %w", hostNetNSPath, err)
	}
	defer hostNS.Close()

	// Clear a leftover from a previous run of this container before claiming
	// the name again. Still in the pod netns at this point.
	purgePodMacvlan(log, ifaceName)

	if err := netns.Set(hostNS); err != nil {
		return nil, fmt.Errorf("enter host netns: %w", err)
	}

	// Purge stale mv* interfaces from dead pods before creating our own.
	purgeStaleMacvlans(log, hostNS, podNS)

	if sourceIface == "" {
		sourceIface, err = defaultRouteInterface()
		if err != nil {
			_ = netns.Set(podNS)
			return nil, fmt.Errorf("detect host default route interface: %w", err)
		}
		log.Info("auto-detected macvlan source interface", "interface", sourceIface)
	}

	parent, err := netlink.LinkByName(sourceIface)
	if err != nil {
		_ = netns.Set(podNS)
		return nil, fmt.Errorf("find source interface %q in host netns: %w", sourceIface, err)
	}

	if err := netlink.LinkAdd(&netlink.Macvlan{
		LinkAttrs: netlink.LinkAttrs{
			Name:        ifaceName,
			ParentIndex: parent.Attrs().Index,
		},
		Mode: netlink.MACVLAN_MODE_BRIDGE,
	}); err != nil {
		_ = netns.Set(podNS)
		return nil, fmt.Errorf("create macvlan %q on %q: %w", ifaceName, sourceIface, err)
	}

	mv, err := netlink.LinkByName(ifaceName)
	if err != nil {
		_ = netns.Set(podNS)
		return nil, fmt.Errorf("re-fetch macvlan %q: %w", ifaceName, err)
	}

	if err := netlink.LinkSetNsFd(mv, int(podNS)); err != nil {
		_ = netlink.LinkDel(mv)
		_ = netns.Set(podNS)
		return nil, fmt.Errorf("move macvlan %q to pod netns: %w", ifaceName, err)
	}
	log.Info("moved macvlan into pod netns", "interface", ifaceName, "parent", sourceIface)

	if err := netns.Set(podNS); err != nil {
		// The interface is already in the pod netns but this thread is stuck in
		// the host netns, so it cannot be cleaned up from here. The next start
		// clears it via purgePodMacvlan.
		return nil, fmt.Errorf("return to pod netns: %w", err)
	}

	// From here on the interface lives in the pod netns, which outlives this
	// process. Remove it on any failure: the name is derived from the pod UID
	// and so is identical on the next start, and LinkSetNsFd carries no rename
	// attribute, so a leftover makes every later start fail with EEXIST.
	defer func() {
		if err != nil {
			teardownMacvlan(log, ifaceName)
		}
	}()

	podLink, err := netlink.LinkByName(ifaceName)
	if err != nil {
		return nil, fmt.Errorf("find macvlan %q in pod netns: %w", ifaceName, err)
	}

	// Suppress the IPv6 link-local address the kernel would otherwise generate
	// when the link comes up. Done before LinkSetUp so the address is never
	// assigned, rather than assigned and then removed.
	//
	// A kernel with IPv6 compiled out, or disabled at boot with ipv6.disable=1,
	// has no inet6 device to configure and rejects this with EAFNOSUPPORT. There
	// is no link-local to suppress in that case, so carry on — the same stance
	// hardenMacvlan takes towards the IPv6 sysctls being absent.
	if err := netlink.LinkSetIP6AddrGenMode(podLink, nl.IN6_ADDR_GEN_MODE_NONE); err != nil {
		if !errors.Is(err, unix.EAFNOSUPPORT) && !errors.Is(err, unix.EOPNOTSUPP) {
			return nil, fmt.Errorf("disable IPv6 address generation on macvlan %q: %w", ifaceName, err)
		}
		log.Info("kernel reports no IPv6 support for this link; nothing to suppress", "interface", ifaceName)
	}

	// Harden before the link comes up, so no LAN traffic is ever processed with
	// the permissive defaults in place.
	if err := hardenMacvlan(log, podLink, ifaceName); err != nil {
		return nil, err
	}

	if err := netlink.LinkSetUp(podLink); err != nil {
		return nil, fmt.Errorf("bring up macvlan %q: %w", ifaceName, err)
	}

	// The interface is deliberately left without an IP address and without any
	// routes. It exists solely to receive DHCP broadcasts from the physical
	// segment; the DHCP server pins its socket to this interface with
	// SO_BINDTODEVICE, which is sufficient for the kernel to both deliver
	// inbound limited broadcasts and send replies back out of it. Leaving the
	// pod's routing table untouched keeps the CNI datapath (Cilium's eBPF
	// programs on eth0, in particular) in sole control of all routed traffic.
	log.Info("macvlan ready", "interface", ifaceName, "parent", sourceIface, "addressed", false)

	return func() { teardownMacvlan(log, ifaceName) }, nil
}

// A bridge-mode macvlan floods every broadcast and multicast frame from the
// physical segment into the pod's network namespace, and at their defaults the
// kernel's per-interface settings let that traffic reconfigure the pod. The
// interface only ever needs to receive DHCP broadcasts, so hardenMacvlan
// switches off every other form of network participation.
//
// The IPv4 half is applied over netlink (see macvlanDevconf), the IPv6 half by
// writing /proc/sys (see macvlanIPv6Sysctls), because the kernel exposes no
// netlink attribute for those.

// IPv4 devconf indexes from include/uapi/linux/ip.h. These are the values the
// kernel expects as attribute types inside IFLA_INET_CONF, and are stable
// kernel ABI.
const (
	devconfAcceptRedirects = 4
	devconfSendRedirects   = 6
	devconfArpAnnounce     = 18
	devconfArpIgnore       = 19

	// iflaInetConf is IFLA_INET_CONF from include/uapi/linux/if_link.h.
	iflaInetConf = 1
)

// macvlanDevconf are the per-interface IPv4 settings applied to the macvlan.
//
// arp_ignore=8 stops the namespace answering ARP on the physical segment for
// addresses it holds on other interfaces, which would otherwise include the
// CNI-assigned address on eth0 and so put the pod's IP on the wrong segment.
// arp_announce=2 keeps any ARP the interface does send from advertising an
// address that is not valid there. The redirect settings stop hosts on the
// segment steering the pod's traffic.
//
// The two ARP settings are the load-bearing ones and are guaranteed to take
// effect: the kernel resolves them as max(conf.all, conf.<if>) (IN_DEV_MAXCONF
// in include/linux/inetdevice.h), so a per-interface value always wins. The
// redirect settings are best effort by comparison — with forwarding disabled
// the kernel ORs them with conf.all, so a conf.all of 1 overrides them.
var macvlanDevconf = map[int]uint32{
	devconfArpIgnore:       8,
	devconfArpAnnounce:     2,
	devconfAcceptRedirects: 0,
	devconfSendRedirects:   0,
}

// macvlanIPv6Sysctls are the per-interface IPv6 settings applied to the
// macvlan. The format verb is the interface name.
//
// Left at their defaults, a router advertisement from the physical segment
// installs an IPv6 default route in the pod that bypasses the CNI datapath
// entirely, and SLAAC configures a global address on the macvlan. Disabling
// IPv6 address generation alone does not prevent either: the interface still
// joins the all-nodes multicast group and still acts on advertisements.
//
// Unlike the IPv4 settings these have no netlink equivalent — inet6_set_link_af
// accepts only IFLA_INET6_TOKEN and IFLA_INET6_ADDR_GEN_MODE — so they need a
// writable /proc/sys, which container runtimes provide only to privileged
// containers.
var macvlanIPv6Sysctls = []struct {
	path  string
	value string
}{
	{"net/ipv6/conf/%s/disable_ipv6", "1"},
	{"net/ipv6/conf/%s/accept_ra", "0"},
	{"net/ipv6/conf/%s/autoconf", "0"},
}

// setIPv4Devconf applies per-interface IPv4 settings over netlink, as
// RTM_SETLINK carrying IFLA_AF_SPEC -> AF_INET -> IFLA_INET_CONF.
//
// This is the same configuration /proc/sys/net/ipv4/conf/<if>/ exposes, but it
// needs only CAP_NET_ADMIN and works when /proc/sys is mounted read-only, which
// is how container runtimes mount it for unprivileged containers.
func setIPv4Devconf(link netlink.Link, values map[int]uint32) error {
	// ifinfomsg carries the index as a signed 32 bit field.
	index := link.Attrs().Index
	if index <= 0 || index > math.MaxInt32 {
		return fmt.Errorf("interface %q has out of range index %d", link.Attrs().Name, index)
	}

	req := nl.NewNetlinkRequest(unix.RTM_SETLINK, unix.NLM_F_ACK)

	msg := nl.NewIfInfomsg(unix.AF_UNSPEC)
	msg.Index = int32(index)
	req.AddData(msg)

	// NLA_F_NESTED is required on each level; without it the kernel rejects the
	// message with EINVAL.
	spec := nl.NewRtAttr(unix.IFLA_AF_SPEC|unix.NLA_F_NESTED, nil)
	inet := spec.AddRtAttr(unix.AF_INET|unix.NLA_F_NESTED, nil)
	conf := inet.AddRtAttr(iflaInetConf|unix.NLA_F_NESTED, nil)
	for cfg, value := range values {
		conf.AddRtAttr(cfg, nl.Uint32Attr(value))
	}
	req.AddData(spec)

	if _, err := req.Execute(unix.NETLINK_ROUTE, 0); err != nil {
		return fmt.Errorf("set IPv4 devconf on %q: %w", link.Attrs().Name, err)
	}

	return nil
}

// hardenMacvlan applies macvlanDevconf and macvlanIPv6Sysctls to the macvlan in
// the current (pod) network namespace.
//
// Failing to apply the IPv4 settings is fatal, since they need nothing beyond
// the CAP_NET_ADMIN the interface was created with. The IPv6 settings are best
// effort: they need a writable /proc/sys, and when the container is not
// privileged the interface is still far more restrained than an unhardened one,
// so this warns rather than refusing to start.
func hardenMacvlan(log logr.Logger, link netlink.Link, ifaceName string) error {
	if !macvlanNameRe.MatchString(ifaceName) {
		return fmt.Errorf("refusing to harden unexpected interface name %q", ifaceName)
	}

	if err := setIPv4Devconf(link, macvlanDevconf); err != nil {
		return err
	}

	for _, s := range macvlanIPv6Sysctls {
		path := "/proc/sys/" + fmt.Sprintf(s.path, ifaceName)
		err := os.WriteFile(path, []byte(s.value+"\n"), 0o600)
		switch {
		case err == nil:
		case errors.Is(err, fs.ErrNotExist):
			// The kernel does not provide this knob, so there is nothing to
			// switch off.
			log.V(1).Info("skipping sysctl not present on this kernel", "path", path)
		case sysctlAlreadySet(path, s.value):
			log.V(1).Info("sysctl already at the required value", "path", path, "value", s.value)
		default:
			log.Info("WARNING: could not harden macvlan against IPv6 router advertisements; "+
				"a router advertisement on the physical network can install an IPv6 default route in this pod "+
				"that bypasses the CNI datapath, and SLAAC can configure a global address on the interface. "+
				"Writing /proc/sys requires a privileged container; set dhcp.macvlanPrivileged to restore it.",
				"path", path, "value", s.value, "err", err)
		}
	}

	return nil
}

// sysctlAlreadySet reports whether path already holds value, so an unwritable
// /proc/sys is only worth warning about when it would actually have changed
// something.
func sysctlAlreadySet(path, value string) bool {
	current, err := os.ReadFile(path)
	if err != nil {
		return false
	}

	return strings.TrimSpace(string(current)) == value
}

// purgePodMacvlan removes an interface named ifaceName from the current (pod)
// network namespace, if one is there.
//
// The pod's network namespace belongs to the sandbox and outlives the
// container, so an ungraceful exit — an OOM kill, a SIGKILL once the
// termination grace period expires, or a panic that skips the deferred
// teardown — leaves the macvlan behind. ifaceName is derived from the pod UID,
// which does not change when the container restarts, so that leftover holds
// exactly the name the next start needs. netlink's LinkSetNsFd sends no rename
// attribute, so the kernel refuses to move the new interface in and returns
// EEXIST; without this purge that failure repeats on every restart and only
// deleting the pod recovers it.
//
// Must be called while the OS thread is in the pod netns.
func purgePodMacvlan(log logr.Logger, ifaceName string) {
	link, err := netlink.LinkByName(ifaceName)
	if err != nil {
		// Nothing left over, which is the normal path.
		return
	}
	log.Info("removing macvlan left in pod netns by a previous run", "interface", ifaceName)
	if err := netlink.LinkDel(link); err != nil {
		log.Error(err, "failed to remove leftover macvlan from pod netns", "interface", ifaceName)
	}
}

// purgeStaleMacvlans removes all mv[0-9a-f]{13} interfaces found in the
// current (host) network namespace. Any such interface still in the host ns
// is an orphan: either a failed setup or the remnant of a pod whose netns was
// destroyed and returned the interface to its parent ns.
//
// Must be called while the OS thread is locked to the host netns.
func purgeStaleMacvlans(log logr.Logger, _, _ netns.NsHandle) {
	links, err := netlink.LinkList()
	if err != nil {
		log.Error(err, "failed to list host netns links for stale macvlan cleanup")
		return
	}
	for _, link := range links {
		name := link.Attrs().Name
		if macvlanNameRe.MatchString(name) {
			log.Info("purging stale macvlan from host netns", "interface", name)
			if err := netlink.LinkDel(link); err != nil {
				log.Error(err, "failed to purge stale macvlan", "interface", name)
			}
		}
	}
}

// teardownMacvlan removes the named macvlan interface from the pod network
// namespace. Called on graceful shutdown after s.Config.Start returns.
// The interface is in the pod ns (not host ns), so no ns switching is needed.
func teardownMacvlan(log logr.Logger, ifaceName string) {
	log.Info("removing macvlan interface on shutdown", "interface", ifaceName)
	link, err := netlink.LinkByName(ifaceName)
	if err != nil {
		// Already gone — pod netns may have been torn down first.
		return
	}
	if err := netlink.LinkDel(link); err != nil {
		log.Error(err, "failed to remove macvlan interface on shutdown", "interface", ifaceName)
	}
}

// defaultRouteInterface returns the name of the host interface that has a
// default IPv4 gateway. It mirrors the logic in autoDetectPublicIpv4WithDefaultGateway:
// a route matches when Dst is nil (kernel omitted it) or when Dst is the zero
// network AND Gw is non-nil (distinguishes a default route from a connected route).
func defaultRouteInterface() (string, error) {
	routes, err := netlink.RouteList(nil, unix.AF_INET)
	if err != nil {
		return "", fmt.Errorf("list routes: %w", err)
	}
	for _, r := range routes {
		if r.Dst == nil || (r.Dst.IP.Equal(net.IPv4(0, 0, 0, 0)) && r.Gw != nil) {
			iface, err := net.InterfaceByIndex(r.LinkIndex)
			if err != nil {
				return "", fmt.Errorf("interface by index %d: %w", r.LinkIndex, err)
			}
			return iface.Name, nil
		}
	}
	return "", fmt.Errorf("no default IPv4 route found")
}
