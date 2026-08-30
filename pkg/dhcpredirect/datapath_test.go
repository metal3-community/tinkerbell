//go:build linux

package dhcpredirect_test

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/dhcpv4/server4"
	"github.com/tinkerbell/tinkerbell/pkg/dhcpredirect"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/net/ipv4"
	"golang.org/x/sys/unix"
)

// The three namespaces and the interfaces between them stand in for a node
// running a CNI: a client shouting on a physical segment, a host that owns the
// segment, and a pod reachable only through a veth.
//
//	client netns          host netns                      pod netns
//	┌──────────┐     ┌───────────────────────┐        ┌───────────────┐
//	│ cl0      │─────│ ph0  192.0.2.1/24     │        │               │
//	│ (no IP)  │veth │                       │        │               │
//	└──────────┘     │ lxc0 10.244.0.1/24    │────────│ eth0          │
//	                 └───────────────────────┘  veth  │ 10.244.0.5/24 │
//	                                                  └───────────────┘
const (
	nsHost   = "tbrdhost"
	nsPod    = "tbrdpod"
	nsClient = "tbrdclient"

	physIface = "ph0"
	physCIDR  = "192.0.2.1/24"
	physGW    = "192.0.2.254"
	lxcIface  = "lxc0"
	lxcCIDR   = "10.244.0.1/24"
	podIface  = "eth0"
	podCIDR   = "10.244.0.5/24"
	podGW     = "10.244.0.1"
	// A link scoped address for the source-selection case; see topoConfig.
	podLinkLocalCIDR = "169.254.1.5/16"
	cliIface         = "cl0"

	readTimeout = 5 * time.Second
)

type topology struct {
	hostNS, podNS, clientNS netns.NsHandle
	hostNSPath              string

	physMAC  net.HardwareAddr
	physAddr netip.Addr
	podAddr  netip.Addr
	podIndex int

	cliMAC   net.HardwareAddr
	cliIndex int
}

// TestRedirectCarriesDHCPBothWays is the end to end proof that the two eBPF
// programs do what the package exists to do: a broadcast DHCPDISCOVER sent on
// a segment the pod has no interface on reaches a DHCP server inside the pod,
// and the reply the pod broadcasts back reaches the client.
//
// It also pins the rewrites, which are the part that is easy to get subtly
// wrong and impossible to notice from counters alone:
//
//   - the request arrives at the server still addressed to 255.255.255.255,
//     which is the only destination the kernel will accept from a client that
//     has no address yet;
//   - the reply arrives at the client sourced from the host's address and MAC,
//     not the pod's, so nothing from the cluster network is visible on the
//     segment.
func TestRedirectCarriesDHCPBothWays(t *testing.T) {
	requirePrivileged(t)

	top := buildTopology(t)
	redirector := startRedirect(t, top)

	if info := redirector.Info(); info.Attach != "tcx" {
		t.Logf("attached as %q rather than tcx; this kernel has no TCX", info.Attach)
	}

	server := startDHCPServer(t, top)

	for _, tt := range []struct {
		name        string
		udpChecksum bool
	}{
		// Over IPv4 the UDP checksum is optional. Real clients send both, and
		// the two take different paths through the rewrite: one has a checksum
		// to repair, the other has a zero that must be left alone rather than
		// "repaired" into a wrong value.
		{name: "with a UDP checksum", udpChecksum: true},
		{name: "without a UDP checksum", udpChecksum: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			before := stats(t, redirector)

			client := openClient(t, top)
			discover, err := dhcpv4.NewDiscovery(top.cliMAC)
			if err != nil {
				t.Fatalf("build DHCPDISCOVER: %v", err)
			}
			frame := encodeFrame(top.cliMAC, broadcastMAC, netip.IPv4Unspecified(), limitedBroadcast, discover.ToBytes(), tt.udpChecksum)

			if err := client.send(frame); err != nil {
				t.Fatalf("send DHCPDISCOVER: %v", err)
			}

			// Inbound: the server must see the request at all, and must see it
			// addressed to the pod rather than to the broadcast address.
			var request receivedRequest
			select {
			case request = <-server.requests:
			case <-time.After(readTimeout):
				t.Fatalf("the DHCP server in the pod never received the broadcast; counters: %+v", stats(t, redirector))
			}
			if request.pkt.TransactionID != discover.TransactionID {
				t.Fatalf("server received xid %v, want %v", request.pkt.TransactionID, discover.TransactionID)
			}
			// Readdressing the request to the pod would make the kernel drop
			// it as a martian source, silently, for every client that has no
			// address yet. It has to arrive as the broadcast it was sent as.
			if got := request.dst; !got.Equal(net.IPv4bcast) {
				t.Errorf("request arrived addressed to %v, want %v", got, net.IPv4bcast)
			}

			// Outbound: the reply must reach the client, sourced from the host.
			reply, err := client.awaitReply(discover.TransactionID)
			if err != nil {
				t.Fatalf("client never received the offer: %v; counters: %+v", err, stats(t, redirector))
			}
			if reply.pkt.MessageType() != dhcpv4.MessageTypeOffer {
				t.Errorf("client received %v, want an OFFER", reply.pkt.MessageType())
			}
			if reply.srcIP != top.physAddr {
				t.Errorf("offer arrived from %v, want the host's address %v; the pod's address leaked onto the segment", reply.srcIP, top.physAddr)
			}
			if reply.srcMAC.String() != top.physMAC.String() {
				t.Errorf("offer arrived from MAC %v, want the host interface's %v; the pod's MAC leaked onto the segment", reply.srcMAC, top.physMAC)
			}
			// Rewriting the source address invalidates both checksums, and a
			// client that verifies them drops a reply that was repaired wrongly
			// without a word. EDK2's UDP driver verifies them.
			if !reply.ipChecksumOK {
				t.Errorf("the offer's IPv4 header checksum is wrong")
			}
			if !reply.udpChecksumOK {
				t.Errorf("the offer's UDP checksum is wrong (field %#04x)", reply.udpChecksum)
			}
			if reply.udpChecksum == 0 {
				t.Errorf("the offer carried no UDP checksum; the pod's kernel always writes one, so it was lost in transit")
			}

			after := stats(t, redirector)
			assertCounted(t, "ToPodMatched", before.ToPodMatched, after.ToPodMatched)
			assertCounted(t, "ToPodRedirected", before.ToPodRedirected, after.ToPodRedirected)
			assertCounted(t, "ToWireMatched", before.ToWireMatched, after.ToWireMatched)
			assertCounted(t, "ToWireRedirected", before.ToWireRedirected, after.ToWireRedirected)
			if after.ToWireError != before.ToWireError {
				t.Errorf("the programs reported rewrite errors: %+v", after)
			}
			if after.Unconfigured != before.Unconfigured {
				t.Errorf("the programs ran without a configuration: %+v", after)
			}
		})
	}
}

