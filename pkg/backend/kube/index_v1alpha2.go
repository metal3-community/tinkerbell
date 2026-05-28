package kube

import (
	"strings"

	v2bmc "github.com/tinkerbell/tinkerbell/api/v1alpha2/tinkerbell/bmc"
	v2 "github.com/tinkerbell/tinkerbell/api/v1alpha2/tinkerbell"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Parallel v1alpha2 indexers. Same logical fields as the v1alpha1
// indexers in index.go (MACAddr, IPAddr, HardwareName, etc.), but
// keyed against v1alpha2 typed objects and using v1alpha2 field paths
// (NetworkInterfaces map, Attributes struct).
//
// These are registered alongside the v1alpha1 indexers in IndexesV1Alpha2
// below. To opt a consumer into v1alpha2 queries, call
// `client.List(ctx, &v2.HardwareList{}, client.MatchingFields{kube.MACAddrIndex: mac})` —
// the cache routes to this set based on the typed object passed.
//
// The exported field-name constants (MACAddrIndex, IPAddrIndex, etc.)
// are shared with the v1alpha1 indexers because controller-runtime
// field indexes are scoped per-GVK; the same human-readable name can
// safely refer to two indexes when the typed objects differ.

// IndexesV1Alpha2 is the v1alpha2 counterpart of Indexes. The keys use
// distinct IndexType values (suffixed "V1Alpha2") so callers that want
// both versions registered can combine the maps without map-key
// collisions.
var IndexesV1Alpha2 = map[IndexType]Index{
	IndexTypeMACAddr + "V1Alpha2": {
		Obj:          &v2.Hardware{},
		Field:        MACAddrIndex,
		ExtractValue: MACAddrsV1Alpha2,
	},
	IndexTypeIPAddr + "V1Alpha2": {
		Obj:          &v2.Hardware{},
		Field:        IPAddrIndex,
		ExtractValue: IPAddrsV1Alpha2,
	},
	IndexTypeHardwareName + "V1Alpha2": {
		Obj:          &v2.Hardware{},
		Field:        NameIndex,
		ExtractValue: HardwareNameV1Alpha2,
	},
	IndexTypeWorkflowAgentID + "V1Alpha2": {
		Obj:          &v2.Workflow{},
		Field:        WorkflowAgentIDIndex,
		ExtractValue: WorkflowAgentIDV1Alpha2,
	},
	IndexTypeHardwareAgentID + "V1Alpha2": {
		Obj:          &v2.Hardware{},
		Field:        HardwareAgentIDIndex,
		ExtractValue: HardwareAgentIDV1Alpha2,
	},
	IndexTypeInstanceID + "V1Alpha2": {
		Obj:          &v2.Hardware{},
		Field:        InstanceIDIndex,
		ExtractValue: InstanceIDV1Alpha2,
	},
	IndexTypeMachineName + "V1Alpha2": {
		Obj:          &v2bmc.Job{}, // v1alpha2 has no Machine; closest analog is Job
		Field:        NameIndex,
		ExtractValue: jobName,
	},
}

// MACAddrsV1Alpha2 returns lowercased MAC addresses for a v1alpha2 Hardware,
// pulled from the NetworkInterfaces map's keys (v1alpha2's MAC field IS
// the map key). Matches the lowercase normalization in the v1alpha1
// → v1alpha2 conversion code so cross-version lookups produce identical
// index entries for objects round-tripped via the conversion webhook.
func MACAddrsV1Alpha2(obj client.Object) []string {
	hw, ok := obj.(*v2.Hardware)
	if !ok {
		return nil
	}
	macs := make([]string, 0, len(hw.Spec.NetworkInterfaces))
	for mac := range hw.Spec.NetworkInterfaces {
		// Skip synthesized keys from conversion (they aren't real MACs).
		s := string(mac)
		if strings.HasPrefix(s, "_synth_") {
			continue
		}
		macs = append(macs, strings.ToLower(s))
	}
	return macs
}

// IPAddrsV1Alpha2 returns IPv4/IPv6 addresses across all network
// interfaces of a v1alpha2 Hardware.
func IPAddrsV1Alpha2(obj client.Object) []string {
	hw, ok := obj.(*v2.Hardware)
	if !ok {
		return nil
	}
	var ips []string
	for _, ni := range hw.Spec.NetworkInterfaces {
		if ni.IPAM == nil {
			continue
		}
		if ni.IPAM.IPv4 != nil && ni.IPAM.IPv4.Address != "" {
			ips = append(ips, ni.IPAM.IPv4.Address)
		}
		if ni.IPAM.IPv6 != nil && ni.IPAM.IPv6.Address != "" {
			ips = append(ips, ni.IPAM.IPv6.Address)
		}
	}
	return ips
}

// HardwareNameV1Alpha2 indexes a v1alpha2 Hardware by .metadata.name.
func HardwareNameV1Alpha2(obj client.Object) []string {
	hw, ok := obj.(*v2.Hardware)
	if !ok {
		return nil
	}
	return []string{hw.Name}
}

// WorkflowAgentIDV1Alpha2 indexes a v1alpha2 Workflow by its task-level
// agent ID. v1alpha2 doesn't have a top-level status.agentID — it tracks
// the agent per-task in status.metadata.task.agentID. We return all
// agent IDs across rendered tasks; the index can't represent "the"
// agent because v1alpha2 supports multiple tasks per workflow.
func WorkflowAgentIDV1Alpha2(obj client.Object) []string {
	wf, ok := obj.(*v2.Workflow)
	if !ok {
		return nil
	}
	if wf.Status.Metadata.Task.AgentID == "" {
		return nil
	}
	return []string{wf.Status.Metadata.Task.AgentID}
}

// HardwareAgentIDV1Alpha2 indexes a v1alpha2 Hardware by spec.agentID
// (the field name is unchanged from v1alpha1).
func HardwareAgentIDV1Alpha2(obj client.Object) []string {
	hw, ok := obj.(*v2.Hardware)
	if !ok || hw.Spec.AgentID == "" {
		return nil
	}
	return []string{hw.Spec.AgentID}
}

// InstanceIDV1Alpha2 indexes a v1alpha2 Hardware by spec.instance.lookupID
// (the closest v1alpha2 analog to v1alpha1's spec.metadata.instance.id —
// see the schema-mapping audit for details).
func InstanceIDV1Alpha2(obj client.Object) []string {
	hw, ok := obj.(*v2.Hardware)
	if !ok {
		return nil
	}
	if hw.Spec.Instance == nil || hw.Spec.Instance.LookupID == "" {
		return nil
	}
	return []string{hw.Spec.Instance.LookupID}
}

func jobName(obj client.Object) []string {
	j, ok := obj.(*v2bmc.Job)
	if !ok {
		return nil
	}
	return []string{j.Name}
}
