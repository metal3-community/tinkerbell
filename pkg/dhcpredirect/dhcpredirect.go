//go:build linux

package dhcpredirect

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"runtime"
	"time"

	"github.com/ccoveille/go-safecast/v2"
	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/rlimit"
	"github.com/go-logr/logr"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

// maxFallbackPriority bounds the search for a free tc filter priority in the
// classic attach path. Anything past a handful means the ingress hook is
// already crowded and something is wrong.
const maxFallbackPriority = 8

// Counter slots in stats_map. Kept in sync with bpf/dhcp_redirect.c.
const (
	statToPodMatched = iota
	statToPodRedirected
	statToWireMatched
	statToWireRedirected
	statToWireError
	statUnconfigured
	statOtherServerReply
	statCount
)

// cfgFlagRedirectPeer is CFG_F_REDIRECT_PEER in bpf/dhcp_redirect.c.
const cfgFlagRedirectPeer = 1 << 0

// Redirector holds the loaded programs and their attachments. It stays alive
// for as long as the redirect should: closing it detaches both programs and
// unloads them.
type Redirector struct {
	log        logr.Logger
	objs       dhcpRedirectObjects
	attached   []*attachment
	hostNSPath string
	info       Info
}

// attachment is one program attached to one interface's ingress hook, by
// whichever mechanism the kernel supports.
type attachment struct {
	ifname string
	// link is set when the program was attached through TCX.
	link link.Link
	// filter is set when the program was attached as a classic tc filter,
	// which has to be removed from the host network namespace by hand.
	filter *netlink.BpfFilter
}

// Setup loads the two eBPF programs and attaches them either side of the
// pod/host boundary. It must be called from inside the pod's network
// namespace, which is where a container's main process starts out.
//
// The returned Redirector must be closed to detach. It is also safe to simply
// exit: both programs are held by file descriptors, so the kernel detaches
// them when the process goes away, however it goes away. Nothing is left
// behind in either namespace for a later run to clean up.
//
// The programs are attached through TCX at the head of each ingress hook,
// which puts them ahead of whatever the CNI has attached and is what lets DHCP
// be taken before the CNI's programs can act on it. On kernels without TCX
// (before 6.6) they are attached as classic tc filters instead, which cannot
// be ordered ahead of an existing filter; Setup logs a warning when it ends up
// behind one.
func Setup(log logr.Logger, cfg Config) (*Redirector, error) {
	// Network namespaces are a property of the OS thread, so this goroutine
	// has to stay on one for as long as it is moving between them.
	runtime.LockOSThread()
	unlock := true
	defer func() {
		if unlock {
			runtime.UnlockOSThread()
		}
	}()

	hostNSPath := cfg.HostNetNSPath
	if hostNSPath == "" {
		hostNSPath = DefaultHostNetNSPath
	}

	podNS, err := netns.Get()
	if err != nil {
		return nil, fmt.Errorf("get pod netns: %w", err)
	}
	defer podNS.Close()

	hostNS, err := openHostNetNS(hostNSPath)
	if err != nil {
		return nil, err
	}
	defer hostNS.Close()

	if podNS.Equal(hostNS) {
		return nil, errors.New("this process is already in the host network namespace; " +
			"the DHCP redirect exists to reach a pod that is not, and has nothing to do here")
	}

	info := Info{}
	if err := resolvePod(cfg.PodInterface, &info); err != nil {
		return nil, err
	}

	// Loading is namespace independent, so do it before switching: a verifier
	// error is far easier to read when it is not tangled up with a namespace
	// the process should not linger in.
	if err := rlimit.RemoveMemlock(); err != nil {
		return nil, fmt.Errorf("raise RLIMIT_MEMLOCK for BPF: %w", err)
	}

	r := &Redirector{log: log, hostNSPath: hostNSPath}
	if err := loadDhcpRedirectObjects(&r.objs, nil); err != nil {
		return nil, fmt.Errorf("load DHCP redirect BPF objects: %w", loadError(err))
	}
	// Anything that fails from here on leaves loaded programs, and possibly
	// an attachment, behind. A flag rather than an inspection of the returned
	// error keeps that true no matter how a later return is written.
	success := false
	defer func() {
		if !success {
			_ = r.Close()
		}
	}()

	if err := netns.Set(hostNS); err != nil {
		return nil, fmt.Errorf("enter host netns %s: %w", hostNSPath, err)
	}
	defer func() {
		if serr := netns.Set(podNS); serr != nil {
			// Handing a thread back to the runtime in the wrong network
			// namespace would silently move unrelated goroutines onto the
			// host's network. Strand the thread instead; it is one thread.
			log.Error(serr, "could not return to the pod network namespace; leaving this OS thread locked and unused")
			unlock = false
		}
	}()

	if err := resolveHost(cfg.PhysicalInterface, &info); err != nil {
		return nil, err
	}

	if err := r.configure(info); err != nil {
		return nil, err
	}

	if err := r.attach(&info); err != nil {
		return nil, err
	}

	r.info = info
	success = true
	return r, nil
}

