// Package network manages macvlan/ipvlan network interfaces used by the
// DHCP proxy server to receive broadcast DHCP packets from the host network.
// In proxy mode the DHCP server needs a Layer 2 interface attached to the
// host network namespace to see uncast/broadcast DHCP traffic.
package network

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

// interfaceType represents the type of virtual network interface.
type interfaceType string

const (
	// interfaceTypeMacvlan creates a macvlan interface in bridge mode.
	interfaceTypeMacvlan interfaceType = "macvlan"
	// interfaceTypeIPvlan creates an ipvlan interface in L2 mode.
	interfaceTypeIPvlan interfaceType = "ipvlan"

	// dhcpIfAddr is the IP assigned to the created interface.
	dhcpIfAddr = "127.1.1.1/32"

	// Leader election defaults — not user-configurable.
	defaultLeaseDuration = 15 * time.Second
	defaultRenewDeadline = 10 * time.Second
	defaultRetryPeriod   = 2 * time.Second
	defaultLockName      = "smee-dhcp-interface"
	defaultNamespace     = "default"
	// defaultIfaceType is the default interface type when none is specified.
	defaultIfaceType = interfaceTypeMacvlan

	// ifaNoPrefixRoute is the IFA_F_NOPREFIXROUTE flag value (0x200).
	// Prevents the kernel from adding a prefix route when an address is added.
	// Defined here because unix.IFA_F_NOPREFIXROUTE is only available on Linux.
	ifaNoPrefixRoute = 0x200
)

// networkInterfaceManager is the common interface for all DHCP proxy interface
// lifecycle managers (macvlan, ipvlan).
type networkInterfaceManager interface {
	Setup(ctx context.Context) error
	Cleanup() error
	Close() error
}

// NetworkManager handles the lifecycle of a macvlan/ipvlan interface.
type NetworkManager struct {
	ifaceType    interfaceType
	log          logr.Logger
	hostNs       netns.NsHandle
	currentLink  netlink.Link
	srcInterface string
}

// LeaderConfig holds configuration for leader-elected interface management.
// Only the fields that callers need to set are exported; all timing/naming
// defaults are applied internally.
type LeaderConfig struct {
	// RestConfig is the Kubernetes client configuration for leader election.
	RestConfig *rest.Config
	// Namespace for the leader election Lease resource.
	// Defaults to "default" if empty.
	Namespace string
}

// LeaderManager coordinates macvlan/ipvlan interface lifecycle with
// Kubernetes leader election. Only the elected leader creates the DHCP proxy
// interface, ensuring a single pod receives broadcast DHCP packets.
type LeaderManager struct {
	ifMgr   networkInterfaceManager
	elector *leaderelection.LeaderElector
	log     logr.Logger
}

// CheckNetworkPrivileges verifies the running container has the privileges
// required to configure a DHCP proxy network interface. It checks two
// necessary conditions:
//
//  1. The container can access the host network namespace (hostPID: true).
//  2. The process has CAP_NET_ADMIN capability.
//
// Returns a detailed, actionable error if any check fails.
func CheckNetworkPrivileges() error {
	var missing []string

	// Check 1: Access to PID 1 network namespace (requires hostPID: true).
	hostNs, err := netns.GetFromPid(1)
	if err != nil {
		missing = append(missing, fmt.Sprintf("  - cannot access host network namespace via PID 1: %v", err))
	} else {
		currentNs, nsErr := netns.Get()
		if nsErr == nil {
			if int(hostNs) == int(currentNs) {
				// Same namespace means we're already in the host network namespace —
				// this is not supported; we need an isolated container namespace.
				missing = append(missing, "  - container is already in the host network namespace; use a dedicated pod with hostPID:true instead of hostNetwork:true")
			}
			currentNs.Close()
		}
		hostNs.Close()
	}

	// Check 2: CAP_NET_ADMIN.
	if !hasNetAdminCapability() {
		missing = append(missing, "  - CAP_NET_ADMIN capability is not set")
	}

	if len(missing) == 0 {
		return nil
	}

	return fmt.Errorf(`DHCP proxy mode requires elevated container privileges but the following checks failed:

%s

To resolve, ensure your pod spec includes:
    spec:
      hostPID: true
      containers:
      - securityContext:
          capabilities:
            add: ["NET_ADMIN"]

If you have already configured a network interface in the container (e.g. via
an init container), set the DHCP bind interface explicitly to skip automatic
interface configuration.`, strings.Join(missing, "\n"))
}