// TestOutboundChecksumsWithPodSoftwareChecksums covers the other half of the
// checksum repair.
//
// The pod's kernel normally defers the checksum to hardware, so the eBPF
// program adjusts a partially computed value; that is what the main test
// exercises. With offload off the kernel finishes the checksum before the
// packet ever reaches the program, and the repair takes a different branch of
// bpf_l4_csum_replace. Both branches have to produce something a client will
// accept.
func TestOutboundChecksumsWithPodSoftwareChecksums(t *testing.T) {
	requirePrivileged(t)

	top := buildTopology(t, withoutPodTxOffload())
	redirector := startRedirect(t, top)
	startDHCPServer(t, top)

	client := openClient(t, top)
	discover, err := dhcpv4.NewDiscovery(top.cliMAC)
	if err != nil {
		t.Fatalf("build DHCPDISCOVER: %v", err)
	}
	if err := client.send(encodeFrame(top.cliMAC, broadcastMAC, netip.IPv4Unspecified(), limitedBroadcast, discover.ToBytes(), true)); err != nil {
		t.Fatalf("send DHCPDISCOVER: %v", err)
	}

	reply, err := client.awaitReply(discover.TransactionID)
	if err != nil {
		t.Fatalf("client never received the offer: %v; counters: %+v", err, stats(t, redirector))
	}
	if !reply.ipChecksumOK {
		t.Errorf("the offer's IPv4 header checksum is wrong")
	}
	if !reply.udpChecksumOK {
		t.Errorf("the offer's UDP checksum is wrong (field %#04x)", reply.udpChecksum)
	}
	if reply.srcIP != top.physAddr {
		t.Errorf("offer arrived from %v, want %v", reply.srcIP, top.physAddr)
	}
}

// TestRedirectDoesNotCarryUnicastReplies pins a limit of the design rather than
// a bug in it, because the limit is invisible from outside.
//
// The outbound program only carries replies addressed to 255.255.255.255. Smee
// replies to the broadcast address only when the request came from 0.0.0.0; a
// client that already holds an address, or a relay that set giaddr, gets a
// unicast reply instead. That reply is not carried, falls through to the CNI,
// and on a real node is masqueraded — which rewrites the source port, so even
// if it arrives the client discards it as not being from port 67.
func TestRedirectDoesNotCarryUnicastReplies(t *testing.T) {
	requirePrivileged(t)

	top := buildTopology(t)
	redirector := startRedirect(t, top)
	server := startDHCPServer(t, top)

	before := stats(t, redirector)
	client := openClient(t, top)
	discover, err := dhcpv4.NewDiscovery(top.cliMAC)
	if err != nil {
		t.Fatalf("build DHCPDISCOVER: %v", err)
	}
	// A source address of its own is the whole difference: Smee then answers
	// the peer directly instead of broadcasting.
	held := netip.MustParseAddr("192.0.2.77")
	if err := client.send(encodeFrame(top.cliMAC, broadcastMAC, held, limitedBroadcast, discover.ToBytes(), true)); err != nil {
		t.Fatalf("send DHCPDISCOVER: %v", err)
	}

	select {
	case <-server.requests:
	case <-time.After(readTimeout):
		t.Fatalf("the request itself was not delivered; counters: %+v", stats(t, redirector))
	}

	// Give the reply every chance to appear before concluding it did not.
	_, replyErr := client.awaitReply(discover.TransactionID)
	after := stats(t, redirector)

	assertCounted(t, "ToPodMatched", before.ToPodMatched, after.ToPodMatched)
	if after.ToWireMatched != before.ToWireMatched {
		t.Errorf("the outbound program carried a unicast reply (%d -> %d); if that is now intended, "+
			"this test is what should change", before.ToWireMatched, after.ToWireMatched)
	}
	t.Logf("unicast reply reached the client: %v", replyErr == nil)
}