// selfNetNSPath is this thread's own network namespace, used as a known good
// example of what a namespace file looks like.
const selfNetNSPath = "/proc/thread-self/ns/net"

// openHostNetNS opens the host's network namespace and checks that what came
// back really is one.
//
// Reaching another process's namespace through /proc/<pid>/ns/ needs ptrace
// read access to that process, and a container is not given it by default.
// Without it the kernel does not fail the open. It hands back the procfs
// symlink itself instead of the namespace behind it, readlink on the same path
// returns an empty string with no error, and the only sign of trouble is an
// unexplained EINVAL from setns much later. Checking here turns a baffling
// error into an actionable one.
//
// The check is the filesystem the descriptor lives on: a real namespace
// descriptor is on nsfs, the same filesystem as this thread's own namespace,
// while the consolation prize is on procfs.
func openHostNetNS(path string) (netns.NsHandle, error) {
	handle, err := netns.GetFromPath(path)
	if err != nil {
		return netns.None(), fmt.Errorf("open host netns %s: %w", path, err)
	}

	self, err := netns.GetFromPath(selfNetNSPath)
	if err != nil {
		_ = handle.Close()
		return netns.None(), fmt.Errorf("open own netns %s: %w", selfNetNSPath, err)
	}
	defer self.Close()

	same, err := onSameFilesystem(int(handle), int(self))
	if err != nil {
		_ = handle.Close()
		return netns.None(), fmt.Errorf("inspect host netns %s: %w", path, err)
	}
	if !same {
		_ = handle.Close()
		return netns.None(), fmt.Errorf(
			"%s is not a network namespace. Reading another process's namespace needs ptrace access to it, "+
				"and without that the kernel quietly returns the /proc symlink instead. Add CAP_SYS_PTRACE to "+
				"this container, or bind-mount the host's network namespace somewhere (`ip netns attach host 1` "+
				"on the node creates /run/netns/host) and point the host netns path at that instead", path)
	}

	return handle, nil
}

// onSameFilesystem reports whether two descriptors live on the same filesystem.
func onSameFilesystem(a, b int) (bool, error) {
	var sa, sb unix.Stat_t
	if err := unix.Fstat(a, &sa); err != nil {
		return false, err
	}
	if err := unix.Fstat(b, &sb); err != nil {
		return false, err
	}
	return sa.Dev == sb.Dev, nil
}

// resolvePod fills in the pod side of info. Must be called from the pod's
// network namespace.
func resolvePod(ifname string, info *Info) error {
	var err error
	if ifname == "" {
		ifname, err = DefaultRouteInterface()
		if err != nil {
			return fmt.Errorf("detect the pod's primary interface: %w", err)
		}
	}

	podLink, err := netlink.LinkByName(ifname)
	if err != nil {
		return fmt.Errorf("find pod interface %q: %w", ifname, err)
	}

	// A veth reports its far end in IFLA_LINK. Anything else — an ipvlan or
	// macvlan handed over by the CNI, or a plain device — has no far end to
	// redirect into, and the pod cannot be reached this way.
	peerIndex := podLink.Attrs().ParentIndex
	if podLink.Type() != "veth" || peerIndex == 0 {
		return fmt.Errorf("pod interface %q is a %s, not one end of a veth pair; "+
			"the DHCP redirect needs a veth to push packets into",
			ifname, podLink.Type())
	}

	// Reported for logging only. The datapath deliberately does not care which
	// address the pod replies from; see redirect_to_wire in the C source.
	addr, err := primaryIPv4(podLink)
	if err != nil {
		return err
	}
	if !addr.IsValid() {
		return fmt.Errorf("pod interface %q has no IPv4 address, so nothing in this pod can serve DHCP", ifname)
	}

	info.PodInterface = ifname
	info.PodIndex = podLink.Attrs().Index
	info.PodAddr = addr
	info.PeerIndex = peerIndex
	return nil
}

