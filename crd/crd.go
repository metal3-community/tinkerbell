package crd

import (
	"context"
	"embed"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/avast/retry-go/v4"
	"github.com/go-logr/logr"
	apiv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	v1 "k8s.io/apiextensions-apiserver/pkg/client/applyconfiguration/apiextensions/v1"
	"k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer/yaml"
	"k8s.io/client-go/rest"
)

//go:embed bases
var crdFS embed.FS

func mustReadCRD(path string) []byte {
	data, err := crdFS.ReadFile(path)
	if err != nil {
		panic(fmt.Sprintf("reading embedded CRD %s: %v", path, err))
	}
	return data
}

// Tinkerbell is the struct that holds the raw custom resource definitions
// and a CRD client for operations.
type Tinkerbell struct {
	CRDs       map[string][]byte
	Client     clientset.Interface
	Logger     logr.Logger
	restConfig *rest.Config
}

const (
	// GroupTinkerbell is the API group for core Tinkerbell resources.
	GroupTinkerbell = "tinkerbell.org"
	// GroupBMC is the API group for BMC resources.
	GroupBMC = "bmc.tinkerbell.org"
)

// AvailableVersions lists all supported CRD API versions.
var AvailableVersions = []string{"v1alpha1", "v1alpha2"}

// TinkerbellV1Alpha1 contains the v1alpha1 Tinkerbell CRDs.
var TinkerbellV1Alpha1 = map[string][]byte{
	"hardware.tinkerbell.org":         mustReadCRD("bases/v1alpha1/tinkerbell.org_hardware.yaml"),
	"templates.tinkerbell.org":        mustReadCRD("bases/v1alpha1/tinkerbell.org_templates.yaml"),
	"workflows.tinkerbell.org":        mustReadCRD("bases/v1alpha1/tinkerbell.org_workflows.yaml"),
	"workflowrulesets.tinkerbell.org": mustReadCRD("bases/v1alpha1/tinkerbell.org_workflowrulesets.yaml"),
	"jobs.bmc.tinkerbell.org":         mustReadCRD("bases/v1alpha1/bmc.tinkerbell.org_jobs.yaml"),
	"machines.bmc.tinkerbell.org":     mustReadCRD("bases/v1alpha1/bmc.tinkerbell.org_machines.yaml"),
	"tasks.bmc.tinkerbell.org":        mustReadCRD("bases/v1alpha1/bmc.tinkerbell.org_tasks.yaml"),
}

// TinkerbellV1Alpha2 contains the v1alpha2 Tinkerbell CRDs.
var TinkerbellV1Alpha2 = map[string][]byte{
	"hardware.tinkerbell.org":  mustReadCRD("bases/v1alpha2/tinkerbell.org_hardware.yaml"),
	"tasks.tinkerbell.org":     mustReadCRD("bases/v1alpha2/tinkerbell.org_tasks.yaml"),
	"bmcs.tinkerbell.org":      mustReadCRD("bases/v1alpha2/tinkerbell.org_bmcs.yaml"),
	"workflows.tinkerbell.org": mustReadCRD("bases/v1alpha2/tinkerbell.org_workflows.yaml"),
	"policies.tinkerbell.org":  mustReadCRD("bases/v1alpha2/tinkerbell.org_policies.yaml"),
	"jobs.bmc.tinkerbell.org":  mustReadCRD("bases/v1alpha2/bmc.tinkerbell.org_jobs.yaml"),
}

// TinkerbellDefaults is the CRD set the migrator installs on startup. It is
// computed at init time from TinkerbellV1Alpha1 + TinkerbellV1Alpha2: where
// the same CRD name appears in both maps (hardware, workflows, jobs.bmc),
// their spec.versions are merged so both versions stay served — v1alpha2
// becomes the storage version, v1alpha1 is kept served with storage:false.
// This makes v1alpha2 CRDs available to clients while the v1alpha1-typed
// runtime (scheme, indexers, controllers) keeps working unchanged.
//
// CRDs that exist in only one map (templates, workflowrulesets, machines.bmc,
// tasks.bmc on v1alpha1; bmcs, policies, tasks.tinkerbell.org on v1alpha2)
// pass through with their single served+storage version intact.
var TinkerbellDefaults = mergedCRDs()

// CRDsByVersion maps API version strings to their CRD source maps. Used by
// the UI to display version-specific resource listings.
var CRDsByVersion = map[string]map[string][]byte{
	"v1alpha1": TinkerbellV1Alpha1,
	"v1alpha2": TinkerbellV1Alpha2,
}

