//go:build linux

package main

import (
	"fmt"
	"os"
	"regexp"
	"runtime"
	"strings"

	"github.com/go-logr/logr"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

// macvlanNameRe matches interface names created by this binary: "mv" + 13 lowercase hex chars.
var macvlanNameRe = regexp.MustCompile(`^mv[0-9a-f]{13}$`)

const (
	// macvlanIfacePrefix is prepended to the truncated pod UID to form the
	// interface name ("mv" + 13 hex chars = 15 chars, the kernel maximum).
	macvlanIfacePrefix = "mv"
	// macvlanAddr is the loopback-routable address assigned to the macvlan
	// interface so Smee's DHCP server can bind to it and receive broadcasts.
	macvlanAddr = "127.1.1.1/32"
	// podUIDEnv is the downward-API env var injected by the Helm chart.
	podUIDEnv = "MY_POD_UID"
	// hostNetNSPath is the host's network namespace, accessible when hostPID=true.
	hostNetNSPath = "/proc/1/ns/net"
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
// namespace, moves it into the calling process's network namespace, brings it
// up, and assigns macvlanAddr so Smee's DHCP server can bind to it.
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
func setupMacvlan(log logr.Logger, sourceIface, ifaceName string) (func(), error) {
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
		return nil, fmt.Errorf("return to pod netns: %w", err)
	}

	podLink, err := netlink.LinkByName(ifaceName)
	if err != nil {
		return nil, fmt.Errorf("find macvlan %q in pod netns: %w", ifaceName, err)
	}

	if err := netlink.LinkSetUp(podLink); err != nil {
		return nil, fmt.Errorf("bring up macvlan %q: %w", ifaceName, err)
	}

	parsed, err := netlink.ParseAddr(macvlanAddr)
	if err != nil {
		return nil, fmt.Errorf("parse macvlan addr %q: %w", macvlanAddr, err)
	}
	parsed.Flags |= unix.IFA_F_NOPREFIXROUTE

	if err := netlink.AddrAdd(podLink, parsed); err != nil {
		return nil, fmt.Errorf("assign %s to macvlan %q: %w", macvlanAddr, ifaceName, err)
	}

	log.Info("macvlan ready", "interface", ifaceName, "addr", macvlanAddr, "parent", sourceIface)

	cleanup := func() {
		teardownMacvlan(log, ifaceName)
	}
	return cleanup, nil
}

// purgeStaleMacvlans removes all mv[0-9a-f]{13} interfaces found in the
// current (host) network namespace. Any such interface still in the host ns
// is an orphan: either a failed setup or the remnant of a pod whose netns was
// destroyed and returned the interface to its parent ns.
//
// Must be called while the OS thread is locked to the host netns.
func purgeStaleMacvlans(log logr.Logger, hostNS, podNS netns.NsHandle) {
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

// defaultRouteInterface returns the interface name of the default IPv4 route
// in the current network namespace.
func defaultRouteInterface() (string, error) {
	routes, err := netlink.RouteList(nil, netlink.FAMILY_V4)
	if err != nil {
		return "", fmt.Errorf("list routes: %w", err)
	}
	for _, r := range routes {
		if r.Dst == nil {
			link, err := netlink.LinkByIndex(r.LinkIndex)
			if err != nil {
				return "", fmt.Errorf("link by index %d: %w", r.LinkIndex, err)
			}
			return link.Attrs().Name, nil
		}
	}
	return "", fmt.Errorf("no default IPv4 route found")
}