// TestRedirectCarriesRepliesFromAnyPodAddress is the regression test for a
// silent failure that looked exactly like the redirect not working at all:
// requests arrived, replies vanished, and no counter or log said why.
//
// The outbound program used to require the reply's source to equal an address
// the Go side had read off the pod's interface. The two sides choose by
// different rules — the Go side takes the first globally scoped address, the
// kernel's inet_select_addr() takes the first address of any scope up to link —
// so a pod that also carries an IPv4 link-local address ahead of its routable
// one replies from an address the check rejected, and every reply was dropped.
// The check is gone; this pins that it stays gone.
func TestRedirectCarriesRepliesFromAnyPodAddress(t *testing.T) {
	requirePrivileged(t)

	top := buildTopology(t, withLinkScopedPodAddress())
	redirector := startRedirect(t, top)
	server := startDHCPServer(t, top)

	before := stats(t, redirector)
	client := openClient(t, top)
	discover, err := dhcpv4.NewDiscovery(top.cliMAC)
	if err != nil {
		t.Fatalf("build DHCPDISCOVER: %v", err)
	}
	if err := client.send(encodeFrame(top.cliMAC, broadcastMAC, netip.IPv4Unspecified(), limitedBroadcast, discover.ToBytes(), true)); err != nil {
		t.Fatalf("send DHCPDISCOVER: %v", err)
	}

	select {
	case <-server.requests:
	case <-time.After(readTimeout):
		t.Fatalf("the request was not delivered; counters: %+v", stats(t, redirector))
	}

	reply, replyErr := client.awaitReply(discover.TransactionID)
	after := stats(t, redirector)
	t.Logf("Setup recorded podAddr=%v; counters %+v", redirector.Info().PodAddr, after)

	if after.ToWireMatched == before.ToWireMatched {
		t.Fatalf("the reply was not carried; the pod sourced it from an address other than the %v "+
			"Setup recorded, and something is filtering on that again", redirector.Info().PodAddr)
	}
	if replyErr != nil {
		t.Fatalf("client never received the offer: %v", replyErr)
	}
	if reply.srcIP != top.physAddr {
		t.Errorf("offer arrived from %v, want %v", reply.srcIP, top.physAddr)
	}
}

// TestOtherServerRepliesAreSeenAndNotTouched covers the counter that exists
// purely to answer a question a pod otherwise cannot: is anything else on this
// segment answering DHCP at all?
//
// In proxy mode Smee supplies boot information and a separate DHCP server
// supplies the address, so a client that never gets an address never boots no
// matter how well the redirect works. From inside a pod with no interface on
// the segment, this counter is the only way to tell the two apart.
func TestOtherServerRepliesAreSeenAndNotTouched(t *testing.T) {
	requirePrivileged(t)

	top := buildTopology(t)
	redirector := startRedirect(t, top)
	server := startDHCPServer(t, top)

	before := stats(t, redirector)

	// Something else on the segment answering a client: a broadcast reply
	// from a DHCP server that is not us.
	offer, err := dhcpv4.NewReplyFromRequest(mustDiscover(t, top.cliMAC),
		dhcpv4.WithMessageType(dhcpv4.MessageTypeOffer),
		dhcpv4.WithYourIP(net.IPv4(192, 0, 2, 50)))
	if err != nil {
		t.Fatalf("build the other server's offer: %v", err)
	}
	client := openClient(t, top)
	if err := client.send(encodeServerFrame(top.cliMAC, netip.MustParseAddr("192.0.2.9"), offer.ToBytes())); err != nil {
		t.Fatalf("send the other server's offer: %v", err)
	}

	// The counter is read after a round trip through the datapath, which the
	// pod's own exchange provides.
	discover := mustDiscover(t, top.cliMAC)
	if err := client.send(encodeFrame(top.cliMAC, broadcastMAC, netip.IPv4Unspecified(), limitedBroadcast,
		discover.ToBytes(), true)); err != nil {
		t.Fatalf("send DHCPDISCOVER: %v", err)
	}
	select {
	case <-server.requests:
	case <-time.After(readTimeout):
		t.Fatalf("the request was not delivered; counters: %+v", stats(t, redirector))
	}

	after := stats(t, redirector)
	assertCounted(t, "OtherServerReplies", before.OtherServerReplies, after.OtherServerReplies)

	// Seeing it must not mean acting on it: another server's reply belongs to
	// the segment, not to us, and redirecting it into the pod would be wrong.
	if after.ToPodRedirected-before.ToPodRedirected != 1 {
		t.Errorf("ToPodRedirected advanced by %d, want 1: something other than the request was redirected",
			after.ToPodRedirected-before.ToPodRedirected)
	}
}

func mustDiscover(t *testing.T, mac net.HardwareAddr) *dhcpv4.DHCPv4 {
	t.Helper()
	d, err := dhcpv4.NewDiscovery(mac)
	if err != nil {
		t.Fatalf("build DHCPDISCOVER: %v", err)
	}
	return d
}

// TestSetupRefusesTheHostNetworkNamespace covers the misconfiguration that
// would otherwise attach both programs to the same interface and redirect
// packets into the interface they arrived on.
func TestSetupRefusesTheHostNetworkNamespace(t *testing.T) {
	requirePrivileged(t)

	// /proc/thread-self is the namespace Setup's own netns.Get() reports, so
	// this is exactly the "you are already on the host network" case.
	_, err := dhcpredirect.Setup(logr.Discard(), dhcpredirect.Config{HostNetNSPath: "/proc/thread-self/ns/net"})
	if err == nil {
		t.Fatal("Setup() succeeded against its own network namespace, want an error")
	}
	if !containsAll(err.Error(), "host network namespace") {
		t.Fatalf("Setup() error = %q, want it to explain that this is the host network namespace", err)
	}
}

