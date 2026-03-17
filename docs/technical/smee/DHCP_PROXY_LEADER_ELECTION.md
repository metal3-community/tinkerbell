# DHCP Proxy Mode with Leader Election

## Overview

When running SMEE in DHCP proxy mode with multiple replicas, only one instance should broadcast DHCP packets to avoid conflicts. This is achieved through leader election and dynamic network interface management.

## Problem

In Kubernetes deployments with multiple SMEE pods running in DHCP proxy mode:
- All pods receive broadcast DHCP packets
- Only the first pod to bind to the network interface can respond
- This creates a race condition where only one pod effectively serves DHCP
- If that pod fails, DHCP service is interrupted until another pod wins the race

## Solution

SMEE now supports:
1. **Leader Election**: Using Kubernetes leases, pods elect a leader
2. **Dynamic Interface Management**: Only the elected leader configures its network interface
3. **Automatic Failover**: When leadership changes, the new leader configures its interface and the old leader cleans up

## Architecture

### Network Interface Management

The network interface manager (`smee/internal/dhcpif`) handles:
- Creating macvlan/ipvlan interfaces in the host network namespace
- Moving the interface into the pod's network namespace
- Configuring the interface with the appropriate IP address
- Cleaning up interfaces when losing leadership

### Leader Election

Leader election uses Kubernetes Coordination API (Leases):
- Each pod competes for leadership
- Leader renews its lease periodically
- Non-leaders watch for leadership changes
- When the leader fails, a new leader is elected

## Configuration

### Environment Variables

```bash
# Enable network interface management
TINKERBELL_SMEE_DHCP_INTERFACE_ENABLED=true

# Interface type: macvlan or ipvlan (default: macvlan)
TINKERBELL_SMEE_DHCP_INTERFACE_TYPE=macvlan

# Source interface (default: auto-detect from default gateway)
TINKERBELL_SMEE_DHCP_INTERFACE_SRC_INTERFACE=eth0

# Enable leader election
TINKERBELL_SMEE_DHCP_INTERFACE_LEADER_ELECTION_ENABLED=true

# Leader election namespace (default: "default")
TINKERBELL_SMEE_DHCP_INTERFACE_LEADER_ELECTION_NAMESPACE=tink-system

# Leader election lock name (default: smee-dhcp-interface)
TINKERBELL_SMEE_DHCP_INTERFACE_LEADER_ELECTION_LOCK_NAME=smee-dhcp-interface

# Leader election identity (default: pod hostname)
TINKERBELL_SMEE_DHCP_INTERFACE_LEADER_ELECTION_IDENTITY=smee-xyz

# Lease duration (default: 15s)
TINKERBELL_SMEE_DHCP_INTERFACE_LEADER_ELECTION_LEASE_DURATION=15s

# Renew deadline (default: 10s)
TINKERBELL_SMEE_DHCP_INTERFACE_LEADER_ELECTION_RENEW_DEADLINE=10s

# Retry period (default: 2s)
TINKERBELL_SMEE_DHCP_INTERFACE_LEADER_ELECTION_RETRY_PERIOD=2s
```

### Command Line Flags

```bash
tinkerbell \
  --dhcp-interface-enabled=true \
  --dhcp-interface-type=macvlan \
  --dhcp-interface-src-interface=eth0 \
  --dhcp-interface-leader-election-enabled=true \
  --dhcp-interface-leader-election-namespace=tink-system
```

### Kubernetes Deployment

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: smee
  namespace: tink-system
spec:
  replicas: 3
  selector:
    matchLabels:
      app: smee
  template:
    metadata:
      labels:
        app: smee
    spec:
      serviceAccountName: smee
      securityContext:
        # Required for network namespace operations
        capabilities:
          add:
            - NET_ADMIN
            - SYS_ADMIN
      hostNetwork: true  # Required for accessing host network namespace
      containers:
      - name: smee
        image: ghcr.io/tinkerbell/tinkerbell:latest
        env:
        - name: TINKERBELL_SMEE_DHCP_ENABLED
          value: "true"
        - name: TINKERBELL_SMEE_DHCP_MODE
          value: "proxy"
        - name: TINKERBELL_SMEE_DHCP_INTERFACE_ENABLED
          value: "true"
        - name: TINKERBELL_SMEE_DHCP_INTERFACE_TYPE
          value: "macvlan"
        - name: TINKERBELL_SMEE_DHCP_INTERFACE_LEADER_ELECTION_ENABLED
          value: "true"
        - name: TINKERBELL_SMEE_DHCP_INTERFACE_LEADER_ELECTION_NAMESPACE
          value: "tink-system"
