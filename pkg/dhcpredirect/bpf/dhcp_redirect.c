/* SPDX-License-Identifier: Apache-2.0 */

/*
 * dhcp_redirect.c moves DHCP between a physical network segment and a pod that
 * has no presence on that segment.
 *
 * A PXE client that has not been given an address yet can only shout: its
 * DHCPDISCOVER is addressed to ff:ff:ff:ff:ff:ff at layer 2 and to
 * 255.255.255.255 at layer 3. A CNI-managed pod is deliberately isolated from
 * that, so the broadcast is dropped by the host before anything in the pod can
 * see it. Two TC programs carry those packets across the boundary by hand:
 *
 *   redirect_to_pod   ingress of the host's physical interface.
 *                     Limited broadcasts to UDP/67 are pushed, untouched,
 *                     straight into the pod's veth.
 *
 *   redirect_to_wire  ingress of the host side of that same veth, which is
 *                     where packets leaving the pod appear.
 *                     The DHCP replies the pod broadcasts back are readdressed
 *                     to look like they came from the host and are pushed out
 *                     of the physical interface.
 *
 * Everything else is returned with TC_ACT_OK and falls through to whatever the
 * CNI has attached behind us, so the pod's ordinary datapath is untouched.
 *
 * Both programs are driven entirely by the config map, which the Go side fills
 * in before attaching. Until it is filled in, the programs pass every packet.
 */

#include <linux/bpf.h>
#include <linux/if_ether.h>
#include <linux/in.h>
#include <linux/ip.h>
#include <linux/pkt_cls.h>
#include <linux/udp.h>

#include <bpf/bpf_endian.h>
#include <bpf/bpf_helpers.h>

#include <stddef.h>

#define DHCP_SERVER_PORT 67
#define DHCP_CLIENT_PORT 68

/* 255.255.255.255. All bits set, so no byte order conversion is needed. */
#define IPV4_LIMITED_BROADCAST 0xffffffff

/* The smallest frame either program has to look at in full: Ethernet, an
 * option-less IPv4 header, and a UDP header. */
#define MIN_HDR_LEN (sizeof(struct ethhdr) + sizeof(struct iphdr) + sizeof(struct udphdr))

/* Offsets of the fields the outbound program rewrites, relative to the start
 * of the frame. saddr sits at a fixed offset inside the IPv4 header, so IP
 * options do not move it; the UDP header does move, hence the argument. */
#define OFF_IP_CHECK (sizeof(struct ethhdr) + offsetof(struct iphdr, check))
#define OFF_IP_SADDR (sizeof(struct ethhdr) + offsetof(struct iphdr, saddr))
#define OFF_UDP_CHECK(l4_off) ((l4_off) + offsetof(struct udphdr, check))
#define OFF_ETH_SOURCE (offsetof(struct ethhdr, h_source))

/* config flags. */
enum {
	/* Deliver into the pod with bpf_redirect_peer() rather than
	 * bpf_redirect(). Set by the Go side once it has confirmed the pod's
	 * link really is a veth. */
	CFG_F_REDIRECT_PEER = 1U << 0,
};

/* Filled in by the Go side before the programs are attached. A pod_ifindex or
 * phys_ifindex of zero means "not configured yet" and disables the program
 * that needs it. */
struct dhcp_config {
	/* Address of the pod's interface, in network byte order. Used to
	 * recognise the pod's own DHCP replies on the way out. */
	__be32 pod_ip;
	/* Primary address of the physical interface, in network byte order.
	 * Outbound replies are rewritten to come from it. Zero leaves the
	 * source address alone. */
	__be32 host_ip;
	/* Host side veth of the pod: the target of the inbound redirect and
	 * the interface the outbound program is attached to. */
	__u32 pod_ifindex;
	/* Physical interface: the interface the inbound program is attached to
	 * and the target of the outbound redirect. */
	__u32 phys_ifindex;
	/* MAC of the physical interface. Outbound replies are rewritten to
	 * come from it so the pod's MAC never appears on the segment. An
	 * all-zero MAC leaves the source alone. */
	__u8 phys_mac[6];
	__u16 flags;
};

struct {
	__uint(type, BPF_MAP_TYPE_ARRAY);
	__type(key, __u32);
	__type(value, struct dhcp_config);
	__uint(max_entries, 1);
} config_map SEC(".maps");

/* Counter slots in stats_map. Kept in sync with the Go side, which names them. */
enum {
	STAT_TO_POD_MATCHED = 0,
	STAT_TO_POD_REDIRECTED = 1,
	STAT_TO_WIRE_MATCHED = 2,
	STAT_TO_WIRE_REDIRECTED = 3,
	STAT_TO_WIRE_ERROR = 4,
	STAT_UNCONFIGURED = 5,
	STAT__MAX = 6,
};