// TestSetupWarnsWhenAnotherRedirectOwnsTheHook covers what two Smee pods on one
// node do to each other: both attach to the same physical interface, only
// whichever is at the head of the hook sees the broadcasts, and the one that
// loses has no other way to find out.
func TestSetupWarnsWhenAnotherRedirectOwnsTheHook(t *testing.T) {
	requirePrivileged(t)

	top := buildTopology(t)
	first := startRedirect(t, top)
	if first.Info().Attach != "tcx" {
		t.Skip("the conflict is only detectable through TCX, which this kernel does not have")
	}

	var logged strings.Builder
	log := funcr.New(func(prefix, args string) { logged.WriteString(prefix + args + "\n") }, funcr.Options{})

	var (
		second   *dhcpredirect.Redirector
		setupErr error
	)
	if err := onThreadIn(top.podNS, func() error {
		second, setupErr = dhcpredirect.Setup(log, dhcpredirect.Config{HostNetNSPath: top.hostNSPath})
		return nil
	}); err != nil {
		t.Fatalf("run Setup: %v", err)
	}
	if setupErr != nil {
		t.Fatalf("second Setup() error = %v", setupErr)
	}
	t.Cleanup(func() { _ = second.Close() })

	if !strings.Contains(logged.String(), "another DHCP broadcast redirect is already attached") {
		t.Fatalf("the second Setup() did not warn about the first; logged:\n%s", logged.String())
	}
}

// TestSetupExplainsAnUnreadableHostNetNS covers the failure mode that is
// otherwise impossible to diagnose. Opening another process's namespace needs
// ptrace access to it; without that the open succeeds and returns the /proc
// symlink rather than the namespace, and the only later symptom is EINVAL from
// setns. Setup has to recognise it and say what to do about it.
func TestSetupExplainsAnUnreadableHostNetNS(t *testing.T) {
	notANamespace := filepath.Join(t.TempDir(), "net")
	if err := os.WriteFile(notANamespace, nil, 0o600); err != nil {
		t.Fatalf("create a stand-in for the /proc symlink: %v", err)
	}

	_, err := dhcpredirect.Setup(logr.Discard(), dhcpredirect.Config{HostNetNSPath: notANamespace})
	if err == nil {
		t.Fatal("Setup() accepted a path that is not a network namespace, want an error")
	}
	if !containsAll(err.Error(), "not a network namespace", "CAP_SYS_PTRACE") {
		t.Fatalf("Setup() error = %q, want it to name the problem and the capability that fixes it", err)
	}
}

// TestSetupRefusesANonVethPodInterface covers the other CNI shapes: an ipvlan
// or macvlan handed to the pod has no peer to redirect into, and saying so is
// far better than attaching programs that silently drop every packet.
func TestSetupRefusesANonVethPodInterface(t *testing.T) {
	requirePrivileged(t)

	newNamedNS(t, nsHost+"nv")
	podNS := newNamedNS(t, nsPod+"nv")

	if err := onThreadIn(podNS, func() error {
		if err := netlink.LinkAdd(&netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "dummy0"}}); err != nil {
			return fmt.Errorf("add dummy0: %w", err)
		}
		return addrUp("dummy0", "10.0.0.2/24")
	}); err != nil {
		t.Fatalf("prepare pod netns: %v", err)
	}

	var setupErr error
	if err := onThreadIn(podNS, func() error {
		_, setupErr = dhcpredirect.Setup(logr.Discard(), dhcpredirect.Config{
			PodInterface:  "dummy0",
			HostNetNSPath: "/run/netns/" + nsHost + "nv",
		})
		return nil
	}); err != nil {
		t.Fatalf("run Setup: %v", err)
	}

	if setupErr == nil {
		t.Fatal("Setup() accepted a dummy interface, want an error")
	}
	if !containsAll(setupErr.Error(), "veth") {
		t.Fatalf("Setup() error = %q, want it to name the veth requirement", setupErr)
	}
}

// --- topology ---------------------------------------------------------------

// topoConfig varies the parts of the topology the investigation needs to poke
// at. The zero value is the ordinary case.
type topoConfig struct {
	// linkScopedPodAddress puts a link scoped IPv4 on the pod interface ahead
	// of the routable one, which is what an environment that also does IPv4
	// link-local addressing ends up with.
	linkScopedPodAddress bool
	// noPodTxOffload makes the pod compute its checksums in software, so the
	// eBPF program sees a finished checksum rather than a deferred one.
	noPodTxOffload bool
}

type topoOpt func(*topoConfig)

func withLinkScopedPodAddress() topoOpt { return func(c *topoConfig) { c.linkScopedPodAddress = true } }
func withoutPodTxOffload() topoOpt      { return func(c *topoConfig) { c.noPodTxOffload = true } }