// hasNetAdminCapability is implemented in capabilities_linux.go and capabilities_other.go.

// NewNetworkManager creates a new macvlan/ipvlan interface manager. It
// resolves the host network namespace via PID 1 and auto-detects the
// source interface from the default gateway.
func NewNetworkManager(log logr.Logger) (*NetworkManager, error) {
	if log.GetSink() == nil {
		log = logr.Discard()
	}

	hostNs, err := netns.GetFromPid(1)
	if err != nil {
		return nil, fmt.Errorf("getting host network namespace: %w", err)
	}

	m := &NetworkManager{
		ifaceType: defaultIfaceType,
		log:       log,
		hostNs:    hostNs,
	}

	iface, err := m.defaultGatewayInterface()
	if err != nil {
		_ = hostNs.Close()
		return nil, fmt.Errorf("detecting default gateway interface: %w", err)
	}
	m.srcInterface = iface

	log.Info("network manager initialized",
		"type", m.ifaceType,
		"srcInterface", m.srcInterface)

	return m, nil
}

// Setup creates and configures the virtual interface. It creates the interface
// in the host namespace, moves it to the container namespace, brings it up,
// and assigns the DHCP address.
func (m *NetworkManager) Setup(_ context.Context) error {
	m.log.Info("setting up DHCP proxy interface",
		"type", m.ifaceType,
		"srcInterface", m.srcInterface)

	if err := m.Cleanup(); err != nil {
		m.log.V(1).Info("cleanup of stale interfaces failed, continuing", "error", err)
	}

	if err := m.createInHost(); err != nil {
		return fmt.Errorf("creating interface in host namespace: %w", err)
	}

	if err := m.moveToContainer(); err != nil {
		return fmt.Errorf("moving interface to container namespace: %w", err)
	}

	if err := m.configureInContainer(); err != nil {
		return fmt.Errorf("configuring interface: %w", err)
	}

	if m.ifaceType == interfaceTypeIPvlan {
		if err := m.ipvlanBroadcastWorkaround(); err != nil {
			m.log.Error(err, "ipvlan broadcast workaround failed, broadcast packets may not work")
		}
	}

	m.log.Info("DHCP proxy interface ready")
	return nil
}