struct {
	__uint(type, BPF_MAP_TYPE_PERCPU_ARRAY);
	__type(key, __u32);
	__type(value, __u64);
	__uint(max_entries, STAT__MAX);
} stats_map SEC(".maps");

static __always_inline void count(__u32 key)
{
	__u64 *slot = bpf_map_lookup_elem(&stats_map, &key);
	if (slot)
		*slot += 1;
}

static __always_inline struct dhcp_config *config(void)
{
	__u32 key = 0;
	return bpf_map_lookup_elem(&config_map, &key);
}

/* Parsed offsets into a UDP over IPv4 frame. */
struct udp_frame {
	__u32 l4_off; /* start of the UDP header */
	__be32 saddr;
	__be32 daddr;
	__be16 sport;
	__be16 dport;
	__be16 udp_check;
};

/* Make the first need bytes of the frame directly addressable, pulling in
 * non-linear data only when the frame does not already have that much in its
 * linear section. Returns 0 on success. */
static __always_inline int pull(struct __sk_buff *skb, __u32 need)
{
	if ((void *)(long)skb->data + need <= (void *)(long)skb->data_end)
		return 0;
	if (bpf_skb_pull_data(skb, need) < 0)
		return -1;
	if ((void *)(long)skb->data + need > (void *)(long)skb->data_end)
		return -1;
	return 0;
}

/* Fill out from an Ethernet frame carrying UDP over IPv4, or return -1 for
 * anything else. This is the hot path: every packet on the interfaces the
 * programs are attached to goes through it, so it does no work beyond what is
 * needed to rule a frame out. */
static __always_inline int parse_udp(struct __sk_buff *skb, struct udp_frame *out)
{
	if (pull(skb, MIN_HDR_LEN) < 0)
		return -1;

	void *data = (void *)(long)skb->data;
	void *data_end = (void *)(long)skb->data_end;

	struct ethhdr *eth = data;
	if ((void *)(eth + 1) > data_end)
		return -1;
	if (eth->h_proto != bpf_htons(ETH_P_IP))
		return -1;

	struct iphdr *ip = (struct iphdr *)(eth + 1);
	if ((void *)(ip + 1) > data_end)
		return -1;
	if (ip->protocol != IPPROTO_UDP)
		return -1;
	/* A fragment carries no ports to match on, and DHCP is never
	 * fragmented. Leave it to the stack. */
	if (ip->frag_off & bpf_htons(0x1fff))
		return -1;

	__u32 ihl = (__u32)ip->ihl * 4;
	if (ihl < sizeof(struct iphdr) || ihl > 60)
		return -1;

	__u32 l4_off = (__u32)sizeof(struct ethhdr) + ihl;

	/* An option-carrying header pushes the UDP header past what the first
	 * pull covered, so widen it and re-derive every pointer: pulling can
	 * move the buffer. */
	if (ihl != sizeof(struct iphdr)) {
		if (pull(skb, l4_off + sizeof(struct udphdr)) < 0)
			return -1;
		data = (void *)(long)skb->data;
		data_end = (void *)(long)skb->data_end;
		ip = data + sizeof(struct ethhdr);
		if ((void *)(ip + 1) > data_end)
			return -1;
	}

	if (data + l4_off + sizeof(struct udphdr) > data_end)
		return -1;
	struct udphdr *udp = data + l4_off;

	out->l4_off = l4_off;
	out->saddr = ip->saddr;
	out->daddr = ip->daddr;
	out->sport = udp->source;
	out->dport = udp->dest;
	out->udp_check = udp->check;
	return 0;
}

/* Replace a 4 byte IPv4 address in the header and repair both checksums it
 * feeds. The UDP checksum is optional over IPv4: a sender that did not compute
 * one leaves the field zero, and zero means "absent" rather than "wrong", so
 * it must be left exactly as it is. Returns 0 on success. */
static __always_inline int rewrite_addr(struct __sk_buff *skb, __u32 addr_off,
					__u32 l4_off, __be32 from, __be32 to,
					__be16 udp_check)
{
	if (bpf_skb_store_bytes(skb, addr_off, &to, sizeof(to), 0) < 0)
		return -1;
	if (bpf_l3_csum_replace(skb, OFF_IP_CHECK, from, to, sizeof(to)) < 0)
		return -1;
	if (udp_check != 0 &&
	    bpf_l4_csum_replace(skb, OFF_UDP_CHECK(l4_off), from, to,
				BPF_F_PSEUDO_HDR | sizeof(to)) < 0)
		return -1;
	return 0;
}

