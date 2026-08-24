package smee

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/tinkerbell/tinkerbell/smee/internal/dhcp"
	"golang.org/x/net/ipv4"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

// fakeLock is an in-memory resourcelock.Interface. Calling steal() makes the
// lock report a different holder, which is how a real Lease looks to an
// instance that has just lost it.
type fakeLock struct {
	mu     sync.Mutex
	record *resourcelock.LeaderElectionRecord
	stolen bool
}

func (f *fakeLock) Get(context.Context) (*resourcelock.LeaderElectionRecord, []byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.record == nil {
		// Must be a typed NotFound, otherwise client-go treats it as a lookup
		// failure instead of an unclaimed lease and never calls Create.
		return nil, nil, apierrors.NewNotFound(
			schema.GroupResource{Group: "coordination.k8s.io", Resource: "leases"}, "fake")
	}
	r := *f.record
	return &r, []byte("{}"), nil
}

func (f *fakeLock) Create(_ context.Context, ler resourcelock.LeaderElectionRecord) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record = &ler
	return nil
}

func (f *fakeLock) Update(_ context.Context, ler resourcelock.LeaderElectionRecord) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stolen {
		return errors.New("lease held by another instance")
	}
	f.record = &ler
	return nil
}

func (f *fakeLock) RecordEvent(string) {}
func (f *fakeLock) Identity() string   { return "test-identity" }
func (f *fakeLock) Describe() string   { return "fake/lease" }

func (f *fakeLock) steal() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stolen = true
	f.record = &resourcelock.LeaderElectionRecord{
		HolderIdentity:       "someone-else",
		LeaseDurationSeconds: int(dhcpLeaseDuration.Seconds()),
		RenewTime:            metaNow(),
		AcquireTime:          metaNow(),
	}
}

// countingHandler records that the DHCP server was constructed and served.
type countingHandler struct {
	mu      sync.Mutex
	handled int
}

func (h *countingHandler) Handle(context.Context, *ipv4.PacketConn, dhcp.Packet) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.handled++
}

// TestLeadDHCPLeaseLossIsNotFatal is the behaviour this design turns on: losing
// the Lease must return cleanly so the caller can stand by and campaign again.
// If it returned an error instead, smee's errgroup would tear down every other
// service in the binary and a flapping Lease would CrashLoopBackOff the pod.
func TestLeadDHCPLeaseLossIsNotFatal(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	lock := &fakeLock{}
	c := &Config{}
	c.DHCP.BindAddr = netip.MustParseAddr("127.0.0.1")

	// A high port so the test needs no privileges.
	addrPort := netip.MustParseAddrPort("127.0.0.1:16767")

	done := make(chan error, 1)
	go func() {
		done <- c.leadDHCP(ctx, logr.Discard(), &countingHandler{}, addrPort, lock)
	}()

	// Wait until this instance has taken the Lease.
	waitFor(t, 30*time.Second, func() bool {
		r, _, err := lock.Get(ctx)
		return err == nil && r != nil && r.HolderIdentity == "test-identity"
	})

	lock.steal()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("leadDHCP() after losing the lease = %v, want nil so the caller can stand by", err)
		}
	case <-ctx.Done():
		t.Fatal("leadDHCP() did not return after the lease was lost")
	}
}

// TestLeadDHCPReleasesPortOnLeaseLoss checks that leadDHCP does not return until
// the DHCP server has actually stopped. The listener sets SO_REUSEPORT, so a
// second bind would silently succeed alongside the first; returning early would
// let the next campaign open a second socket while the old one still answered.
func TestLeadDHCPReleasesPortOnLeaseLoss(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	lock := &fakeLock{}
	c := &Config{}
	c.DHCP.BindAddr = netip.MustParseAddr("127.0.0.1")
	addrPort := netip.MustParseAddrPort("127.0.0.1:16768")

	done := make(chan error, 1)
	go func() {
		done <- c.leadDHCP(ctx, logr.Discard(), &countingHandler{}, addrPort, lock)
	}()

	waitFor(t, 30*time.Second, func() bool {
		r, _, err := lock.Get(ctx)
		return err == nil && r != nil && r.HolderIdentity == "test-identity"
	})
	if !udpPortBound(t, 16768) {
		t.Fatal("DHCP port is not bound while this instance holds the lease")
	}

	lock.steal()

	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("leadDHCP() did not return after the lease was lost")
	}

	// leadDHCP has returned, so serveDHCP has returned, so its deferred
	// conn.Close has run.
	if udpPortBound(t, 16768) {
		t.Fatal("DHCP port is still bound after leadDHCP returned; the socket outlived the lease")
	}
}

// TestDHCPLeaseWatchdogFiresAtRenewDeadline pins the arithmetic the watchdog
// depends on. LeaderElector.Check reports an error once the last renewal is
// older than LeaseDuration plus the tolerance it is given, so the tolerance must
// be negative to bring that forward to the renew deadline. A positive value
// would let this instance keep serving past the point where a challenger takes
// over, and both would answer DHCP.
func TestDHCPLeaseWatchdogFiresAtRenewDeadline(t *testing.T) {
	tolerance := dhcpRenewDeadline - dhcpLeaseDuration

	if got := dhcpLeaseDuration + tolerance; got != dhcpRenewDeadline {
		t.Fatalf("watchdog fires at %s, want the renew deadline %s", got, dhcpRenewDeadline)
	}
	if tolerance >= 0 {
		t.Fatalf("tolerance %s must be negative, otherwise the watchdog fires after the lease has already expired", tolerance)
	}
	// A challenger only takes over after the full lease duration, so stopping at
	// the renew deadline must leave a margin.
	if dhcpRenewDeadline >= dhcpLeaseDuration {
		t.Fatalf("renew deadline %s must be shorter than lease duration %s", dhcpRenewDeadline, dhcpLeaseDuration)
	}
}

func metaNow() metav1.Time { return metav1.Now() }

// udpPortBound reports whether any socket in this network namespace is bound to
// port on IPv4 UDP. A bind attempt cannot be used to test this: the DHCP
// listener sets SO_REUSEPORT, so a second bind would succeed regardless.
func udpPortBound(t *testing.T, port uint16) bool {
	t.Helper()

	b, err := os.ReadFile("/proc/net/udp")
	if err != nil {
		t.Skipf("cannot inspect bound UDP ports on this platform: %v", err)
	}

	// Columns are "sl local_address rem_address ...", where local_address is
	// HEXADDR:HEXPORT.
	want := fmt.Sprintf(":%04X", port)
	for _, line := range strings.Split(string(b), "\n")[1:] {
		fields := strings.Fields(line)
		if len(fields) > 1 && strings.HasSuffix(fields[1], want) {
			return true
		}
	}

	return false
}

func waitFor(t *testing.T, limit time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", limit)
}