// resolveHost fills in the host side of info and checks that the peer index
// the pod reported really is the other end of the pod's veth. Must be called
// from the host's network namespace.
func resolveHost(ifname string, info *Info) error {
	var err error
	if ifname == "" {
		ifname, err = DefaultRouteInterface()
		if err != nil {
			return fmt.Errorf("detect the host's physical interface: %w", err)
		}
	}

	physLink, err := netlink.LinkByName(ifname)
	if err != nil {
		return fmt.Errorf("find host interface %q: %w", ifname, err)
	}

	peerLink, err := netlink.LinkByIndex(info.PeerIndex)
	if err != nil {
		return fmt.Errorf("find the host side of the pod's veth (index %d) in the host netns: %w", info.PeerIndex, err)
	}
	// The pod's interface named this one as its peer; this one must name the
	// pod's back. Without the round trip a stale or reused index would have us
	// redirecting DHCP into an unrelated container.
	if peerLink.Attrs().ParentIndex != info.PodIndex {
		return fmt.Errorf("host interface %q (index %d) is not the peer of pod interface %q (index %d): it points at index %d",
			peerLink.Attrs().Name, info.PeerIndex, info.PodInterface, info.PodIndex, peerLink.Attrs().ParentIndex)
	}

	addr, err := primaryIPv4(physLink)
	if err != nil {
		return err
	}

	info.PhysicalInterface = ifname
	info.PhysicalIndex = physLink.Attrs().Index
	info.PhysicalAddr = addr
	info.PhysicalMAC = physLink.Attrs().HardwareAddr
	info.PeerInterface = peerLink.Attrs().Name
	info.RedirectPeer = peerLink.Type() == "veth"
	return nil
}

// configure writes the single config map entry the programs read on every
// matching packet.
func (r *Redirector) configure(info Info) error {
	peerIndex, err := safecast.Convert[uint32](info.PeerIndex)
	if err != nil {
		return fmt.Errorf("interface index %d for %q is out of range: %w", info.PeerIndex, info.PeerInterface, err)
	}
	physIndex, err := safecast.Convert[uint32](info.PhysicalIndex)
	if err != nil {
		return fmt.Errorf("interface index %d for %q is out of range: %w", info.PhysicalIndex, info.PhysicalInterface, err)
	}

	cfg := dhcpRedirectDhcpConfig{
		HostIp:      wireOrderFromAddr(info.PhysicalAddr),
		PodIfindex:  peerIndex,
		PhysIfindex: physIndex,
	}
	if info.RedirectPeer {
		cfg.Flags |= cfgFlagRedirectPeer
	}
	copy(cfg.PhysMac[:], info.PhysicalMAC)

	if err := r.objs.ConfigMap.Put(uint32(0), &cfg); err != nil {
		return fmt.Errorf("write the DHCP redirect config map: %w", err)
	}
	return nil
}