---
apiVersion: v1
kind: ServiceAccount
metadata:
  name: smee
  namespace: tink-system
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: smee-leader-election
  namespace: tink-system
rules:
- apiGroups: ["coordination.k8s.io"]
  resources: ["leases"]
  verbs: ["get", "create", "update"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: smee-leader-election
  namespace: tink-system
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: smee-leader-election
subjects:
- kind: ServiceAccount
  name: smee
  namespace: tink-system
```

## Interface Types

### macvlan (Recommended)

- **Mode**: Bridge
- **Use Case**: Most common DHCP proxy deployments
- **Advantages**: 
  - Well-tested and stable
  - Works with most network configurations
  - Separate MAC address per interface
- **Requirements**:
  - Parent interface must be in promiscuous mode (usually automatic)

### ipvlan

- **Mode**: L2
- **Use Case**: Environments where MAC address uniqueness is problematic
- **Advantages**:
  - Shares MAC address with parent interface
  - Lower resource usage
- **Disadvantages**:
  - Requires broadcast workaround (automatically handled)
  - May not work with all network configurations
- **Requirements**:
  - Kernel support for ipvlan

## Migration from Shell Script Approach

The previous approach used an init container with a shell script to configure the interface. This has been replaced with Go-based management integrated with leader election.

### Before (Shell Script in ConfigMap)

```yaml
initContainers:
- name: init-interface
  image: registry.k8s.io/pause:latest
  command: ["/bin/sh", "/scripts/host_interface.sh"]
  volumeMounts:
  - name: host-interface-script
    mountPath: /scripts
  securityContext:
    privileged: true
volumes:
- name: host-interface-script
  configMap:
    name: host-interface-script
```

### After (Built-in Leader Election)

```yaml
containers:
- name: smee
  env:
  - name: TINKERBELL_SMEE_DHCP_INTERFACE_ENABLED
    value: "true"
  - name: TINKERBELL_SMEE_DHCP_INTERFACE_LEADER_ELECTION_ENABLED
    value: "true"
  securityContext:
    capabilities:
      add:
        - NET_ADMIN
        - SYS_ADMIN
```

## Troubleshooting

### Check Leader Election Status

```bash
# View the current leader
kubectl get lease smee-dhcp-interface -n tink-system -o yaml

# Check logs for leader election events
kubectl logs -n tink-system -l app=smee --tail=100 | grep "leader"
```

### Common Issues

1. **Interface not created**
   - Check pod has NET_ADMIN and SYS_ADMIN capabilities
   - Verify hostNetwork: true is set
   - Check logs for interface creation errors

2. **Leader election not working**
   - Verify ServiceAccount has appropriate RBAC permissions
   - Check lease resource exists and is being updated
   - Verify namespace is correct

3. **Broadcast packets not received (ipvlan)**
   - Ensure workaround is being applied (check logs)
   - Try switching to macvlan mode

4. **Multiple pods responding to DHCP**
   - Check only one pod is leader
   - Verify leader election is enabled
   - Check for network configuration issues

### Debug Commands

```bash
# Check interface in pod
kubectl exec -n tink-system smee-xyz -- ip link show

# Check interface in host namespace (requires nsenter)
kubectl exec -n tink-system smee-xyz -- nsenter -t1 -n ip link show

# Watch leader election events
kubectl get events -n tink-system --watch | grep lease

# Check DHCP traffic
kubectl exec -n tink-system smee-xyz -- tcpdump -i macvlan0 -n port 67 or port 68
```

## Performance Considerations

- **Leader Election Overhead**: Minimal, uses efficient Kubernetes lease mechanism
- **Failover Time**: Typically 15-30 seconds (configurable via lease parameters)
- **Resource Usage**: Negligible additional CPU/memory for leader election
- **Network Overhead**: Only leader responds to DHCP packets

## Security Considerations

- Requires elevated privileges (NET_ADMIN, SYS_ADMIN) for network namespace operations
- Leader election uses Kubernetes RBAC for authorization
- Interface is isolated to pod's network namespace
- Cleanup ensures interfaces are removed when pod terminates

## References

- [Kubernetes Leader Election](https://kubernetes.io/docs/concepts/cluster-administration/networking/#network-plugins)
- [Linux Network Namespaces](https://man7.org/linux/man-pages/man7/network_namespaces.7.html)
- [macvlan Documentation](https://developers.redhat.com/blog/2018/10/22/introduction-to-linux-interfaces-for-virtual-networking#macvlan)
- [ipvlan Documentation](https://developers.redhat.com/blog/2018/10/22/introduction-to-linux-interfaces-for-virtual-networking#ipvlan)
