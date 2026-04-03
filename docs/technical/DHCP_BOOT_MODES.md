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

## DHCP Proxy Interface Management

In `proxy` and `auto-proxy` modes, Tinkerbell automatically creates a macvlan interface so it can receive broadcast DHCP packets from the host network. When running multiple replicas, the interface should only be active on the pod that is currently serving the Kubernetes Service (i.e. the Service endpoint leader). Tinkerbell supports three interface management modes:

| Mode | When to use |
|---|---|
| **Lease-watch** | Multi-replica with Cilium L2 announcements (or any LB that uses a Lease for leader selection) |
| **Leader election** | Multi-replica without an external Lease to follow |
| **Static** | Single replica, no HA |

### Aligning with Cilium L2 Announcements

When using [Cilium L2 announcements](https://docs.cilium.io/en/stable/network/l2-announcements/) for the Tinkerbell Service, Cilium maintains a Lease object that tracks which node is the current L2 leader. Tinkerbell can watch this Lease and activate the DHCP proxy interface only on the matching node, ensuring the DHCP interface is always co-located with the Service endpoint.

Cilium L2 announcement Leases follow the naming convention `cilium-l2announce-<service-namespace>-<service-name>` and are created in the `kube-system` namespace. For example, a Service named `tinkerbell` in the `default` namespace produces the Lease `cilium-l2announce-default-tinkerbell`.

#### Configuration

Set the following CLI flags or environment variables:

```bash
--dhcp-interface-lease-watch-name=cilium-l2announce-default-tinkerbell
--dhcp-interface-lease-watch-namespace=kube-system
--dhcp-interface-node-name=<this-node-name>
```

Or equivalently:

```bash
TINKERBELL_SMEE_DHCP_INTERFACE_LEASE_WATCH_NAME=cilium-l2announce-default-tinkerbell
TINKERBELL_SMEE_DHCP_INTERFACE_LEASE_WATCH_NAMESPACE=kube-system
TINKERBELL_SMEE_DHCP_INTERFACE_NODE_NAME=<this-node-name>
```

The node name is typically injected via the Kubernetes downward API. In a Helm values file or pod spec:

```yaml
containers:
- name: tinkerbell
  env:
  - name: NODE_NAME
    valueFrom:
      fieldRef:
        fieldPath: spec.nodeName
  - name: TINKERBELL_SMEE_DHCP_INTERFACE_LEASE_WATCH_NAME
    value: "cilium-l2announce-default-tinkerbell"
  - name: TINKERBELL_SMEE_DHCP_INTERFACE_LEASE_WATCH_NAMESPACE
    value: "kube-system"
  - name: TINKERBELL_SMEE_DHCP_INTERFACE_NODE_NAME
    valueFrom:
      fieldRef:
        fieldPath: spec.nodeName
  securityContext:
    capabilities:
      add: ["NET_ADMIN"]
```

> **Note:** The pod must have `hostPID: true` and `CAP_NET_ADMIN` for macvlan interface creation. When `lease-watch-name` is set it takes precedence over `--dhcp-interface-leader-election-enabled`.

#### How it works

1. Tinkerbell performs a Kubernetes list+watch on the configured Lease object.
2. When the Lease's `holderIdentity` matches the configured node name, the macvlan interface is created and the DHCP server begins serving.
3. When the holder changes to a different node (or the Lease is deleted), the interface is torn down and the DHCP server stops.
4. If the watch connection drops, it reconnects automatically.

This ensures that broadcast DHCP packets are always received by the same pod that Cilium has selected to answer unicast traffic for the Tinkerbell Service.

## Interoperability with other DHCP servers

When a DHCP server exists on the network, Tinkerbell should be set to run `proxy` or `auto-proxy` mode. This will allow Tinkerbell to provide the next boot information to clients that request it and the existing DHCP server will provide IP address information. Layer 2 access to machines or a DHCP relay agent that will forward the DHCP requests to Tinkerbell is required.