// attach hooks both programs onto the ingress side of their interfaces.
func (r *Redirector) attach(info *Info) error {
	// Only the physical interface can be contended: the veth belongs to this
	// pod alone.
	if alreadyRedirecting(info.PhysicalIndex) {
		r.log.Info("WARNING: another DHCP broadcast redirect is already attached to this interface. "+
			"Whichever one is at the head of the hook takes the broadcasts and the other never sees them, "+
			"because a redirect consumes the packet rather than passing it on. Expect this for a few seconds "+
			"during a rolling update; if it persists, two Smee pods are running on this node and one of them "+
			"will not answer DHCP.",
			"interface", info.PhysicalInterface)
	}

	inbound, mechanism, priority, err := attachIngress(r.objs.RedirectToPod, "redirect_to_pod", info.PhysicalIndex, info.PhysicalInterface)
	if err != nil {
		return err
	}
	r.attached = append(r.attached, inbound)
	info.Attach = mechanism
	info.FallbackPriority = priority

	outbound, _, _, err := attachIngress(r.objs.RedirectToWire, "redirect_to_wire", info.PeerIndex, info.PeerInterface)
	if err != nil {
		return err
	}
	r.attached = append(r.attached, outbound)

	if info.Attach != "tcx" {
		r.log.Info("WARNING: this kernel has no TCX, so the DHCP redirect was attached as a classic tc filter "+
			"and cannot be ordered ahead of the CNI's own programs. DHCP replies leaving the pod are the ones at "+
			"risk: a CNI that consumes them before this filter runs will stop clients ever seeing an offer. "+
			"Linux 6.6 or later removes the ambiguity.",
			"interface", info.PhysicalInterface, "priority", info.FallbackPriority)
	}

	return nil
}

// alreadyRedirecting reports whether another instance of this redirect is
// already on the interface's ingress hook.
//
// Two Smee pods on one node both attach to the same physical interface, and
// only whichever is at the head of the hook sees the broadcasts, because a
// redirect consumes the packet rather than passing it along. Nothing about that
// is visible from the pod that loses, so it is worth saying out loud. Anything
// that stops this from answering is treated as "no conflict": a missing warning
// is a far smaller problem than refusing to start over a failed query.
func alreadyRedirecting(ifindex int) bool {
	result, err := link.QueryPrograms(link.QueryOptions{
		Target: ifindex,
		Attach: ebpf.AttachTCXIngress,
	})
	if err != nil || result == nil {
		return false
	}

	for _, attached := range result.Programs {
		prog, err := ebpf.NewProgramFromID(attached.ID)
		if err != nil {
			continue
		}
		pinfo, err := prog.Info()
		_ = prog.Close()
		if err == nil && pinfo.Name == kernelObjectName("redirect_to_pod") {
			return true
		}
	}
	return false
}

// kernelObjectName is the name the kernel records for a BPF object: at most
// BPF_OBJ_NAME_LEN-1 bytes, silently truncated past that.
func kernelObjectName(name string) string {
	const maxLen = 15
	if len(name) > maxLen {
		return name[:maxLen]
	}
	return name
}

// attachIngress attaches prog to an interface's ingress hook, preferring TCX
// so it can be placed ahead of the CNI's programs. It returns the attachment,
// the mechanism used ("tcx" or "tc"), and the tc filter priority when the
// classic path was taken. Must be called from the interface's netns.
func attachIngress(prog *ebpf.Program, name string, ifindex int, ifname string) (*attachment, string, int, error) {
	l, err := link.AttachTCX(link.TCXOptions{
		Program:   prog,
		Attach:    ebpf.AttachTCXIngress,
		Interface: ifindex,
		// Ahead of everything already there. On the pod's veth the CNI's own
		// program would otherwise consume the DHCP replies first.
		Anchor: link.Head(),
	})
	if err == nil {
		return &attachment{ifname: ifname, link: l}, "tcx", 0, nil
	}
	// AttachTCX probes for TCX itself and reports ErrNotSupported only when
	// the kernel has no TCX at all, so anything else is a real failure and
	// must not be papered over by quietly using the weaker mechanism.
	if !errors.Is(err, ebpf.ErrNotSupported) {
		return nil, "", 0, fmt.Errorf("attach %s to %s ingress with TCX: %w", name, ifname, err)
	}

	filter, priority, err := attachClassic(prog, name, ifindex, ifname)
	if err != nil {
		return nil, "", 0, err
	}
	return &attachment{ifname: ifname, filter: filter}, "tc", priority, nil
}

