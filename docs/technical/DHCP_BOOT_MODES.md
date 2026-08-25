# DHCP Boot Modes

This document explains the different DHCP boot modes available in Tinkerbell and how to configure them.

## Overview

As requirements across users can vary, flexibility in DHCP boot modes is essential for accommodating different network environments and client needs.
Tinkerbell provides several DHCP boot modes to cater to these requirements. The modes include DHCP Reservation, Proxy DHCP, Auto Proxy DHCP, and DHCP disabled. Each mode has its own use case and configuration options.

## DHCP Modes

### DHCP Reservation

This mode is used to provide IP addresses and next boot information to clients based on their MAC addresses. In this mode, the IP address is reservation-based, meaning there must be a corresponding Hardware object for the requesting client's MAC address.

#### DHCP Reservation Configuration

This is the default mode. To explicitly enable this mode use the CLI flag `--dhcp-mode=reservation` or the environment variable `TINKERBELL_DHCP_MODE=reservation`.

### Proxy DHCP

This mode is used to provide next boot information to clients. In this mode, a Hardware object must exist for the requesting client's MAC address. In this mode Tinkerbell does NOT provide IP addresses to clients, it only provides next boot information. A DHCP server on the network must be configured to provide IP addresses to clients. Tinkerbell requires Layer 2 access to machines or a DHCP relay agent that will forward DHCP requests to Tinkerbell.

#### Proxy DHCP Configuration

To enable this mode set the CLI flag `--dhcp-mode=proxy` or the environment variable `TINKERBELL_DHCP_MODE=proxy`.

### Auto Proxy DHCP

This mode is used to provide next boot information to clients without requiring a pre-existing Hardware object. In this mode, Tinkerbell will respond to PXE enabled DHCP requests from clients and provide them with next boot info when network booting. All network booting clients will be provided the next boot info for the iPXE binary. When a client needs an iPXE script, if no corresponding Hardware object is found for the requesting client's MAC address, Tinkerbell will provide the client with a statically defined iPXE script. If a Hardware record is found, then the normal `auto.ipxe` script will be served. In this mode Tinkerbell does NOT provide IP addresses to clients, it only provides next boot information. A DHCP server on the network must be configured to provide IP addresses to clients. Tinkerbell requires Layer 2 access to machines or a DHCP relay agent that will forward DHCP requests to Tinkerbell.

#### Auto Proxy DHCP Configuration

To enable this mode set the CLI flag `--dhcp-mode=auto-proxy` or the environment variable `TINKERBELL_DHCP_MODE=auto-proxy`.

### DHCP Disabled

This mode is used to disable all DHCP functionality in Tinkerbell. In this mode, the user is required to handle all DHCP functionality.

#### DHCP Disabled Configuration

To enable this mode set the CLI flag `--dhcp-enabled=false` or the environment variable `TINKERBELL_DHCP_ENABLED=false`.

## Receiving broadcast DHCP in a pod

A machine being network booted has no address yet, so its DHCPDISCOVER is a
broadcast: `ff:ff:ff:ff:ff:ff` at layer 2 and `255.255.255.255` at layer 3. A
pod on a CNI-managed network never sees it. Tinkerbell can bridge that gap
without giving the pod an interface on the physical network.

Enable it with `--dhcp-broadcast-redirect-enabled` (environment variable
`TINKERBELL_DHCP_BROADCAST_REDIRECT_ENABLED=true`), or in the Helm chart with
`deployment.envs.dhcp.broadcastRedirectEnabled=true`.

### How it works

Two TC eBPF programs are attached at the ingress hooks either side of the pod
boundary:

| Hook | Program | Action |
|---|---|---|
| Host's physical interface, ingress | `redirect_to_pod` | Broadcasts to UDP/67 are redirected, untouched, into the pod's veth |
| Host side of the pod's veth, ingress | `redirect_to_wire` | The DHCP replies the pod broadcasts back are rewritten to come from the host's address and MAC, then sent out of the physical interface |

Every other packet is returned with `TC_ACT_OK` and falls through to the CNI's
own programs, so the pod's normal datapath is untouched. Nothing is created,
renamed or deleted in either network namespace, which is the difference from the
macvlan and ipvlan approaches this replaces. The attachments are held open by
file descriptors, so they disappear when the process does — however it exits —
and leave nothing behind for the next start to clean up.