/*
 * Ingress of the host's physical interface.
 *
 * A DHCP client with no address broadcasts to ff:ff:ff:ff:ff:ff and
 * 255.255.255.255:67. Hand that frame to the pod's veth exactly as it arrived.
 *
 * It is tempting to readdress it to the pod's own IP on the way through, on
 * the grounds that a pod ought to receive traffic addressed to it. That does
 * not work, and fails on precisely the packets this exists to carry. A client
 * that has not been given an address yet sends from 0.0.0.0, and the kernel
 * accepts a source of 0.0.0.0 only when the destination is the limited
 * broadcast -- see the "Accept zero addresses only to limited broadcast"
 * check in ip_route_input_slow(). Readdressed to a unicast destination, every
 * DHCPDISCOVER is dropped as a martian source, silently: no counter in
 * /proc/net/snmp moves, and the drop is visible only with log_martians on.
 *
 * Left as a broadcast the frame takes the brd_input path instead, which is
 * unconditional local delivery and skips source validation altogether. So the
 * frame goes over untouched, which also means there are no checksums to
 * repair here. The destination MAC stays broadcast for the same reason: the
 * pod's stack takes broadcast frames happily.
 *
 * The one thing this asks of the DHCP server is that its socket be bound to
 * 0.0.0.0 rather than to a specific address, which is the default.
 */
SEC("tc")
int redirect_to_pod(struct __sk_buff *skb)
{
	struct udp_frame frame;

	if (parse_udp(skb, &frame) < 0)
		return TC_ACT_OK;
	if (frame.dport != bpf_htons(DHCP_SERVER_PORT))
		return TC_ACT_OK;
	if (frame.daddr != IPV4_LIMITED_BROADCAST)
		return TC_ACT_OK;

	count(STAT_TO_POD_MATCHED);

	struct dhcp_config *cfg = config();
	if (!cfg || cfg->pod_ifindex == 0) {
		count(STAT_UNCONFIGURED);
		return TC_ACT_OK;
	}

	count(STAT_TO_POD_REDIRECTED);

	/* bpf_redirect_peer() hands the frame straight to the interface on the
	 * far side of the veth, skipping the host side's egress hooks. */
	if (cfg->flags & CFG_F_REDIRECT_PEER)
		return bpf_redirect_peer(cfg->pod_ifindex, 0);
	return bpf_redirect(cfg->pod_ifindex, 0);
}

/*
 * Ingress of the host side of the pod's veth, which is where everything the
 * pod sends turns up.
 *
 * The pod answers a DHCP request by broadcasting from its own address, which
 * is meaningless on the physical segment and would be dropped on the way out.
 * Rewrite it to come from the host, at both layer 2 and layer 3, and send it
 * out of the physical interface.
 */
SEC("tc")
int redirect_to_wire(struct __sk_buff *skb)
{
	struct udp_frame frame;

	if (parse_udp(skb, &frame) < 0)
		return TC_ACT_OK;
	if (frame.sport != bpf_htons(DHCP_SERVER_PORT))
		return TC_ACT_OK;
	if (frame.dport != bpf_htons(DHCP_CLIENT_PORT) &&
	    frame.dport != bpf_htons(DHCP_SERVER_PORT))
		return TC_ACT_OK;
	if (frame.daddr != IPV4_LIMITED_BROADCAST)
		return TC_ACT_OK;

	struct dhcp_config *cfg = config();
	if (!cfg || cfg->phys_ifindex == 0) {
		count(STAT_UNCONFIGURED);
		return TC_ACT_OK;
	}
	/* Only the pod's own DHCP replies are carried out. Anything else
	 * broadcasting from this veth is not ours to move. */
	if (cfg->pod_ip == 0 || frame.saddr != cfg->pod_ip)
		return TC_ACT_OK;

	count(STAT_TO_WIRE_MATCHED);

	if (cfg->host_ip != 0 &&
	    rewrite_addr(skb, OFF_IP_SADDR, frame.l4_off, frame.saddr,
			 cfg->host_ip, frame.udp_check) < 0) {
		count(STAT_TO_WIRE_ERROR);
		return TC_ACT_SHOT;
	}

	/* Leaving the pod's MAC as the source would put a MAC that belongs to
	 * no port on the segment into every switch's forwarding table. */
	__u8 zero_mac[6] = {};
	if (__builtin_memcmp(cfg->phys_mac, zero_mac, sizeof(zero_mac)) != 0 &&
	    bpf_skb_store_bytes(skb, OFF_ETH_SOURCE, cfg->phys_mac,
				sizeof(cfg->phys_mac), 0) < 0) {
		count(STAT_TO_WIRE_ERROR);
		return TC_ACT_SHOT;
	}

	count(STAT_TO_WIRE_REDIRECTED);

	return bpf_redirect(cfg->phys_ifindex, 0);
}

char _license[] SEC("license") = "GPL";
