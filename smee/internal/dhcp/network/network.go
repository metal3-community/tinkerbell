// Package network manages macvlan/ipvlan network interfaces used by the
// DHCP proxy server to receive broadcast DHCP packets from the host network.
// In proxy mode the DHCP server needs a Layer 2 interface attached to the
// host network namespace to see uncast/broadcast DHCP traffic.
package network

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
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
	"k8s.io/klog/v2"
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
	// These are deliberately longer than the controller-runtime defaults (15s/10s/2s)
	// because the DHCP interface lifecycle is expensive to flap and the embedded
	// API server may be slow to respond during multi-component startup.
	defaultLeaseDuration = 30 * time.Second
	defaultRenewDeadline = 20 * time.Second
	defaultRetryPeriod   = 5 * time.Second
	defaultLockName      = "smee-dhcp-interface"
	defaultNamespace     = "default"
	// defaultIfaceType is the default interface type when none is specified.
	defaultIfaceType = interfaceTypeMacvlan

	// defaultStabilizePeriod is the time to wait after re-acquiring leadership
	// before setting up the interface. This prevents interface flapping when
	// leadership bounces rapidly (e.g. during API server rolling restarts).
	defaultStabilizePeriod = 10 * time.Second

	// maxSetupRetries is the maximum number of attempts to set up the network
	// interface after acquiring leadership before giving up the lease.
	maxSetupRetries = 5

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

	// OnReady is called when the DHCP proxy interface is fully configured and
	// ready to receive packets. The DHCP server should start serving when this
	// fires. May be nil.
	OnReady func()
	// OnLost is called when leadership is lost and the DHCP proxy interface
	// has been torn down. The DHCP server should stop accepting new packets
	// when this fires. May be nil.
	OnLost func()
}

// LeaderManager coordinates macvlan/ipvlan interface lifecycle with
// Kubernetes leader election. Only the elected leader creates the DHCP proxy
// interface, ensuring a single pod receives broadcast DHCP packets.
//
// LeaderManager acts as a supervisor: it notifies the DHCP server when the
// interface is ready (OnReady) and when it is torn down (OnLost), allowing
// the server to start/stop cleanly in response to leadership changes.
type LeaderManager struct {
	ifMgr       networkInterfaceManager
	electionCfg leaderelection.LeaderElectionConfig
	log         logr.Logger

	retryPeriod time.Duration
	stabilize   time.Duration
	onReady     func()
	onLost      func()

	// interfaceUp tracks whether the network interface was successfully
	// configured. OnStoppedLeading always fires (even if we never acquired
	// the lease or Setup failed), so we only call onLost/Cleanup when the
	// interface was actually brought up.
	interfaceUp atomic.Bool

	closeOnce sync.Once
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
		defaultStabilizePeriod, ifMgr, log)
}

// newLeaderManagerWithTimings creates a LeaderManager with explicit timing and
// identity parameters. Intended for use in tests to speed up leader election.
func newLeaderManagerWithTimings(cfg LeaderConfig, lockName, identity string, leaseDuration, renewDeadline, retryPeriod, stabilize time.Duration, ifMgr networkInterfaceManager, log logr.Logger) (*LeaderManager, error) {
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
		ifMgr:       ifMgr,
		log:         log,
		retryPeriod: retryPeriod,
		stabilize:   stabilize,
		onReady:     cfg.OnReady,
		onLost:      cfg.OnLost,
	}

	lm.electionCfg = leaderelection.LeaderElectionConfig{
		Lock:          lock,
		LeaseDuration: leaseDuration,
		RenewDeadline: renewDeadline,
		RetryPeriod:   retryPeriod,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: lm.onStartedLeading,
			OnStoppedLeading: lm.onStoppedLeading,
			OnNewLeader: func(id string) {
				if id == identity {
					return
				}
				log.Info("new leader elected", "leader", id)
			},
		},
		// ReleaseOnCancel is false: on context cancellation the lease is NOT
		// explicitly released. Instead it expires naturally after LeaseDuration.
		// This avoids extra API calls during chaotic shutdown (e.g. another
		// errgroup member failing) and eliminates the race between the release
		// call and the Cleanup path. For single-replica Tinkerbell deployments
		// the 30 s expiry window is acceptable.
		ReleaseOnCancel: false,
		Name:            lockName,
		// Coordinated (CLE) is deliberately not enabled. CLE is an Alpha
		// feature that requires the CoordinatedLeaderElection API-server
		// feature gate and is designed for version-aware leader selection
		// during rolling upgrades (e.g. kube-controller-manager). The DHCP
		// interface only needs simple single-leader semantics.
	}

	return lm, nil
}

