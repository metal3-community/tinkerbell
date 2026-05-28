package crd

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/serializer/yaml"
)

// patchConversionWebhook is the core function: when called with a webhook
// config, multi-version CRDs in conversionCapableKinds should pick up
// spec.conversion={strategy:Webhook,...}; single-version and non-allowlisted
// kinds should be unchanged.

func TestPatchConversionWebhook_PatchesMultiVersionHardware(t *testing.T) {
	in := mergedCRDs()
	cfg := WebhookClientConfig{
		URL:      "https://tinkerbell:9443/convert",
		CABundle: []byte("dummy-ca"),
	}
	out := patchConversionWebhook(in, cfg)

	raw, ok := out["hardware.tinkerbell.org"]
	if !ok {
		t.Fatal("hardware.tinkerbell.org missing from output")
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatal(err)
	}
	spec := obj["spec"].(map[string]any)
	conv, ok := spec["conversion"].(map[string]any)
	if !ok {
		t.Fatalf("expected spec.conversion to be set on hardware CRD; spec=%v", spec)
	}
	if conv["strategy"] != "Webhook" {
		t.Errorf("strategy: got %v, want Webhook", conv["strategy"])
	}
	webhook, _ := conv["webhook"].(map[string]any)
	cc, _ := webhook["clientConfig"].(map[string]any)
	if cc["url"] != "https://tinkerbell:9443/convert" {
		t.Errorf("url: got %v", cc["url"])
	}
	if cc["caBundle"] != base64.StdEncoding.EncodeToString([]byte("dummy-ca")) {
		t.Errorf("caBundle: got %v", cc["caBundle"])
	}
	// conversionReviewVersions must be present and contain "v1"
	crv, _ := webhook["conversionReviewVersions"].([]any)
	if len(crv) != 1 || crv[0] != "v1" {
		t.Errorf("conversionReviewVersions: got %v, want [v1]", crv)
	}
}

func TestPatchConversionWebhook_LeavesSingleVersionAlone(t *testing.T) {
	in := mergedCRDs()
	cfg := WebhookClientConfig{
		URL:      "https://tinkerbell:9443/convert",
		CABundle: []byte("dummy-ca"),
	}
	out := patchConversionWebhook(in, cfg)

	// machines.bmc.tinkerbell.org is single-version (v1alpha1 only).
	// Even though it's not in conversionCapableKinds, double-check that
	// it's identical to the input.
	if string(out["machines.bmc.tinkerbell.org"]) != string(in["machines.bmc.tinkerbell.org"]) {
		t.Error("single-version CRD machines.bmc.tinkerbell.org was modified")
	}
}

func TestPatchConversionWebhook_LeavesNonHardwareAlone(t *testing.T) {
	in := mergedCRDs()
	cfg := WebhookClientConfig{
		URL:      "https://tinkerbell:9443/convert",
		CABundle: []byte("dummy-ca"),
	}
	out := patchConversionWebhook(in, cfg)

	// workflows.tinkerbell.org IS multi-version after merge, but it isn't
	// in conversionCapableKinds (no Workflow conversion implementation
	// yet), so patching must leave it untouched.
	if string(out["workflows.tinkerbell.org"]) != string(in["workflows.tinkerbell.org"]) {
		t.Error("workflows.tinkerbell.org was modified despite not being in conversionCapableKinds")
	}
}

func TestPatchConversionWebhook_ServiceRef(t *testing.T) {
	port := int32(9443)
	cfg := WebhookClientConfig{
		Service: &WebhookServiceRef{
			Name:      "tinkerbell-webhook",
			Namespace: "tinkerbell-system",
			Port:      &port,
		},
		CABundle: []byte("dummy-ca"),
	}
	out := patchConversionWebhook(mergedCRDs(), cfg)
	var obj map[string]any
	_ = json.Unmarshal(out["hardware.tinkerbell.org"], &obj)
	cc := obj["spec"].(map[string]any)["conversion"].(map[string]any)["webhook"].(map[string]any)["clientConfig"].(map[string]any)
	svc, ok := cc["service"].(map[string]any)
	if !ok {
		t.Fatalf("expected service ref; got clientConfig=%v", cc)
	}
	if svc["name"] != "tinkerbell-webhook" || svc["namespace"] != "tinkerbell-system" {
		t.Errorf("service name/ns mismatch: %v", svc)
	}
	if svc["path"] != "/convert" {
		t.Errorf("expected default path /convert; got %v", svc["path"])
	}
	// JSON-unmarshaled numbers come back as float64 unless explicitly typed
	if p, ok := svc["port"].(float64); !ok || int32(p) != 9443 {
		t.Errorf("port: got %v (type %T)", svc["port"], svc["port"])
	}
}