// Cleanup removes the virtual interface from both container and host namespaces.
func (m *NetworkManager) Cleanup() error {
	var errs []error

	names := []string{
		string(interfaceTypeMacvlan) + "0",
		string(interfaceTypeIPvlan) + "0",
		string(interfaceTypeIPvlan) + "0-wa",
	}

	for _, name := range names {
		if link, err := netlink.LinkByName(name); err == nil {
			if err := netlink.LinkDel(link); err != nil {
				errs = append(errs, fmt.Errorf("deleting container interface %s: %w", name, err))
			} else {
				m.log.V(1).Info("deleted container interface", "name", name)
			}
		}
	}

	if m.hostNs != 0 {
		if err := m.inHostNs(func() error {
			for _, name := range names {
				if link, err := netlink.LinkByName(name); err == nil {
					if err := netlink.LinkDel(link); err != nil {
						errs = append(errs, fmt.Errorf("deleting host interface %s: %w", name, err))
					} else {
						m.log.V(1).Info("deleted host interface", "name", name)
					}
				}
			}
			return nil
		}); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

// Close releases the host namespace handle.
func (m *NetworkManager) Close() error {
	if m.hostNs != 0 {
		return m.hostNs.Close()
	}
	return nil
}

// createInHost creates the virtual interface in the host network namespace.
func (m *NetworkManager) createInHost() error {
	return m.inHostNs(func() error {
		parent, err := netlink.LinkByName(m.srcInterface)
		if err != nil {
			return fmt.Errorf("finding parent interface %s: %w", m.srcInterface, err)
		}

		ifName := string(m.ifaceType) + "0"

		var link netlink.Link
		switch m.ifaceType {
		case interfaceTypeMacvlan:
			link = &netlink.Macvlan{
				LinkAttrs: netlink.LinkAttrs{
					Name:        ifName,
					ParentIndex: parent.Attrs().Index,
				},
				Mode: netlink.MACVLAN_MODE_BRIDGE,
			}
		case interfaceTypeIPvlan:
			link = &netlink.IPVlan{
				LinkAttrs: netlink.LinkAttrs{
					Name:        ifName,
					ParentIndex: parent.Attrs().Index,
				},
				Mode: netlink.IPVLAN_MODE_L2,
			}
		}

		if err := netlink.LinkAdd(link); err != nil && !errors.Is(err, unix.EEXIST) {
			return fmt.Errorf("creating %s interface: %w", m.ifaceType, err)
		}

		m.currentLink, err = netlink.LinkByName(ifName)
		if err != nil {
			return fmt.Errorf("retrieving created interface: %w", err)
		}

		m.log.V(1).Info("created interface in host namespace",
			"interface", ifName,
			"parent", m.srcInterface)

		return nil
	})
}

// moveToContainer moves the interface from host to container namespace.
func (m *NetworkManager) moveToContainer() error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	containerNs, err := netns.Get()
	if err != nil {
		return fmt.Errorf("getting container namespace: %w", err)
	}
	defer containerNs.Close()

	if err := netns.Set(m.hostNs); err != nil {
		return fmt.Errorf("switching to host namespace: %w", err)
	}
	defer func() { _ = netns.Set(containerNs) }()

	if err := netlink.LinkSetNsFd(m.currentLink, int(containerNs)); err != nil {
		// Clean up: delete the interface from the host namespace to avoid
		// leaving stale interfaces, matching the shell script's fallback.
		_ = netlink.LinkDel(m.currentLink)
		return fmt.Errorf("moving interface to container namespace: %w", err)
	}

	m.log.V(1).Info("moved interface to container namespace",
		"interface", m.currentLink.Attrs().Name)
	return nil
}

// configureInContainer brings up the interface and assigns the DHCP address.
func (m *NetworkManager) configureInContainer() error {
	ifName := string(m.ifaceType) + "0"
	link, err := netlink.LinkByName(ifName)
	if err != nil {
		return fmt.Errorf("finding interface %s in container: %w", ifName, err)
	}
	m.currentLink = link

	if err := netlink.LinkSetUp(link); err != nil {
		return fmt.Errorf("bringing interface up: %w", err)
	}

	addr, err := netlink.ParseAddr(dhcpIfAddr)
	if err != nil {
		return fmt.Errorf("parsing address %s: %w", dhcpIfAddr, err)
	}
	addr.Scope = 254 // RT_SCOPE_HOST
	addr.Flags = ifaNoPrefixRoute

	if err := netlink.AddrAdd(link, addr); err != nil && !errors.Is(err, unix.EEXIST) {
		return fmt.Errorf("adding address: %w", err)
	}

	m.log.V(1).Info("configured interface",
		"interface", ifName,
		"ip", dhcpIfAddr)

	return nil
}

// ipvlanBroadcastWorkaround creates a bridge-mode ipvlan interface in the host
// namespace to enable broadcast packet reception for ipvlan L2 mode.
// It also sends broadcast packets before and after creating the workaround
// interface to prime the kernel's broadcast forwarding path for ipvlan.
func (m *NetworkManager) ipvlanBroadcastWorkaround() error {
	m.log.V(1).Info("applying ipvlan broadcast workaround")

	// Send a broadcast packet before creating the workaround interface to
	// prime the kernel's broadcast forwarding path (matches the shell script
	// pattern that runs nmap broadcast-dhcp-discover).
	if err := m.broadcastPrime(); err != nil {
		m.log.V(1).Info("pre-creation broadcast prime failed", "error", err)
	}

	if err := m.inHostNs(func() error {
		parent, err := netlink.LinkByName(m.srcInterface)
		if err != nil {
			return fmt.Errorf("finding parent interface: %w", err)
		}

		waLink := &netlink.IPVlan{
			LinkAttrs: netlink.LinkAttrs{
				Name:        "ipvlan0-wa",
				ParentIndex: parent.Attrs().Index,
			},
			Mode: netlink.IPVLAN_MODE_L2,
			Flag: netlink.IPVLAN_FLAG_BRIDGE,
		}

		if err := netlink.LinkAdd(waLink); err != nil && !errors.Is(err, unix.EEXIST) {
			return fmt.Errorf("creating workaround interface: %w", err)
		}

		m.log.V(1).Info("created ipvlan broadcast workaround interface")
		return nil
	}); err != nil {
		return err
	}

	// Send another broadcast packet after creating the workaround interface
	// to ensure broadcast forwarding is fully activated.
	if err := m.broadcastPrime(); err != nil {
		m.log.V(1).Info("post-creation broadcast prime failed", "error", err)
	}

	return nil
}

// broadcastPrime sends a UDP broadcast packet in the host network namespace to
// prime the kernel's ipvlan broadcast forwarding path. Without this, ipvlan
// interfaces may not start receiving broadcast packets after creation.
func (m *NetworkManager) broadcastPrime() error {
	return m.inHostNs(func() error {
		conn, err := net.DialUDP("udp4", nil, &net.UDPAddr{
			IP:   net.IPv4bcast,
			Port: 67,
		})
		if err != nil {
			return fmt.Errorf("dialing broadcast for prime: %w", err)
		}
		defer conn.Close()
		if _, err := conn.Write([]byte{0}); err != nil {
			return fmt.Errorf("sending broadcast prime packet: %w", err)
		}
		m.log.V(1).Info("sent broadcast prime packet")
		return nil
	})
}

// defaultGatewayInterface returns the interface for the default route in the host namespace.
func (m *NetworkManager) defaultGatewayInterface() (string, error) {
	var ifName string
	err := m.inHostNs(func() error {
		routes, err := netlink.RouteList(nil, unix.AF_INET)
		if err != nil {
			return fmt.Errorf("listing routes: %w", err)
		}

		for _, route := range routes {
			if route.Dst == nil || route.Dst.String() == "0.0.0.0/0" {
				if route.LinkIndex > 0 {
					link, err := netlink.LinkByIndex(route.LinkIndex)
					if err != nil {
						continue
					}
					ifName = link.Attrs().Name
					return nil
				}
			}
		}
		return fmt.Errorf("no default gateway interface found")
	})
	return ifName, err
}

// inHostNs executes fn in the host network namespace, restoring the original
// namespace afterwards. It locks the OS thread for the duration.
func (m *NetworkManager) inHostNs(fn func() error) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	origNs, err := netns.Get()
	if err != nil {
		return fmt.Errorf("getting current namespace: %w", err)
	}
	defer origNs.Close()

	if err := netns.Set(m.hostNs); err != nil {
		return fmt.Errorf("switching to host namespace: %w", err)
	}
	defer func() { _ = netns.Set(origNs) }()

	return fn()
}