// mergedCRDs combines TinkerbellV1Alpha1 and TinkerbellV1Alpha2 into a single
// migrator-ready CRD set. CRDs present only in one map are passed through
// unchanged. CRDs present in both maps are merged: the v1alpha1 versions[]
// entries are appended to the v1alpha2 ones, with v1alpha1 marked
// storage:false and v1alpha2 marked storage:true. Output is JSON []byte
// (which is valid YAML) suitable for the existing decode-and-apply path in
// Migrate.
func mergedCRDs() map[string][]byte {
	out := make(map[string][]byte, len(TinkerbellV1Alpha1)+len(TinkerbellV1Alpha2))
	decoder := yaml.NewDecodingSerializer(unstructured.UnstructuredJSONScheme)

	parse := func(raw []byte) *unstructured.Unstructured {
		u := &unstructured.Unstructured{}
		if _, _, err := decoder.Decode(raw, nil, u); err != nil {
			panic(fmt.Sprintf("decoding embedded CRD: %v", err))
		}
		return u
	}
	setStorage := func(versions []any, name string, storage bool) {
		for _, v := range versions {
			vm, _ := v.(map[string]any)
			if vm["name"] == name {
				vm["storage"] = storage
			}
		}
	}

	// Start with v1alpha2 (its storage:true will remain).
	for name, raw := range TinkerbellV1Alpha2 {
		out[name] = raw
	}
	for name, raw := range TinkerbellV1Alpha1 {
		v2raw, hasV2 := out[name]
		if !hasV2 {
			// v1alpha1-only CRD; pass through.
			out[name] = raw
			continue
		}
		// Multi-version merge: append v1alpha1 versions[] to v1alpha2's, flip
		// storage flags.
		v1u := parse(raw)
		v2u := parse(v2raw)
		v1Versions, _, _ := unstructured.NestedSlice(v1u.Object, "spec", "versions")
		v2Versions, _, _ := unstructured.NestedSlice(v2u.Object, "spec", "versions")
		setStorage(v1Versions, "v1alpha1", false)
		setStorage(v2Versions, "v1alpha2", true)
		if err := unstructured.SetNestedSlice(v2u.Object, append(v2Versions, v1Versions...), "spec", "versions"); err != nil {
			panic(fmt.Sprintf("merging CRD %s: %v", name, err))
		}
		merged, err := v2u.MarshalJSON()
		if err != nil {
			panic(fmt.Sprintf("marshaling merged CRD %s: %v", name, err))
		}
		out[name] = merged
	}
	return out
}

// ConfigOption is a function that sets a configuration option.
type ConfigOption func(*Tinkerbell)

func WithRestConfig(config *rest.Config) ConfigOption {
	return func(t *Tinkerbell) {
		t.restConfig = config
	}
}

// WithLogger sets a structured logger for Kubernetes API server warnings.
func WithLogger(logger logr.Logger) ConfigOption {
	return func(t *Tinkerbell) {
		t.Logger = logger
	}
}

// WebhookClientConfig describes how the Kubernetes API server reaches the
// conversion webhook. Exactly one of URL or Service must be set. Mirrors
// apiextensions.k8s.io/v1.WebhookClientConfig but expressed as plain types
// so callers don't have to import the apiextensions package.
type WebhookClientConfig struct {
	// URL gives the location of the webhook as a https:// URL. Use this for
	// out-of-cluster setups (docker-compose dev) where there's no Service
	// object. Mutually exclusive with Service.
	URL string
	// Service references an in-cluster Service. Use this for helm-deployed
	// production where cert-manager + Service objects handle networking and
	// TLS. Mutually exclusive with URL.
	Service *WebhookServiceRef
	// CABundle is the PEM-encoded CA bundle the apiserver uses to validate
	// the webhook's TLS certificate. Required.
	CABundle []byte
}

// WebhookServiceRef points at an in-cluster Service that fronts the
// conversion webhook.
type WebhookServiceRef struct {
	Name      string
	Namespace string
	// Path on the webhook endpoint. Defaults to "/convert" when empty.
	Path string
	// Port the Service exposes. Defaults to 443 when nil.
	Port *int32
}

// conversionCapableKinds is the set of CRD kinds for which a working
// conversion webhook exists in this binary. Patching conversion=Webhook
// onto a CRD without a corresponding ConvertTo/ConvertFrom would break the
// apiserver's ability to read/write that resource, so we restrict
// patching to this allowlist. As conversion handlers are added for more
// kinds (Workflow, Job), append them here.
var conversionCapableKinds = map[string]struct{}{
	"hardware.tinkerbell.org": {},
}