// TestMergedCRDs_MultiVersionPolicy asserts the per-kind versions-served
// policy enforced by mergedCRDs:
//
//   - Hardware (in conversionCapableKinds) is multi-version
//     with v1alpha2 as storage and v1alpha1 served-only.
//   - Workflow + Jobs.bmc — also present in both source maps but NOT in
//     conversionCapableKinds — are single-version v1alpha1. They MUST
//     stay that way until a conversion handler or data-migration
//     controller exists, otherwise strategy:None drops fields silently.
//   - v1alpha2-only kinds (bmcs/policies/tasks) are single-version v1alpha2.
//   - v1alpha1-only kinds (templates/workflowrulesets/machines.bmc/tasks.bmc)
//     are single-version v1alpha1.
func TestMergedCRDs_MultiVersionPolicy(t *testing.T) {
	out := mergedCRDs()
	want := map[string][]string{
		"hardware.tinkerbell.org":         {"v1alpha2", "v1alpha1"}, // multi-version (conversion-capable)
		"workflows.tinkerbell.org":        {"v1alpha1"},             // strategy:None unsafe with this schema diff
		"jobs.bmc.tinkerbell.org":         {"v1alpha1"},             // ditto
		"templates.tinkerbell.org":        {"v1alpha1"},             // v1alpha1-only kind
		"workflowrulesets.tinkerbell.org": {"v1alpha1"},             // v1alpha1-only kind
		"machines.bmc.tinkerbell.org":     {"v1alpha1"},             // v1alpha1-only kind
		"tasks.bmc.tinkerbell.org":        {"v1alpha1"},             // v1alpha1-only kind
		"bmcs.tinkerbell.org":             {"v1alpha2"},             // v1alpha2-only kind
		"policies.tinkerbell.org":         {"v1alpha2"},             // v1alpha2-only kind
		"tasks.tinkerbell.org":            {"v1alpha2"},             // v1alpha2-only kind
	}
	decoder := yaml.NewDecodingSerializer(unstructured.UnstructuredJSONScheme)
	for name, wantVersions := range want {
		raw, ok := out[name]
		if !ok {
			t.Errorf("%s: missing from mergedCRDs output", name)
			continue
		}
		u := &unstructured.Unstructured{}
		if _, _, err := decoder.Decode(raw, nil, u); err != nil {
			t.Errorf("%s: decode: %v", name, err)
			continue
		}
		versions, _, _ := unstructured.NestedSlice(u.Object, "spec", "versions")
		got := []string{}
		for _, v := range versions {
			got = append(got, v.(map[string]any)["name"].(string))
		}
		if len(got) != len(wantVersions) {
			t.Errorf("%s: got versions %v, want %v", name, got, wantVersions)
			continue
		}
		for i, want := range wantVersions {
			if got[i] != want {
				t.Errorf("%s: versions[%d] = %q, want %q", name, i, got[i], want)
			}
		}
	}
}

func TestWithConversionWebhook_AppliedToDefaultCRDs(t *testing.T) {
	// Exercise WithConversionWebhook applied via NewTinkerbell flow.
	t1 := Tinkerbell{CRDs: TinkerbellDefaults}
	opt := WithConversionWebhook(WebhookClientConfig{
		URL:      "https://tinkerbell:9443/convert",
		CABundle: []byte("ca"),
	})
	opt(&t1)

	var obj map[string]any
	if err := json.Unmarshal(t1.CRDs["hardware.tinkerbell.org"], &obj); err != nil {
		t.Fatal(err)
	}
	if _, ok := obj["spec"].(map[string]any)["conversion"]; !ok {
		t.Error("expected conversion to be set on hardware CRD after WithConversionWebhook")
	}
}