// WaitForInterface waits for a network interface to be up and ready.
func WaitForInterface(ctx context.Context, ifName string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("timeout waiting for interface %s: %w", ifName, ctx.Err())
		case <-ticker.C:
			link, err := netlink.LinkByName(ifName)
			if err != nil {
				continue
			}
			if link.Attrs().Flags&net.FlagUp != 0 {
				return nil
			}
		}
	}
}

// --- Leader election ---

// NewLeaderManager creates a leader-elected network interface manager.
// Only cfg.RestConfig is required; cfg.Namespace defaults to "default".
func NewLeaderManager(cfg LeaderConfig, log logr.Logger) (*LeaderManager, error) {
	if log.GetSink() == nil {
		log = logr.Discard()
	}

	if cfg.RestConfig == nil {
		return nil, fmt.Errorf("rest config is required for leader election")
	}

	ifMgr, err := NewNetworkManager(log.WithName("interface"))
	if err != nil {
		return nil, fmt.Errorf("creating network interface manager: %w", err)
	}

	lm, err := newLeaderManagerWithIfMgr(cfg, ifMgr, log)
	if err != nil {
		_ = ifMgr.Close()
		return nil, err
	}
	return lm, nil
}

// newLeaderManagerWithIfMgr creates a LeaderManager with a pre-created
// networkInterfaceManager using the standard production defaults.
// This allows tests to inject a mock interface manager.
func newLeaderManagerWithIfMgr(cfg LeaderConfig, ifMgr networkInterfaceManager, log logr.Logger) (*LeaderManager, error) {
	return newLeaderManagerWithTimings(cfg, defaultLockName, leaderIdentity(),
		defaultLeaseDuration, defaultRenewDeadline, defaultRetryPeriod,
		ifMgr, log)
}