func buildTopology(t *testing.T, opts ...topoOpt) *topology {
	t.Helper()

	var cfg topoConfig
	for _, o := range opts {
		o(&cfg)
	}

	top := &topology{
		hostNSPath: "/run/netns/" + nsHost,
	}
	top.hostNS = newNamedNS(t, nsHost)
	top.podNS = newNamedNS(t, nsPod)
	top.clientNS = newNamedNS(t, nsClient)

	// Everything is created in the host namespace and the far ends are handed
	// out, which is exactly how a CNI builds a pod's network.
	if err := onThreadIn(top.hostNS, func() error {
		for _, pair := range [][2]string{{physIface, cliIface}, {lxcIface, podIface}} {
			if err := netlink.LinkAdd(&netlink.Veth{
				LinkAttrs: netlink.LinkAttrs{Name: pair[0]},
				PeerName:  pair[1],
			}); err != nil {
				return fmt.Errorf("create veth %s/%s: %w", pair[0], pair[1], err)
			}
		}
		for _, move := range []struct {
			name string
			ns   netns.NsHandle
		}{{cliIface, top.clientNS}, {podIface, top.podNS}} {
			l, err := netlink.LinkByName(move.name)
			if err != nil {
				return fmt.Errorf("find %s: %w", move.name, err)
			}
			if err := netlink.LinkSetNsFd(l, int(move.ns)); err != nil {
				return fmt.Errorf("move %s: %w", move.name, err)
			}
		}
		if err := addrUp(physIface, physCIDR); err != nil {
			return err
		}
		// Stand in for a NIC: finalise deferred checksums on the way out so the
		// client sees what a real client would. See setTxChecksumOffload.
		if err := setTxChecksumOffload(physIface, false); err != nil {
			return err
		}
		if err := addrUp(lxcIface, lxcCIDR); err != nil {
			return err
		}
		// A default route so Setup's auto-detection has something to find,
		// which is the path a real deployment takes.
		if err := defaultRoute(physIface, physGW); err != nil {
			return err
		}
		l, err := netlink.LinkByName(physIface)
		if err != nil {
			return err
		}
		top.physMAC = l.Attrs().HardwareAddr
		top.physAddr = netip.MustParsePrefix(physCIDR).Addr()
		return nil
	}); err != nil {
		t.Fatalf("build host netns: %v", err)
	}

	if err := onThreadIn(top.podNS, func() error {
		// Added first so it sits ahead of the routable address in the kernel's
		// list, which is what decides the source address of a broadcast.
		if cfg.linkScopedPodAddress {
			if err := addScopedAddr(podIface, podLinkLocalCIDR, unix.RT_SCOPE_LINK); err != nil {
				return err
			}
		}
		if err := addrUp(podIface, podCIDR); err != nil {
			return err
		}
		if cfg.noPodTxOffload {
			if err := setTxChecksumOffload(podIface, false); err != nil {
				return err
			}
		}
		if err := defaultRoute(podIface, podGW); err != nil {
			return err
		}
		l, err := netlink.LinkByName(podIface)
		if err != nil {
			return err
		}
		top.podIndex = l.Attrs().Index
		top.podAddr = netip.MustParsePrefix(podCIDR).Addr()
		return nil
	}); err != nil {
		t.Fatalf("build pod netns: %v", err)
	}

	if err := onThreadIn(top.clientNS, func() error {
		if err := addrUp(cliIface, ""); err != nil {
			return err
		}
		l, err := netlink.LinkByName(cliIface)
		if err != nil {
			return err
		}
		top.cliMAC = l.Attrs().HardwareAddr
		top.cliIndex = l.Attrs().Index
		return nil
	}); err != nil {
		t.Fatalf("build client netns: %v", err)
	}

	return top
}

func startRedirect(t *testing.T, top *topology) *dhcpredirect.Redirector {
	t.Helper()

	log := funcr.New(func(prefix, args string) { t.Logf("%s%s", prefix, args) }, funcr.Options{Verbosity: 1})

	var (
		redirector *dhcpredirect.Redirector
		setupErr   error
	)
	if err := onThreadIn(top.podNS, func() error {
		// Interfaces deliberately left empty: auto-detection from the default
		// routes either side is what a deployment relies on.
		redirector, setupErr = dhcpredirect.Setup(log, dhcpredirect.Config{HostNetNSPath: top.hostNSPath})
		return nil
	}); err != nil {
		t.Fatalf("run Setup: %v", err)
	}
	if setupErr != nil {
		t.Fatalf("Setup() error = %v", setupErr)
	}
	t.Cleanup(func() {
		if err := redirector.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})

	info := redirector.Info()
	if info.PhysicalInterface != physIface {
		t.Fatalf("Setup() picked physical interface %q, want %q", info.PhysicalInterface, physIface)
	}
	if info.PodInterface != podIface {
		t.Fatalf("Setup() picked pod interface %q, want %q", info.PodInterface, podIface)
	}
	if info.PeerInterface != lxcIface {
		t.Fatalf("Setup() picked peer interface %q, want %q", info.PeerInterface, lxcIface)
	}
	t.Logf("redirect ready: %v", info.LogValues())
	return redirector
}

// --- the DHCP server inside the pod ------------------------------------------

type receivedRequest struct {
	pkt *dhcpv4.DHCPv4
	dst net.IP
}

type dhcpServer struct {
	requests chan receivedRequest
}

