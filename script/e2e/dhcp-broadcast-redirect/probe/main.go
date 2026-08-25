// Command dhcpprobe performs a full DHCP handshake the way a PXE client does,
// over a raw packet socket from an interface with no address, and prints what
// came back.
//
// It exists for script/e2e/dhcp-broadcast-redirect, where it stands in for the
// machine being network booted: it is run in a container attached to the same
// layer 2 segment as the Kubernetes node, so the only way it can be answered is
// if the DHCP broadcast redirect carried its packets into and out of the pod.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4/nclient4"
)

func main() {
	iface := flag.String("interface", "eth0", "interface to broadcast from")
	timeout := flag.Duration("timeout", 10*time.Second, "how long to wait for each response")
	retries := flag.Int("retries", 3, "how many times to retransmit")
	flag.Parse()

	if err := run(*iface, *timeout, *retries); err != nil {
		fmt.Fprintf(os.Stderr, "FAIL: %v\n", err)
		os.Exit(1)
	}
}

func run(iface string, timeout time.Duration, retries int) error {
	client, err := nclient4.New(iface,
		nclient4.WithTimeout(timeout),
		nclient4.WithRetry(retries),
	)
	if err != nil {
		return fmt.Errorf("open a raw DHCP client on %s: %w", iface, err)
	}
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), timeout*time.Duration(retries+1)*2)
	defer cancel()

	offer, err := client.DiscoverOffer(ctx)
	if err != nil {
		return fmt.Errorf("no DHCPOFFER: %w", err)
	}
	fmt.Printf("OFFER  serverID=%v yiaddr=%s siaddr=%s bootfile=%q\n",
		offer.ServerIdentifier(), offer.YourIPAddr, offer.ServerIPAddr, offer.BootFileName)

	// A broadcast segment can carry more than one DHCP server, and the first
	// answer is not necessarily the one being tested. An offer with no address
	// in it is not an offer worth acting on.
	if offer.YourIPAddr == nil || offer.YourIPAddr.IsUnspecified() {
		return fmt.Errorf("the offer carried no address; another DHCP server (%v) answered first", offer.ServerIdentifier())
	}

	lease, err := client.RequestFromOffer(ctx, offer)
	if err != nil {
		return fmt.Errorf("no DHCPACK: %w", err)
	}
	fmt.Printf("ACK    yiaddr=%s netmask=%v router=%v dns=%v\n",
		lease.ACK.YourIPAddr, lease.ACK.SubnetMask(), lease.ACK.Router(), lease.ACK.DNS())
	fmt.Println("OK: a full DHCP handshake completed over the broadcast segment")
	return nil
}