// newLeaderManagerWithTimings creates a LeaderManager with explicit timing and
// identity parameters. Intended for use in tests to speed up leader election.
func newLeaderManagerWithTimings(cfg LeaderConfig, lockName, identity string, leaseDuration, renewDeadline, retryPeriod time.Duration, ifMgr networkInterfaceManager, log logr.Logger) (*LeaderManager, error) {
	if log.GetSink() == nil {
		log = logr.Discard()
	}

	if cfg.RestConfig == nil {
		return nil, fmt.Errorf("rest config is required for leader election")
	}

	ns := cfg.Namespace
	if ns == "" {
		ns = defaultNamespace
	}

	clientset, err := kubernetes.NewForConfig(cfg.RestConfig)
	if err != nil {
		return nil, fmt.Errorf("creating kubernetes client: %w", err)
	}

	lock := &resourcelock.LeaseLock{
		LeaseMeta: metav1.ObjectMeta{
			Name:      lockName,
			Namespace: ns,
		},
		Client: clientset.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{
			Identity: identity,
		},
	}

	lm := &LeaderManager{
		ifMgr: ifMgr,
		log:   log,
	}

	elector, err := leaderelection.NewLeaderElector(leaderelection.LeaderElectionConfig{
		Lock:          lock,
		LeaseDuration: leaseDuration,
		RenewDeadline: renewDeadline,
		RetryPeriod:   retryPeriod,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(ctx context.Context) {
				log.Info("elected as leader, setting up DHCP proxy interface")
				if err := ifMgr.Setup(ctx); err != nil {
					log.Error(err, "failed to setup interface after becoming leader")
					return
				}
				log.Info("DHCP proxy interface ready, holding until leadership is lost")
				<-ctx.Done()
			},
			OnStoppedLeading: func() {
				log.Info("lost leadership, cleaning up DHCP proxy interface")
				if err := ifMgr.Cleanup(); err != nil {
					log.Error(err, "failed to cleanup interface after losing leadership")
				}
			},
			OnNewLeader: func(id string) {
				if id == identity {
					return
				}
				log.Info("new leader elected", "leader", id)
			},
		},
		ReleaseOnCancel: true,
		Name:            lockName,
	})
	if err != nil {
		return nil, fmt.Errorf("creating leader elector: %w", err)
	}

	lm.elector = elector
	return lm, nil
}

// Start runs the leader election loop. It blocks until ctx is cancelled.
func (lm *LeaderManager) Start(ctx context.Context) error {
	lm.log.Info("starting leader election for DHCP proxy interface")
	lm.elector.Run(ctx)
	return nil
}

// Close releases all resources held by the leader manager.
func (lm *LeaderManager) Close() error {
	return lm.ifMgr.Close()
}

func leaderIdentity() string {
	if h := os.Getenv("HOSTNAME"); h != "" {
		return h
	}
	h, _ := os.Hostname()
	return h
}