// WithConversionWebhook patches the CRD set so that multi-version CRDs
// (those listed in conversionCapableKinds) use the Webhook conversion
// strategy with the provided client config. Must be applied AFTER
// WithCRDs or the default load; if no CRDs are configured yet, the
// default TinkerbellDefaults set is loaded first.
func WithConversionWebhook(cfg WebhookClientConfig) ConfigOption {
	return func(t *Tinkerbell) {
		if t.CRDs == nil {
			t.CRDs = TinkerbellDefaults
		}
		t.CRDs = patchConversionWebhook(t.CRDs, cfg)
	}
}

// patchConversionWebhook returns a copy of in where each multi-version
// CRD in conversionCapableKinds has its spec.conversion field set to
// Webhook with the provided client config. Single-version CRDs and
// kinds outside the allowlist pass through unmodified.
func patchConversionWebhook(in map[string][]byte, cfg WebhookClientConfig) map[string][]byte {
	decoder := yaml.NewDecodingSerializer(unstructured.UnstructuredJSONScheme)
	out := make(map[string][]byte, len(in))
	for name, raw := range in {
		if _, ok := conversionCapableKinds[name]; !ok {
			out[name] = raw
			continue
		}
		u := &unstructured.Unstructured{}
		if _, _, err := decoder.Decode(raw, nil, u); err != nil {
			panic(fmt.Sprintf("patchConversionWebhook: decoding %s: %v", name, err))
		}
		versions, _, _ := unstructured.NestedSlice(u.Object, "spec", "versions")
		if len(versions) <= 1 {
			// Single-version CRD doesn't need conversion.
			out[name] = raw
			continue
		}
		conversion := map[string]any{
			"strategy": "Webhook",
			"webhook": map[string]any{
				"conversionReviewVersions": []any{"v1"},
				"clientConfig":             clientConfigToMap(cfg),
			},
		}
		if err := unstructured.SetNestedMap(u.Object, conversion, "spec", "conversion"); err != nil {
			panic(fmt.Sprintf("patchConversionWebhook: set conversion on %s: %v", name, err))
		}
		patched, err := u.MarshalJSON()
		if err != nil {
			panic(fmt.Sprintf("patchConversionWebhook: marshal %s: %v", name, err))
		}
		out[name] = patched
	}
	return out
}

func clientConfigToMap(cfg WebhookClientConfig) map[string]any {
	out := map[string]any{}
	if len(cfg.CABundle) > 0 {
		// Apiextensions expects base64; unstructured marshaling handles
		// base64 encoding of []byte fields natively when we round-trip
		// via JSON, so we pass the bytes through as a base64 string.
		out["caBundle"] = base64.StdEncoding.EncodeToString(cfg.CABundle)
	}
	if cfg.URL != "" {
		out["url"] = cfg.URL
	}
	if cfg.Service != nil {
		svc := map[string]any{
			"name":      cfg.Service.Name,
			"namespace": cfg.Service.Namespace,
		}
		if cfg.Service.Path != "" {
			svc["path"] = cfg.Service.Path
		} else {
			svc["path"] = "/convert"
		}
		if cfg.Service.Port != nil {
			svc["port"] = int64(*cfg.Service.Port)
		}
		out["service"] = svc
	}
	return out
}

// logrWarningHandler adapts a logr.Logger to the rest.WarningHandler interface.
type logrWarningHandler struct {
	logger logr.Logger
}

func (h logrWarningHandler) HandleWarningHeader(code int, agent string, text string) {
	h.logger.Info("Kubernetes API warning", "code", code, "agent", agent, "text", text)
}

// NewTinkerbell returns a struct with a CRD client and the CRDs.
// If no CRDs are provided, it will use the default (TinkerbellDefaults) CRDs.
func NewTinkerbell(opts ...ConfigOption) (Tinkerbell, error) {
	tbell := Tinkerbell{
		CRDs: TinkerbellDefaults,
	}
	for _, opt := range opts {
		opt(&tbell)
	}

	if tbell.restConfig != nil {
		cfg := rest.CopyConfig(tbell.restConfig)
		if tbell.Logger.GetSink() != nil {
			cfg.WarningHandler = logrWarningHandler{logger: tbell.Logger}
		}
		client, err := clientset.NewForConfig(cfg)
		if err != nil {
			return Tinkerbell{}, fmt.Errorf("failed to create CRD client: %w", err)
		}
		tbell.Client = client
	}

	if tbell.Client == nil {
		return Tinkerbell{}, fmt.Errorf("no Kubernetes client configured: provide a rest.Config (e.g. WithRestConfig) or set Client directly via a ConfigOption")
	}

	return tbell, nil
}

func (t Tinkerbell) MigrateAndReady(ctx context.Context) error {
	if err := t.Migrate(ctx); err != nil {
		return err
	}

	return t.Ready(ctx)
}