// startDHCPServer runs a DHCP server in the pod namespace the same way Smee
// does: a UDP socket, IP_PKTINFO for the arrival interface, and replies
// broadcast back out of that interface.
func startDHCPServer(t *testing.T, top *topology) *dhcpServer {
	t.Helper()

	var conn *ipv4.PacketConn
	if err := onThreadIn(top.podNS, func() error {
		// The socket belongs to the namespace it was created in, not to the
		// thread, so it keeps working once this thread is gone.
		c, err := server4.NewIPv4UDPConn("", &net.UDPAddr{Port: 67})
		if err != nil {
			return fmt.Errorf("listen on :67: %w", err)
		}
		conn = ipv4.NewPacketConn(c)
		return conn.SetControlMessage(ipv4.FlagInterface|ipv4.FlagDst, true)
	}); err != nil {
		t.Fatalf("start the DHCP server: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	server := &dhcpServer{requests: make(chan receivedRequest, 4)}
	go func() {
		buf := make([]byte, 4096)
		for {
			n, cm, peer, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}
			pkt, err := dhcpv4.FromBytes(buf[:n])
			if err != nil {
				continue
			}
			server.requests <- receivedRequest{pkt: pkt, dst: cm.Dst}

			reply, err := dhcpv4.NewReplyFromRequest(pkt,
				dhcpv4.WithMessageType(dhcpv4.MessageTypeOffer),
				dhcpv4.WithYourIP(net.IPv4(192, 0, 2, 50)),
				dhcpv4.WithServerIP(net.IPv4(192, 0, 2, 1)),
			)
			if err != nil {
				continue
			}
			// Exactly what Smee does: reply to the peer, but substitute the
			// broadcast address when the client had none of its own. See the
			// peer rewrite in smee/internal/dhcp/server and replyDestination.
			dst := &net.UDPAddr{IP: net.IPv4bcast, Port: 68}
			if u, ok := peer.(*net.UDPAddr); ok && u.IP != nil && !u.IP.To4().Equal(net.IPv4zero) {
				dst = &net.UDPAddr{IP: u.IP, Port: u.Port}
			}
			_, _ = conn.WriteTo(reply.ToBytes(), &ipv4.ControlMessage{IfIndex: cm.IfIndex}, dst)
		}
	}()

	return server
}

// --- the client on the physical segment --------------------------------------

type rawClient struct {
	fd    int
	index int
}

type receivedReply struct {
	pkt    *dhcpv4.DHCPv4
	srcIP  netip.Addr
	srcMAC net.HardwareAddr

	// Checksums as they arrived. The outbound program rewrites the source
	// address, which both of these cover, and a receiver that verifies them —
	// EDK2's UDP driver does — drops the reply if the repair was wrong.
	ipChecksumOK  bool
	udpChecksum   uint16
	udpChecksumOK bool
}

// openClient opens a packet socket in the client namespace, which is the only
// way to send from 0.0.0.0 before an address has been handed out and the only
// way to see what the reply actually looked like on the wire.
func openClient(t *testing.T, top *topology) *rawClient {
	t.Helper()

	client := &rawClient{index: top.cliIndex}
	if err := onThreadIn(top.clientNS, func() error {
		fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW|unix.SOCK_CLOEXEC, int(htons(unix.ETH_P_IP)))
		if err != nil {
			return fmt.Errorf("open packet socket: %w", err)
		}
		if err := unix.Bind(fd, &unix.SockaddrLinklayer{
			Protocol: htons(unix.ETH_P_IP),
			Ifindex:  top.cliIndex,
		}); err != nil {
			_ = unix.Close(fd)
			return fmt.Errorf("bind packet socket to %s: %w", cliIface, err)
		}
		tv := unix.NsecToTimeval(int64(readTimeout))
		if err := unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv); err != nil {
			_ = unix.Close(fd)
			return fmt.Errorf("set receive timeout: %w", err)
		}
		client.fd = fd
		return nil
	}); err != nil {
		t.Fatalf("open the client socket: %v", err)
	}
	t.Cleanup(func() { _ = unix.Close(client.fd) })

	return client
}

func (c *rawClient) send(frame []byte) error {
	return unix.Sendto(c.fd, frame, 0, &unix.SockaddrLinklayer{
		Protocol: htons(unix.ETH_P_IP),
		Ifindex:  c.index,
		Halen:    6,
		Addr:     [8]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff},
	})
}

// awaitReply reads frames until it finds the DHCP reply for xid, or the socket
// timeout expires.
func (c *rawClient) awaitReply(xid dhcpv4.TransactionID) (receivedReply, error) {
	buf := make([]byte, 2048)
	deadline := time.Now().Add(readTimeout)
	for time.Now().Before(deadline) {
		n, _, err := unix.Recvfrom(c.fd, buf, 0)
		if err != nil {
			if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EINTR) {
				continue
			}
			return receivedReply{}, fmt.Errorf("read from the packet socket: %w", err)
		}
		reply, ok := decodeReply(buf[:n], xid)
		if ok {
			return reply, nil
		}
	}
	return receivedReply{}, errors.New("timed out")
}

// --- frame encoding and decoding ---------------------------------------------

var (
	broadcastMAC     = net.HardwareAddr{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}
	limitedBroadcast = netip.MustParseAddr("255.255.255.255")
)

const (
	ethHdrLen = 14
	ipHdrLen  = 20
	udpHdrLen = 8
)

// encodeFrame builds a complete Ethernet frame carrying a DHCP request: UDP
// over IPv4 from the client port to the server port. The UDP checksum is
// optional over IPv4, and withUDPChecksum selects between a computed one and
// the zero that means "none".
func encodeFrame(srcMAC, dstMAC net.HardwareAddr, srcIP, dstIP netip.Addr, payload []byte, withUDPChecksum bool) []byte {
	const srcPort, dstPort = 68, 67

	frame := make([]byte, ethHdrLen+ipHdrLen+udpHdrLen+len(payload))

	copy(frame[0:6], dstMAC)
	copy(frame[6:12], srcMAC)
	binary.BigEndian.PutUint16(frame[12:14], unix.ETH_P_IP)

	ip := frame[ethHdrLen : ethHdrLen+ipHdrLen]
	ip[0] = 0x45 // IPv4, 5 word header
	binary.BigEndian.PutUint16(ip[2:4], uint16(ipHdrLen+udpHdrLen+len(payload)))
	ip[8] = 64 // TTL
	ip[9] = unix.IPPROTO_UDP
	src4, dst4 := srcIP.As4(), dstIP.As4()
	copy(ip[12:16], src4[:])
	copy(ip[16:20], dst4[:])
	binary.BigEndian.PutUint16(ip[10:12], onesComplement(sum(ip, 0)))

	udp := frame[ethHdrLen+ipHdrLen:]
	binary.BigEndian.PutUint16(udp[0:2], srcPort)
	binary.BigEndian.PutUint16(udp[2:4], dstPort)
	binary.BigEndian.PutUint16(udp[4:6], uint16(udpHdrLen+len(payload)))
	copy(udp[udpHdrLen:], payload)
	if withUDPChecksum {
		binary.BigEndian.PutUint16(udp[6:8], onesComplement(sum(udp, pseudoHeaderSum(src4, dst4, uint16(udpHdrLen+len(payload))))))
	}

	return frame
}