Requests are deliberately **not** readdressed to the pod on the way in. A client
that has no address yet sends from `0.0.0.0`, and the kernel accepts that source
only when the destination is the limited broadcast (see the "Accept zero
addresses only to limited broadcast" check in `ip_route_input_slow()`).
Readdressed to the pod's own IP, every DHCPDISCOVER is dropped as a martian
source, silently: no counter in `/proc/net/snmp` moves, and the drop is visible
only with `log_martians` enabled. Leaving the packet as a broadcast takes the
`brd_input` path, which is unconditional local delivery.

The consequence is that the DHCP server must be bound to `0.0.0.0`, which is the
default. Setting `--dhcp-bind-addr` to a specific address stops it receiving
broadcasts.

### Requirements

* Linux 5.10 or later on the node (for `bpf_redirect_peer`).
* Linux 6.6 or later to order the programs ahead of the CNI's own, using TCX. On
  older kernels they are attached as classic tc filters, which cannot be placed
  ahead of an existing filter; Smee logs a warning at startup when that happens.
* `hostPID: true`, so the host network namespace can be reached through
  `/proc/1/ns/net`.
* `CAP_BPF`, `CAP_NET_ADMIN`, `CAP_SYS_ADMIN` and `CAP_SYS_PTRACE`. The Helm
  chart adds all four when `broadcastRedirectEnabled` is set. `CAP_SYS_PTRACE`
  is needed only to open `/proc/1/ns/net`, which is another process's
  namespace: without ptrace access the kernel does not fail the open, it returns
  the `/proc` symlink instead of the namespace behind it, and the failure
  surfaces later as an unexplained `EINVAL` from `setns`. Smee detects that case
  and says so.

  To avoid `hostPID` and `CAP_SYS_PTRACE` entirely, pin the host namespace to a
  file on each node (`ip netns attach host 1`, which creates `/run/netns/host`),
  hostPath-mount `/run/netns` into the pod, and set
  `--dhcp-broadcast-redirect-host-netns` (Helm:
  `deployment.envs.dhcp.broadcastRedirectHostNetns`) to the mounted path. A
  bind-mounted namespace file needs no ptrace access to open.
* A pod whose primary interface is one end of a veth pair, which covers Cilium,
  Calico, Flannel and every other common CNI. A pod given an ipvlan or macvlan
  interface directly has no peer to redirect into, and Smee refuses to start
  rather than attaching programs that would silently drop everything.

### Running more than one replica

The redirect is per-pod and per-node: each pod attaches to its own veth and to
its own node's physical interface. Use leader election
(`--dhcp-enable-leader-election`, on by default with the kube backend) so only
one replica answers, exactly as without the redirect.

Do not run two Smee pods with the redirect enabled **on the same node**. Both
attach to that node's physical interface, and only whichever is at the head of
the hook sees the broadcasts, because a redirect consumes the packet rather than
passing it along — the other pod would never receive DHCP even if it held the
lease. Smee logs a warning when it finds another redirect already attached.
Spread replicas across nodes with `topologySpreadConstraints` or pod
anti-affinity. Briefly overlapping during a rolling update is harmless.

### Observability

Both programs keep counters, logged at startup and again at shutdown:

```
DHCP broadcast redirect counters  toPodMatched=12 toPodRedirected=12 toWireMatched=6 toWireRedirected=6 toWireError=0 unconfigured=0
```

`toPodMatched` moving while `toWireMatched` stays at zero means requests are
arriving but the DHCP server is not answering them. Both at zero means the
broadcasts are not reaching the interface the programs are attached to; check
that `--dhcp-broadcast-redirect-interface` names the interface facing the
provisioning network.

## Interoperability with other DHCP servers

When a DHCP server exists on the network, Tinkerbell should be set to run `proxy` or `auto-proxy` mode. This will allow Tinkerbell to provide the next boot information to clients that request it and the existing DHCP server will provide IP address information. Layer 2 access to machines or a DHCP relay agent that will forward the DHCP requests to Tinkerbell is required.
