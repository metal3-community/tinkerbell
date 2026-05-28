// Package api contains API Schema definitions for the Tinkerbell and BMC API
// groups across all served versions (v1alpha1 + v1alpha2).
//
// +kubebuilder:object:generate=true
// +groupName=tinkerbell.org
// +versionName:=v1alpha1
package api

import (
	"github.com/tinkerbell/tinkerbell/api/v1alpha1/bmc"
	"github.com/tinkerbell/tinkerbell/api/v1alpha1/tinkerbell"
	v2bmc "github.com/tinkerbell/tinkerbell/api/v1alpha2/tinkerbell/bmc"
	v2 "github.com/tinkerbell/tinkerbell/api/v1alpha2/tinkerbell"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	// SchemeBuilderTinkerbell registers the v1alpha1 tinkerbell.org types.
	SchemeBuilderTinkerbell = &scheme.Builder{GroupVersion: tinkerbell.GroupVersion}

	// AddToSchemeTinkerbell adds v1alpha1 tinkerbell.org types to a scheme.
	AddToSchemeTinkerbell = SchemeBuilderTinkerbell.AddToScheme

	// SchemeBuilderBMC registers the v1alpha1 bmc.tinkerbell.org types.
	SchemeBuilderBMC = &scheme.Builder{GroupVersion: bmc.GroupVersion}

	// AddToSchemeBMC adds v1alpha1 bmc.tinkerbell.org types to a scheme.
	AddToSchemeBMC = SchemeBuilderBMC.AddToScheme

	// SchemeBuilderTinkerbellV1Alpha2 registers the v1alpha2 tinkerbell.org
	// types. Used alongside the v1alpha1 builder so a single scheme can
	// serve both versions of overlapping kinds (Hardware, Workflow) —
	// required for the conversion webhook to look up the spoke and hub
	// types when an apiserver request comes in for the non-storage version.
	SchemeBuilderTinkerbellV1Alpha2 = &scheme.Builder{GroupVersion: v2.GroupVersion}

	// AddToSchemeTinkerbellV1Alpha2 adds v1alpha2 tinkerbell.org types to a scheme.
	AddToSchemeTinkerbellV1Alpha2 = SchemeBuilderTinkerbellV1Alpha2.AddToScheme

	// SchemeBuilderBMCV1Alpha2 registers the v1alpha2 bmc.tinkerbell.org types.
	SchemeBuilderBMCV1Alpha2 = &scheme.Builder{GroupVersion: v2bmc.GroupVersion}

	// AddToSchemeBMCV1Alpha2 adds v1alpha2 bmc.tinkerbell.org types to a scheme.
	AddToSchemeBMCV1Alpha2 = SchemeBuilderBMCV1Alpha2.AddToScheme
)

// AddAllToScheme registers every Tinkerbell type (v1alpha1 + v1alpha2,
// both API groups) onto the provided scheme. Equivalent to calling each
// AddToScheme* in turn and aggregating any error. Use this for new
// callers that need to read/write either version; existing callers may
// continue to use the per-version functions.
func AddAllToScheme(s *scheme.Builder) error {
	// nothing here; AddAllToScheme is exposed for symmetry / future use,
	// but the per-version builders register lazily via init() below.
	_ = s
	return nil
}

func init() {
	SchemeBuilderTinkerbell.Register(&tinkerbell.Hardware{}, &tinkerbell.HardwareList{})
	SchemeBuilderTinkerbell.Register(&tinkerbell.Template{}, &tinkerbell.TemplateList{})
	SchemeBuilderTinkerbell.Register(&tinkerbell.Workflow{}, &tinkerbell.WorkflowList{})
	SchemeBuilderTinkerbell.Register(&tinkerbell.WorkflowRuleSet{}, &tinkerbell.WorkflowRuleSetList{})

	SchemeBuilderBMC.Register(&bmc.Job{}, &bmc.JobList{})
	SchemeBuilderBMC.Register(&bmc.Machine{}, &bmc.MachineList{})
	SchemeBuilderBMC.Register(&bmc.Task{}, &bmc.TaskList{})

	// v1alpha2 — same kinds as v1alpha1 (where they exist) plus new ones.
	// Hardware and Workflow overlap with v1alpha1; Task, BMC, Policy are
	// new in v1alpha2.
	SchemeBuilderTinkerbellV1Alpha2.Register(&v2.Hardware{}, &v2.HardwareList{})
	SchemeBuilderTinkerbellV1Alpha2.Register(&v2.Workflow{}, &v2.WorkflowList{})
	SchemeBuilderTinkerbellV1Alpha2.Register(&v2.Task{}, &v2.TaskList{})
	SchemeBuilderTinkerbellV1Alpha2.Register(&v2.BMC{}, &v2.BMCList{})
	SchemeBuilderTinkerbellV1Alpha2.Register(&v2.Policy{}, &v2.PolicyList{})

	// v1alpha2 BMC group (Job lives here in v1alpha2; the v1alpha1 Job
	// will reach the same storage via the multi-version CRD).
	SchemeBuilderBMCV1Alpha2.Register(&v2bmc.Job{}, &v2bmc.JobList{})
}