// encodeServerFrame builds a broadcast DHCP reply as another server on the
// segment would send it: from port 67 to port 68.
func encodeServerFrame(dstMAC net.HardwareAddr, srcIP netip.Addr, payload []byte) []byte {
	frame := encodeFrame(net.HardwareAddr{0x02, 0, 0, 0, 0, 0x09}, broadcastMAC, srcIP, limitedBroadcast, payload, true)
	binary.BigEndian.PutUint16(frame[ethHdrLen+ipHdrLen+0:], 67)
	binary.BigEndian.PutUint16(frame[ethHdrLen+ipHdrLen+2:], 68)
	// The UDP checksum covered the old ports, and a wrong one would be
	// rejected before the program ever saw the packet.
	udp := frame[ethHdrLen+ipHdrLen:]
	binary.BigEndian.PutUint16(udp[6:8], 0)
	src4, dst4 := srcIP.As4(), limitedBroadcast.As4()
	binary.BigEndian.PutUint16(udp[6:8], onesComplement(sum(udp, pseudoHeaderSum(src4, dst4, uint16(len(udp))))))
	_ = dstMAC
	return frame
}

// decodeReply pulls a DHCP reply for xid out of a frame, reporting false for
// anything else on the segment.
func decodeReply(frame []byte, xid dhcpv4.TransactionID) (receivedReply, bool) {
	if len(frame) < ethHdrLen+ipHdrLen+udpHdrLen {
		return receivedReply{}, false
	}
	if binary.BigEndian.Uint16(frame[12:14]) != unix.ETH_P_IP {
		return receivedReply{}, false
	}
	ip := frame[ethHdrLen:]
	ihl := int(ip[0]&0x0f) * 4
	if ihl < ipHdrLen || len(ip) < ihl+udpHdrLen || ip[9] != unix.IPPROTO_UDP {
		return receivedReply{}, false
	}
	udp := ip[ihl:]
	if binary.BigEndian.Uint16(udp[0:2]) != 67 || binary.BigEndian.Uint16(udp[2:4]) != 68 {
		return receivedReply{}, false
	}
	length := int(binary.BigEndian.Uint16(udp[4:6]))
	if length < udpHdrLen || len(udp) < length {
		return receivedReply{}, false
	}
	pkt, err := dhcpv4.FromBytes(udp[udpHdrLen:length])
	if err != nil || pkt.TransactionID != xid {
		return receivedReply{}, false
	}

	var src4, dst4 [4]byte
	copy(src4[:], ip[12:16])
	copy(dst4[:], ip[16:20])

	// Summing a correct checksum together with the field holding it folds to
	// all ones, which complements to zero.
	udpChecksum := binary.BigEndian.Uint16(udp[6:8])
	udpChecksumOK := true
	if udpChecksum != 0 {
		udpChecksumOK = onesComplement(sum(udp[:length], pseudoHeaderSum(src4, dst4, uint16(length)))) == 0
	}

	srcIP, _ := netip.AddrFromSlice(ip[12:16])
	return receivedReply{
		pkt:           pkt,
		srcIP:         srcIP,
		srcMAC:        net.HardwareAddr(append([]byte(nil), frame[6:12]...)),
		ipChecksumOK:  onesComplement(sum(ip[:ihl], 0)) == 0,
		udpChecksum:   udpChecksum,
		udpChecksumOK: udpChecksumOK,
	}, true
}

func pseudoHeaderSum(src, dst [4]byte, length uint16) uint32 {
	return sum(src[:], 0) + sum(dst[:], 0) + uint32(unix.IPPROTO_UDP) + uint32(length)
}

func sum(b []byte, initial uint32) uint32 {
	total := initial
	for i := 0; i+1 < len(b); i += 2 {
		total += uint32(binary.BigEndian.Uint16(b[i : i+2]))
	}
	if len(b)%2 == 1 {
		total += uint32(b[len(b)-1]) << 8
	}
	return total
}

func onesComplement(total uint32) uint16 {
	for total>>16 != 0 {
		total = total&0xffff + total>>16
	}
	return ^uint16(total)
}

func htons(v uint16) uint16 { return v<<8 | v>>8 }

// ETHTOOL_STXCSUM, which still maps onto the modern tx-checksum features.
const ethtoolSTxCsum = 0x17

type ethtoolValue struct {
	cmd  uint32
	data uint32
}

// ifreqEthtool is struct ifreq with its union holding an ethtool command
// pointer: 16 bytes of name and 24 bytes of union.
type ifreqEthtool struct {
	name [16]byte
	data *ethtoolValue
	_    [16]byte
}

// setTxChecksumOffload turns an interface's transmit checksum offload on or
// off. Must be called from the interface's namespace.
//
// The tests need this because a veth is not a NIC. A real NIC finalises a
// deferred (CHECKSUM_PARTIAL) checksum in hardware as the frame leaves, so a
// receiver always sees a complete one. A veth advertises checksum offload and
// then does nothing, so the deferred value travels all the way to the receiver
// and there is nothing there to check. Turning the offload off makes the kernel
// finalise it in software on the way out, which is the same thing a NIC does
// and the only way to see on the wire what a real client would see.
func setTxChecksumOffload(name string, enabled bool) error {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open a socket for ethtool: %w", err)
	}
	defer unix.Close(fd)

	value := ethtoolValue{cmd: ethtoolSTxCsum}
	if enabled {
		value.data = 1
	}
	req := ifreqEthtool{data: &value}
	copy(req.name[:], name)

	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), unix.SIOCETHTOOL, uintptr(unsafe.Pointer(&req)))
	runtime.KeepAlive(value)
	if errno != 0 {
		return fmt.Errorf("set tx checksum offload on %s: %w", name, errno)
	}
	return nil
}