// Migrate applies the CRDs to the cluster.
func (t Tinkerbell) Migrate(ctx context.Context) error {
	// TODO: should we check for differences in the CRDs? Should we check for the presence of the CRDs?
	// This function should eventually grow to handle upgrades.
	decoder := yaml.NewDecodingSerializer(unstructured.UnstructuredJSONScheme)
	for _, raw := range t.CRDs {
		obj := &unstructured.Unstructured{}
		if _, _, err := decoder.Decode(raw, nil, obj); err != nil {
			return fmt.Errorf("failed to decode YAML: %w", err)
		}

		// Try apply, if that fails, try create. Apply only works if the CRD already exists.
		if errApply := t.apply(ctx, obj); errApply != nil {
			if errUpdate := t.update(ctx, obj); errUpdate != nil {
				if errCreate := t.create(ctx, obj); errCreate != nil {
					return errors.Join(errApply, errUpdate, errCreate)
				}
			}
		}
	}

	return nil
}

// Ready checks if the CRDs exist in the cluster and are established.
func (t Tinkerbell) Ready(ctx context.Context) error {
	// Get the CRDs from the cluster to verify they are available and usable.
	for name := range t.CRDs {
		if err := retry.Do(func() error {
			crd, err := t.Client.ApiextensionsV1().CustomResourceDefinitions().Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				return fmt.Errorf("failed to get CRD %s: %w", name, err)
			}

			establishedCond := getCondition(crd, apiv1.Established)
			namesAcceptedCond := getCondition(crd, apiv1.NamesAccepted)
			if establishedCond == nil || establishedCond.Status != apiv1.ConditionTrue {
				return fmt.Errorf("CRD %s is not established yet", name)
			}
			if namesAcceptedCond == nil || namesAcceptedCond.Status != apiv1.ConditionTrue {
				return fmt.Errorf("CRD %s names are not accepted yet", name)
			}
			return nil
		}, retry.Attempts(5), retry.Delay(2*time.Second), retry.Context(ctx)); err != nil {
			return fmt.Errorf("failed waiting for CRD %s to be ready: %w", name, err)
		}
	}

	return nil
}

func (t Tinkerbell) create(ctx context.Context, obj *unstructured.Unstructured) error {
	var crdef apiv1.CustomResourceDefinition
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, &crdef); err != nil {
		return fmt.Errorf("failed to convert unstructured to CRD: %w", err)
	}
	if _, err := t.Client.ApiextensionsV1().CustomResourceDefinitions().Create(ctx, &crdef, metav1.CreateOptions{FieldManager: "Tinkerbell CLI"}); err != nil {
		return fmt.Errorf("failed to create CRD: %w", err)
	}

	return nil
}

func (t Tinkerbell) apply(ctx context.Context, obj *unstructured.Unstructured) error {
	crdef := &v1.CustomResourceDefinitionApplyConfiguration{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, crdef); err != nil {
		return fmt.Errorf("failed to convert unstructured to CRD: %w", err)
	}

	if _, err := t.Client.ApiextensionsV1().CustomResourceDefinitions().Apply(ctx, crdef, metav1.ApplyOptions{FieldManager: "Tinkerbell CLI"}); err != nil {
		return fmt.Errorf("failed to apply CRD: %w", err)
	}

	return nil
}

func (t Tinkerbell) update(ctx context.Context, obj *unstructured.Unstructured) error {
	var crdef apiv1.CustomResourceDefinition
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, &crdef); err != nil {
		return fmt.Errorf("failed to convert unstructured to CRD: %w", err)
	}
	// Get the existing CRD to update it.
	existingCRD, err := t.Client.ApiextensionsV1().CustomResourceDefinitions().Get(ctx, crdef.Name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get existing CRD %s: %w", crdef.Name, err)
	}
	// Update the existing CRD with the new spec.
	crdef.ResourceVersion = existingCRD.ResourceVersion
	crdef.UID = existingCRD.UID
	crdef.CreationTimestamp = existingCRD.CreationTimestamp
	if _, err := t.Client.ApiextensionsV1().CustomResourceDefinitions().Update(ctx, &crdef, metav1.UpdateOptions{FieldManager: "Tinkerbell CLI"}); err != nil {
		return fmt.Errorf("failed to update CRD: %w", err)
	}

	return nil
}

// getCondition returns a condition from a list of conditions if it exists.
func getCondition(crd *apiv1.CustomResourceDefinition, conditionType apiv1.CustomResourceDefinitionConditionType) *apiv1.CustomResourceDefinitionCondition {
	for _, cond := range crd.Status.Conditions {
		if cond.Type == conditionType {
			return &cond
		}
	}
	return nil
}
