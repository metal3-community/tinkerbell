# DHCP broadcast redirect, end to end

Proves that [`pkg/dhcpredirect`](../../../pkg/dhcpredirect) does what it claims:
a machine that can only broadcast completes a DHCP handshake against a Smee pod
that has no interface on the machine's network, on a node whose datapath belongs
to Cilium.

```console
$ script/e2e/dhcp-broadcast-redirect/run.sh
```

Takes a few minutes, mostly Cilium starting up. Needs `docker`, `kind`,
`kubectl`, `helm` and `go`; nothing has to be installed on the host and no root
is needed. Pass `--keep` to leave the cluster running afterwards.

## What it sets up

```
  docker network tink-dhcp-redirect-e2e (172.30.0.0/16)
  ┌──────────────────────────────────────────────────────────────────┐
  │                                                                  │
  │  probe container            kind node (172.30.0.2)               │
  │  ┌──────────────────┐       ┌──────────────────────────────────┐ │
  │  │ 52:54:00:dc:be:ef│───────│ eth0                             │ │
  │  │ no IP address    │  L2   │  ▲ redirect_to_pod (TCX ingress) │ │
  │  └──────────────────┘       │  │                               │ │
  │                             │  ▼ lxc… ── veth ── smee pod      │ │
  │                             │    redirect_to_wire   10.244.…   │ │
  │                             └──────────────────────────────────┘ │
  └──────────────────────────────────────────────────────────────────┘
```

* A kind cluster with `disableDefaultCNI` and `kubeProxyMode: none`, then Cilium
  with `kubeProxyReplacement=true`. Cilium owns the node's datapath and has its
  own TC eBPF programs on the same hooks, which is the interesting part: the
  redirect has to sit ahead of them without disturbing them.
* Smee in an ordinary pod — **not** `hostNetwork` — with
  `--dhcp-broadcast-redirect-enabled` and a single reservation for the probe's
  MAC, served from the file backend.
* A probe container on the same docker network as the node, with a fixed MAC and
  no address, which broadcasts a DHCPDISCOVER from a raw packet socket exactly
  as a PXE client does.

The cluster gets a docker network of its own rather than the shared `kind` one.
Every kind cluster on a host shares that network by default, which would put
their nodes on the same broadcast segment as the probe; any of them running a
DHCP server can answer first, and the test would then be measuring something
else entirely.

## What passing looks like

```
==> what the redirect resolved
"DHCP broadcast redirect active" physicalInterface=eth0 physicalAddr=172.30.0.2
  physicalMAC=ee:9d:70:53:3e:8b podInterface=eth0 podAddr=10.244.0.108
  peerInterface=lxc88472dd1de67 attach=tcx redirectPeer=true

==> Smee pod IP 10.244.0.108 (node 172.30.0.2) — no interface on the 52:54:00:dc:be:ef segment

==> broadcasting a DHCPDISCOVER from a container on the node's segment
OFFER  serverID=172.30.0.2 yiaddr=172.30.99.10 siaddr=172.30.0.2 bootfile=""
ACK    yiaddr=172.30.99.10 netmask=ffff0000 router=[172.30.0.1] dns=[1.1.1.1]
OK: a full DHCP handshake completed over the broadcast segment
```

`peerInterface=lxc…` is Cilium's own veth for the pod, and `attach=tcx` means
the programs went on ahead of Cilium's. The handshake completing at all is the
result: the pod's only interface is on 10.244.0.0/16, so every one of those four
packets crossed the boundary through the eBPF programs.

## The faster test

Most of the datapath is covered without any of this by the namespace test in
`pkg/dhcpredirect`, which builds the same shape out of three network namespaces
and asserts on the packets themselves. It needs root, so run it in a container:

```console
$ CGO_ENABLED=0 go test -c -o /tmp/dhcpredirect.test ./pkg/dhcpredirect/
$ docker run --privileged --rm -v /tmp:/t:ro alpine /t/dhcpredirect.test -test.v
```

That one runs in under a second. Use this end to end test when the question is
specifically about coexisting with a real CNI.