// addScopedAddr adds an address with an explicit scope, which addrUp cannot do.
func addScopedAddr(name, cidr string, scope int) error {
	l, err := netlink.LinkByName(name)
	if err != nil {
		return fmt.Errorf("find %s: %w", name, err)
	}
	addr, err := netlink.ParseAddr(cidr)
	if err != nil {
		return fmt.Errorf("parse %s: %w", cidr, err)
	}
	addr.Scope = scope
	if err := netlink.AddrAdd(l, addr); err != nil {
		return fmt.Errorf("add %s to %s: %w", cidr, name, err)
	}
	return nil
}

// --- namespace plumbing -------------------------------------------------------

// onThreadIn runs fn on an OS thread of its own, moved into ns and moved back
// afterwards.
//
// Putting the thread back matters more than it looks: the goroutine may be
// running on the process's main thread, and that thread is the one /proc/self
// reports on and the one the runtime cannot discard. Leaving it in another
// namespace changes what unrelated code sees for the rest of the run. A thread
// that cannot be moved back is left locked instead, so the runtime never hands
// it to another goroutine.
func onThreadIn(ns netns.NsHandle, fn func() error) error {
	done := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		var (
			err      error
			restored bool
		)
		defer func() {
			if restored {
				runtime.UnlockOSThread()
			}
			done <- err
		}()

		orig, oerr := netns.Get()
		if oerr != nil {
			err = fmt.Errorf("get current netns: %w", oerr)
			return
		}
		defer func() {
			restored = netns.Set(orig) == nil
			_ = orig.Close()
		}()

		if err = netns.Set(ns); err != nil {
			err = fmt.Errorf("enter netns: %w", err)
			return
		}
		err = fn()
	}()
	return <-done
}

func newNamedNS(t *testing.T, name string) netns.NsHandle {
	t.Helper()

	// A previous run that was killed rather than cleaned up leaves the mount
	// behind, and creating it again would fail.
	_ = netns.DeleteNamed(name)

	var (
		handle netns.NsHandle
		err    error
	)
	done := make(chan struct{})
	go func() {
		// NewNamed unshares the calling thread into the new namespace, so the
		// same restore rule as onThreadIn applies.
		runtime.LockOSThread()
		restored := false
		defer func() {
			if restored {
				runtime.UnlockOSThread()
			}
			close(done)
		}()

		orig, oerr := netns.Get()
		if oerr != nil {
			err = fmt.Errorf("get current netns: %w", oerr)
			return
		}
		defer func() {
			restored = netns.Set(orig) == nil
			_ = orig.Close()
		}()

		handle, err = netns.NewNamed(name)
	}()
	<-done
	if err != nil {
		t.Fatalf("create netns %q: %v", name, err)
	}

	t.Cleanup(func() {
		_ = handle.Close()
		if err := netns.DeleteNamed(name); err != nil {
			t.Logf("delete netns %q: %v", name, err)
		}
	})
	return handle
}

// addrUp brings an interface up, optionally giving it an address first. Must be
// called from the interface's namespace.
func addrUp(name, cidr string) error {
	l, err := netlink.LinkByName(name)
	if err != nil {
		return fmt.Errorf("find %s: %w", name, err)
	}
	if cidr != "" {
		addr, err := netlink.ParseAddr(cidr)
		if err != nil {
			return fmt.Errorf("parse %s: %w", cidr, err)
		}
		if err := netlink.AddrAdd(l, addr); err != nil {
			return fmt.Errorf("add %s to %s: %w", cidr, name, err)
		}
	}
	if err := netlink.LinkSetUp(l); err != nil {
		return fmt.Errorf("bring up %s: %w", name, err)
	}
	return nil
}

func defaultRoute(name, gateway string) error {
	l, err := netlink.LinkByName(name)
	if err != nil {
		return fmt.Errorf("find %s: %w", name, err)
	}
	if err := netlink.RouteAdd(&netlink.Route{
		LinkIndex: l.Attrs().Index,
		Gw:        net.ParseIP(gateway),
	}); err != nil {
		return fmt.Errorf("add default route via %s on %s: %w", gateway, name, err)
	}
	return nil
}

// --- assorted helpers ---------------------------------------------------------

// requirePrivileged skips when the process cannot do what these tests need:
// create network namespaces, and load and attach BPF programs. BPF capabilities
// are checked against the initial user namespace, so `unshare -r` is not
// enough; the tests need real root, which in practice means a privileged
// container or a VM.
func requirePrivileged(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("needs root to create network namespaces and load BPF programs; " +
			"run under `docker run --privileged` or as root")
	}
}

func stats(t *testing.T, r *dhcpredirect.Redirector) dhcpredirect.Stats {
	t.Helper()
	s, err := r.Stats()
	if err != nil {
		t.Fatalf("Stats() error = %v", err)
	}
	return s
}

func assertCounted(t *testing.T, name string, before, after uint64) {
	t.Helper()
	if after <= before {
		t.Errorf("%s did not advance: %d then %d", name, before, after)
	}
}

func containsAll(haystack string, needles ...string) bool {
	for _, n := range needles {
		if !strings.Contains(haystack, n) {
			return false
		}
	}
	return true
}
