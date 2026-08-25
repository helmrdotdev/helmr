//go:build ignore

#define SEC(name) __attribute__((section(name), used))
#define __uint(name, value) int (*name)[value]
#define __type(name, value) typeof(value) *name
#define __packed __attribute__((packed))

typedef unsigned char __u8;
typedef unsigned short __u16;
typedef unsigned int __u32;

enum {
	BPF_MAP_TYPE_ARRAY = 2,
	TC_ACT_OK = 0,
	TC_ACT_SHOT = 2,
	ETH_P_IP = 0x0800,
	ETH_P_ARP = 0x0806,
	ARP_REQUEST = 1,
	ARP_REPLY = 2,
};

struct __sk_buff {
	__u32 len;
	__u32 pkt_type;
	__u32 mark;
	__u32 queue_mapping;
	__u32 protocol;
	__u32 vlan_present;
	__u32 vlan_tci;
	__u32 vlan_proto;
	__u32 priority;
	__u32 ingress_ifindex;
	__u32 ifindex;
	__u32 tc_index;
	__u32 cb[5];
	__u32 hash;
	__u32 tc_classid;
	__u32 data;
	__u32 data_end;
};

struct binding_state {
	__u32 active;
	__u32 mark;
	__u8 guest_ipv4[4];
	__u8 gateway_ipv4[4];
	__u8 guest_mac[6];
	__u8 reserved[2];
};

struct {
	__uint(type, BPF_MAP_TYPE_ARRAY);
	__uint(max_entries, 1);
	__type(key, __u32);
	__type(value, struct binding_state);
} state SEC(".maps");

static void *(*bpf_map_lookup_elem)(void *map, const void *key) =
	(void *)1;

struct eth_header {
	__u8 destination[6];
	__u8 source[6];
	__u16 protocol;
} __packed;

struct arp_header {
	__u16 hardware_type;
	__u16 protocol_type;
	__u8 hardware_length;
	__u8 protocol_length;
	__u16 operation;
	__u8 sender_mac[6];
	__u8 sender_ipv4[4];
	__u8 target_mac[6];
	__u8 target_ipv4[4];
} __packed;

struct ipv4_header {
	__u8 version_ihl;
	__u8 tos;
	__u16 total_length;
	__u16 identification;
	__u16 fragment_offset;
	__u8 ttl;
	__u8 protocol;
	__u16 checksum;
	__u8 source[4];
	__u8 destination[4];
} __packed;

static __inline __u16 host_u16(__u16 value)
{
	return __builtin_bswap16(value);
}

static __inline int bytes_equal(const __u8 *left, const __u8 *right, int count)
{
#pragma unroll
	for (int index = 0; index < 6; index++) {
		if (index < count && left[index] != right[index])
			return 0;
	}
	return 1;
}

static __inline int bytes_zero(const __u8 *value, int count)
{
#pragma unroll
	for (int index = 0; index < 6; index++) {
		if (index < count && value[index] != 0)
			return 0;
	}
	return 1;
}

static __inline int bytes_broadcast(const __u8 *value)
{
#pragma unroll
	for (int index = 0; index < 6; index++) {
		if (value[index] != 0xff)
			return 0;
	}
	return 1;
}

static __inline struct ipv4_header *valid_ipv4(void *payload, void *data_end)
{
	struct ipv4_header *ipv4 = payload;
	if ((void *)(ipv4 + 1) > data_end)
		return 0;
	__u32 version = ipv4->version_ihl >> 4;
	__u32 header_bytes = (ipv4->version_ihl & 0x0f) * 4;
	if (version != 4 || header_bytes < sizeof(*ipv4) || header_bytes > 60)
		return 0;
	__u32 available_bytes = (__u8 *)data_end - (__u8 *)ipv4;
	if (header_bytes > available_bytes)
		return 0;
	__u32 total_length = host_u16(ipv4->total_length);
	if (total_length < header_bytes || total_length > available_bytes)
		return 0;
	return ipv4;
}

static __inline int admit_tap(struct eth_header *eth, void *data_end,
	struct binding_state *binding)
{
	if (!bytes_equal(eth->source, binding->guest_mac, 6))
		return 0;
	if (host_u16(eth->protocol) == ETH_P_IP) {
		struct ipv4_header *ipv4 = valid_ipv4(eth + 1, data_end);
		return ipv4 && bytes_equal(ipv4->source, binding->guest_ipv4, 4);
	}
	if (host_u16(eth->protocol) != ETH_P_ARP)
		return 0;
	struct arp_header *arp = (void *)(eth + 1);
	if ((void *)(arp + 1) > data_end)
		return 0;
	return host_u16(arp->hardware_type) == 1 &&
		host_u16(arp->protocol_type) == ETH_P_IP &&
		arp->hardware_length == 6 && arp->protocol_length == 4 &&
		host_u16(arp->operation) == ARP_REQUEST &&
		bytes_broadcast(eth->destination) &&
		bytes_equal(arp->sender_mac, binding->guest_mac, 6) &&
		bytes_equal(arp->sender_ipv4, binding->guest_ipv4, 4) &&
		bytes_zero(arp->target_mac, 6) &&
		bytes_equal(arp->target_ipv4, binding->gateway_ipv4, 4);
}

SEC("classifier")
int helmr_ingress(struct __sk_buff *skb)
{
	void *data = (void *)(long)skb->data;
	void *data_end = (void *)(long)skb->data_end;
	struct eth_header *eth = data;
	__u32 state_key = 0;

	skb->mark = 0;
	struct binding_state *binding = bpf_map_lookup_elem(&state, &state_key);
	if (!binding || !binding->active || !binding->mark)
		return TC_ACT_SHOT;
	if ((void *)(eth + 1) > data_end)
		return TC_ACT_SHOT;
	if (!admit_tap(eth, data_end, binding))
		return TC_ACT_SHOT;
	skb->mark = binding->mark;
	return TC_ACT_OK;
}

char helmr_license[] SEC("license") = "Dual MIT/GPL";