// attachClassic attaches prog as a direct-action tc filter on the interface's
// clsact ingress hook, for kernels without TCX.
func attachClassic(prog *ebpf.Program, name string, ifindex int, ifname string) (*netlink.BpfFilter, int, error) {
	// Add rather than replace: replacing an existing clsact qdisc destroys it
	// along with every filter on it, which on a CNI-managed interface means
	// tearing down the CNI's datapath.
	qdisc := &netlink.GenericQdisc{
		QdiscAttrs: netlink.QdiscAttrs{
			LinkIndex: ifindex,
			Handle:    netlink.MakeHandle(0xffff, 0),
			Parent:    netlink.HANDLE_CLSACT,
		},
		QdiscType: "clsact",
	}
	if err := netlink.QdiscAdd(qdisc); err != nil && !errors.Is(err, unix.EEXIST) {
		return nil, 0, fmt.Errorf("add clsact qdisc to %s: %w", ifname, err)
	}

	// Priority orders classic filters, lowest first, and a priority already in
	// use is refused rather than overwritten. Take the lowest free one so we
	// run as early as this mechanism allows.
	for priority := 1; priority <= maxFallbackPriority; priority++ {
		filter := &netlink.BpfFilter{
			FilterAttrs: netlink.FilterAttrs{
				LinkIndex: ifindex,
				Parent:    netlink.HANDLE_MIN_INGRESS,
				Handle:    netlink.MakeHandle(0, 1),
				Protocol:  unix.ETH_P_ALL,
				Priority:  uint16(priority),
			},
			Fd:           prog.FD(),
			Name:         name,
			DirectAction: true,
		}
		err := netlink.FilterAdd(filter)
		if err == nil {
			return filter, priority, nil
		}
		if !errors.Is(err, unix.EEXIST) {
			return nil, 0, fmt.Errorf("attach %s to %s ingress as a tc filter: %w", name, ifname, err)
		}
	}

	return nil, 0, fmt.Errorf("attach %s to %s ingress: priorities 1 to %d are all taken", name, ifname, maxFallbackPriority)
}

// Info reports the interfaces and mechanism [Setup] settled on.
func (r *Redirector) Info() Info { return r.info }

// Stats reads the eBPF counters, summing the per-CPU values.
func (r *Redirector) Stats() (Stats, error) {
	var counters [statCount]uint64
	for key := range uint32(statCount) {
		var perCPU []uint64
		if err := r.objs.StatsMap.Lookup(key, &perCPU); err != nil {
			return Stats{}, fmt.Errorf("read DHCP redirect counter %d: %w", key, err)
		}
		for _, v := range perCPU {
			counters[key] += v
		}
	}

	return Stats{
		ToPodMatched:       counters[statToPodMatched],
		ToPodRedirected:    counters[statToPodRedirected],
		ToWireMatched:      counters[statToWireMatched],
		ToWireRedirected:   counters[statToWireRedirected],
		ToWireError:        counters[statToWireError],
		Unconfigured:       counters[statUnconfigured],
		OtherServerReplies: counters[statOtherServerReply],
	}, nil
}

// LogCounters logs the packet counters whenever they change, until ctx is
// done, and once more on the way out.
//
// Quiet by design: a run with no DHCP on the network logs nothing at all, and a
// machine booting produces a line. That matters because the counters are the
// only instrument for the failure this package can have — replies that are
// generated but never reach the wire — and needing to stop the process to read
// them is no use while a machine is trying to boot.
func (r *Redirector) LogCounters(ctx context.Context, every time.Duration) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()

	var last Stats
	report := func() {
		current, err := r.Stats()
		if err != nil {
			r.log.Error(err, "failed to read the DHCP redirect counters")
			return
		}
		if current == last {
			return
		}
		last = current
		r.log.Info("DHCP broadcast redirect counters", current.LogValues()...)
	}

	for {
		select {
		case <-ctx.Done():
			report()
			return
		case <-ticker.C:
			report()
		}
	}
}

// Close detaches both programs and unloads them. It is safe to call more than
// once, and safe to skip: the kernel does the same thing when the process
// exits.
func (r *Redirector) Close() error {
	var errs []error

	// Classic tc filters are state in the host's network namespace rather than
	// something a file descriptor owns, so they have to be removed by hand and
	// from there. TCX links need neither.
	if r.needsHostNetNS() {
		errs = append(errs, r.removeClassicFilters())
	}

	for _, a := range r.attached {
		if a.link != nil {
			if err := a.link.Close(); err != nil {
				errs = append(errs, fmt.Errorf("detach from %s: %w", a.ifname, err))
			}
		}
	}
	r.attached = nil

	errs = append(errs, r.objs.Close())

	return errors.Join(errs...)
}