// onStartedLeading is called when this instance becomes the leader.
// It retries interface setup with capped exponential backoff before
// giving up and releasing the lease.
func (lm *LeaderManager) onStartedLeading(ctx context.Context) {
	lm.log.Info("elected as leader, setting up DHCP proxy interface")

	var setupOK bool
	for attempt := range maxSetupRetries {
		if err := lm.ifMgr.Setup(ctx); err != nil {
			if ctx.Err() != nil {
				lm.log.Info("context cancelled during interface setup")
				return
			}
			backoff := lm.retryBackoff(attempt)
			lm.log.Error(err, "interface setup failed, retrying",
				"attempt", attempt+1,
				"maxAttempts", maxSetupRetries,
				"backoff", backoff)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
				continue
			}
		}
		setupOK = true
		break
	}

	if !setupOK {
		lm.log.Error(nil, "interface setup failed after all retries, releasing leadership",
			"attempts", maxSetupRetries)
		return
	}

	lm.interfaceUp.Store(true)
	lm.log.Info("DHCP proxy interface ready")
	if lm.onReady != nil {
		lm.onReady()
	}

	<-ctx.Done()
}

// onStoppedLeading is called when leadership is lost. The leaderelection
// package calls this even if the lease was never acquired (e.g. context
// cancelled during initial acquire), so we only tear down the interface
// and notify the DHCP server when it was actually brought up.
func (lm *LeaderManager) onStoppedLeading() {
	if !lm.interfaceUp.CompareAndSwap(true, false) {
		lm.log.Info("leadership callback fired but interface was never set up, skipping cleanup")
		return
	}
	lm.log.Info("lost leadership, tearing down DHCP proxy interface")
	if lm.onLost != nil {
		lm.onLost()
	}
	if err := lm.ifMgr.Cleanup(); err != nil {
		lm.log.Error(err, "failed to cleanup interface after losing leadership")
	}
}

// retryBackoff returns a capped exponential backoff duration for the given
// attempt number, based on the configured retry period.
func (lm *LeaderManager) retryBackoff(attempt int) time.Duration {
	const maxBackoff = 30 * time.Second
	backoff := time.Duration(float64(lm.retryPeriod) * math.Pow(2, float64(attempt)))
	if backoff > maxBackoff {
		backoff = maxBackoff
	}
	return backoff
}

// Start runs the leader election loop. It blocks until ctx is cancelled.
// A new LeaderElector is created for each election cycle because a
// LeaderElector's internal state becomes invalid after Run returns.
// If leadership is lost (e.g. due to a transient API server outage), the
// manager waits for a stabilization period before re-entering the election
// to prevent interface flapping.
func (lm *LeaderManager) Start(ctx context.Context) error {
	lm.log.Info("starting leader election for DHCP proxy interface")
	// Inject our structured logger so client-go leader election code that
	// calls klog.FromContext uses the Smee logger, not the global klog
	// which may be overwritten by rufio or tink-controller.
	ctx = klog.NewContext(ctx, lm.log)

	firstRun := true
	for {
		// LeaderElector.Run is single-use: a new instance must be created for
		// each election cycle. See k8s.io/client-go/tools/leaderelection docs.
		elector, err := leaderelection.NewLeaderElector(lm.electionCfg)
		if err != nil {
			return fmt.Errorf("creating leader elector: %w", err)
		}

		elector.Run(ctx)

		if ctx.Err() != nil {
			lm.log.Info("leader election stopped", "reason", ctx.Err())
			return nil
		}

		// Leadership was lost unexpectedly (API server blip, network
		// partition, etc.). Wait for a stabilization period before
		// re-entering the election to avoid interface flapping.
		delay := lm.retryPeriod
		if !firstRun {
			delay = lm.stabilize
		}
		firstRun = false

		lm.log.Info("leader election ended, waiting before re-election",
			"delay", delay)

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(delay):
		}
	}
}

// Close releases all resources held by the leader manager. Safe to call
// multiple times.
func (lm *LeaderManager) Close() error {
	var err error
	lm.closeOnce.Do(func() {
		err = lm.ifMgr.Close()
	})
	return err
}

func leaderIdentity() string {
	if h := os.Getenv("HOSTNAME"); h != "" {
		return h
	}
	h, _ := os.Hostname()
	return h
}
