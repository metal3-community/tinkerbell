package kube

import (
	"reflect"
	"sort"
	"testing"

	v2bmc "github.com/tinkerbell/tinkerbell/api/v1alpha2/tinkerbell/bmc"
	v2 "github.com/tinkerbell/tinkerbell/api/v1alpha2/tinkerbell"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestMACAddrsV1Alpha2(t *testing.T) {
	hw := &v2.Hardware{
		Spec: v2.HardwareSpec{
			NetworkInterfaces: v2.NetworkInterfaces{
				v2.MAC("aa:bb:cc:00:00:01"): {},
				v2.MAC("AA:BB:CC:00:00:02"): {}, // tests lowercasing
				v2.MAC("_synth_0"):          {}, // synthesized key from conversion should be skipped
			},
		},
	}
	got := MACAddrsV1Alpha2(hw)
	sort.Strings(got)
	want := []string{"aa:bb:cc:00:00:01", "aa:bb:cc:00:00:02"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestMACAddrsV1Alpha2_WrongType(t *testing.T) {
	if got := MACAddrsV1Alpha2(&v2.Workflow{}); got != nil {
		t.Errorf("expected nil for non-Hardware object; got %v", got)
	}
}

func TestIPAddrsV1Alpha2_IPv4AndIPv6(t *testing.T) {
	hw := &v2.Hardware{
		Spec: v2.HardwareSpec{
			NetworkInterfaces: v2.NetworkInterfaces{
				"aa:bb:cc:00:00:01": v2.NetworkInterface{
					IPAM: &v2.IPAM{
						IPv4: &v2.IP{Address: "10.0.0.5"},
						IPv6: &v2.IP{Address: "2001:db8::5"},
					},
				},
			},
		},
	}
	got := IPAddrsV1Alpha2(hw)
	sort.Strings(got)
	want := []string{"10.0.0.5", "2001:db8::5"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestHardwareNameV1Alpha2(t *testing.T) {
	hw := &v2.Hardware{ObjectMeta: metav1.ObjectMeta{Name: "hw-1"}}
	got := HardwareNameV1Alpha2(hw)
	if !reflect.DeepEqual(got, []string{"hw-1"}) {
		t.Errorf("got %v", got)
	}
}

func TestHardwareAgentIDV1Alpha2(t *testing.T) {
	tests := []struct {
		name string
		hw   *v2.Hardware
		want []string
	}{
		{"empty agentID", &v2.Hardware{}, nil},
		{"set", &v2.Hardware{Spec: v2.HardwareSpec{AgentID: "agent-1"}}, []string{"agent-1"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := HardwareAgentIDV1Alpha2(tc.hw)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestInstanceIDV1Alpha2(t *testing.T) {
	tests := []struct {
		name string
		hw   *v2.Hardware
		want []string
	}{
		{"no Instance", &v2.Hardware{}, nil},
		{"empty LookupID", &v2.Hardware{Spec: v2.HardwareSpec{Instance: &v2.Instance{}}}, nil},
		{"set", &v2.Hardware{Spec: v2.HardwareSpec{Instance: &v2.Instance{LookupID: "lookup-1"}}}, []string{"lookup-1"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := InstanceIDV1Alpha2(tc.hw)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestIndexesV1Alpha2_KeysAreDisjointFromV1(t *testing.T) {
	// Sanity-check that the suffixed IndexType values in IndexesV1Alpha2
	// don't collide with the unsuffixed v1alpha1 Indexes keys. Allows
	// callers to merge the two maps without losing entries.
	for k := range IndexesV1Alpha2 {
		if _, conflict := Indexes[k]; conflict {
			t.Errorf("key %q exists in both Indexes (v1) and IndexesV1Alpha2", k)
		}
	}
}

func TestIndexesV1Alpha2_JobExtractor(t *testing.T) {
	j := &v2bmc.Job{ObjectMeta: metav1.ObjectMeta{Name: "job-1"}}
	got := jobName(j)
	if !reflect.DeepEqual(got, []string{"job-1"}) {
		t.Errorf("got %v", got)
	}
}
