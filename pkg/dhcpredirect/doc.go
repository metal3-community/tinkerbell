// Package dhcpredirect carries DHCP between a physical network segment and a
// DHCP server running in a Kubernetes pod that has no presence on that segment.
//
// A machine being PXE booted has no address yet, so its DHCPDISCOVER can only
// be a broadcast: ff:ff:ff:ff:ff:ff at layer 2 and 255.255.255.255 at layer 3.
// A pod on a CNI-managed network never sees it. The usual answers to that are
// to give the pod a foot on the physical segment (host networking, a macvlan
// or ipvlan interface, Multus) — all of which mean managing host network
// interfaces from inside the pod and, with an eBPF based CNI such as Cilium,
// a second datapath running alongside the one the CNI installed.
//
// This package takes the opposite approach and leaves the interfaces alone.
// Two small TC eBPF programs are attached at the ingress hooks either side of
// the boundary and move only DHCP across it:
//
//   - On the host's physical interface, limited broadcasts to UDP/67 are
//     redirected, untouched, straight into the pod's veth. They stay
//     broadcasts: a DHCP client with no address yet sends from 0.0.0.0, and
//     the kernel accepts that source only for a limited broadcast, so
//     readdressing the packet to the pod would have it dropped as a martian
//     source.
//   - On the host side of that veth, the DHCP replies the pod broadcasts back
//     are rewritten to come from the host and redirected out of the physical
//     interface.
//
// Every other packet is returned with TC_ACT_OK and falls through to the CNI's
// own programs, so the pod's ordinary datapath is untouched. Nothing is
// created, renamed or deleted in either network namespace, and the attachments
// are held open by file descriptors, so they disappear on their own if the
// process dies.
//
// Requirements: Linux 5.10 or later (bpf_redirect_peer), CAP_BPF (or
// CAP_SYS_ADMIN) and CAP_NET_ADMIN to load and attach the programs, and
// CAP_SYS_ADMIN, CAP_SYS_PTRACE and a visible host PID namespace to reach the
// host network namespace through /proc/1/ns/net. CAP_SYS_PTRACE is needed only
// to open that path, because it is another process's namespace; a site that
// would rather not grant it can pin the host namespace to a file instead (`ip
// netns attach host 1`) and point [Config.HostNetNSPath] at the bind mount.
// Ordering ahead of the CNI's own TC programs needs Linux 6.6 or later, where
// the programs can be attached through TCX; see [Setup].
//
// One pod per node. Two pods redirecting on the same node both attach to its
// physical interface, and only whichever is at the head of the hook sees the
// broadcasts, since a redirect consumes the packet rather than passing it
// along. [Setup] warns when it finds another redirect already there.
package dhcpredirect