func (r *Redirector) needsHostNetNS() bool {
	for _, a := range r.attached {
		if a.filter != nil {
			return true
		}
	}
	return false
}

// removeClassicFilters re-enters the host network namespace to delete the tc
// filters attached there.
func (r *Redirector) removeClassicFilters() error {
	runtime.LockOSThread()
	unlock := true
	defer func() {
		if unlock {
			runtime.UnlockOSThread()
		}
	}()

	current, err := netns.Get()
	if err != nil {
		return fmt.Errorf("get current netns: %w", err)
	}
	defer current.Close()

	hostNS, err := netns.GetFromPath(r.hostNSPath)
	if err != nil {
		return fmt.Errorf("open host netns %s: %w", r.hostNSPath, err)
	}
	defer hostNS.Close()

	if err := netns.Set(hostNS); err != nil {
		return fmt.Errorf("enter host netns %s: %w", r.hostNSPath, err)
	}
	defer func() {
		if serr := netns.Set(current); serr != nil {
			r.log.Error(serr, "could not return from the host network namespace; leaving this OS thread locked and unused")
			unlock = false
		}
	}()

	var errs []error
	for _, a := range r.attached {
		if a.filter == nil {
			continue
		}
		if err := netlink.FilterDel(a.filter); err != nil && !errors.Is(err, unix.ENOENT) {
			errs = append(errs, fmt.Errorf("remove tc filter from %s: %w", a.ifname, err))
		}
	}
	return errors.Join(errs...)
}

// DefaultRouteInterface returns the name of the interface in the current
// network namespace that carries the default IPv4 route. A route matches when
// Dst is nil (the kernel omitted it) or when Dst is the zero network and Gw is
// set, which is what distinguishes a default route from a connected one.
func DefaultRouteInterface() (string, error) {
	routes, err := netlink.RouteList(nil, unix.AF_INET)
	if err != nil {
		return "", fmt.Errorf("list routes: %w", err)
	}
	for _, r := range routes {
		if r.Dst == nil || (r.Dst.IP.Equal(net.IPv4zero) && r.Gw != nil) {
			iface, err := net.InterfaceByIndex(r.LinkIndex)
			if err != nil {
				return "", fmt.Errorf("interface by index %d: %w", r.LinkIndex, err)
			}
			return iface.Name, nil
		}
	}
	return "", errors.New("no default IPv4 route found")
}

// primaryIPv4 returns the first global-scope IPv4 address on a link, or an
// invalid Addr when it has none.
func primaryIPv4(l netlink.Link) (netip.Addr, error) {
	addrs, err := netlink.AddrList(l, unix.AF_INET)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("list IPv4 addresses on %q: %w", l.Attrs().Name, err)
	}
	for _, a := range addrs {
		if a.Scope != unix.RT_SCOPE_UNIVERSE {
			continue
		}
		addr, ok := netip.AddrFromSlice(a.IP.To4())
		if ok {
			return addr, nil
		}
	}
	return netip.Addr{}, nil
}

// wireOrderFromAddr packs an IPv4 address into the uint32 the config map
// holds. The eBPF side compares it against the address as it appears in the
// packet and never byte swaps, so the four bytes go in in wire order: on a
// little-endian host that puts the first byte of the address in the low bits.
func wireOrderFromAddr(addr netip.Addr) uint32 {
	// Unmap so a v4-in-v6 address, which Is4 rejects, is still recognised.
	addr = addr.Unmap()
	if !addr.Is4() {
		return 0
	}
	b := addr.As4()
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}

// loadError unwraps a verifier error into something readable. cilium/ebpf
// truncates the verifier log in Error(), and the full log is the only thing
// that explains a rejected program.
func loadError(err error) error {
	if ve, ok := errors.AsType[*ebpf.VerifierError](err); ok {
		return fmt.Errorf("%+v", ve)
	}
	if errors.Is(err, unix.EPERM) {
		return fmt.Errorf("%w (loading BPF programs needs CAP_BPF and CAP_NET_ADMIN, "+
			"or CAP_SYS_ADMIN on kernels before 5.8)", err)
	}
	return err
}
