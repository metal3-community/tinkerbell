package conversionwebhook

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-logr/logr"
	v1alpha1 "github.com/tinkerbell/tinkerbell/api/v1alpha1/tinkerbell"
	v1alpha2 "github.com/tinkerbell/tinkerbell/api/v1alpha2/tinkerbell"
	apix "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/conversion"
)

func nopLogger() logr.Logger { return logr.Discard() }

// TestConvertHandler_V1ToV2 wires the controller-runtime conversion handler
// to an httptest.Server, posts a v1alpha1 Hardware in a ConversionReview,
// and verifies the response carries the converted v1alpha2 object.
func TestConvertHandler_V1ToV2(t *testing.T) {
	scheme := NewScheme()
	handler := conversion.NewWebhookHandler(scheme)
	srv := httptest.NewServer(handler)
	defer srv.Close()

	src := &v1alpha1.Hardware{
		TypeMeta: metav1.TypeMeta{
			APIVersion: v1alpha1.GroupVersion.String(),
			Kind:       "Hardware",
		},
		ObjectMeta: metav1.ObjectMeta{Name: "hw", Namespace: "ns"},
		Spec: v1alpha1.HardwareSpec{
			AgentID:     "agent-1",
			TinkVersion: 7,
			Disks:       []v1alpha1.Disk{{Device: "/dev/sda"}},
		},
	}
	srcRaw, err := json.Marshal(src)
	if err != nil {
		t.Fatal(err)
	}
	review := &apix.ConversionReview{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "apiextensions.k8s.io/v1",
			Kind:       "ConversionReview",
		},
		Request: &apix.ConversionRequest{
			UID:               "test-uid",
			DesiredAPIVersion: v1alpha2.GroupVersion.String(),
			Objects:           []runtime.RawExtension{{Raw: srcRaw}},
		},
	}
	body, err := json.Marshal(review)
	if err != nil {
		t.Fatal(err)
	}

	resp, err := http.Post(srv.URL, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: %d", resp.StatusCode)
	}

	respReview := &apix.ConversionReview{}
	if err := json.NewDecoder(resp.Body).Decode(respReview); err != nil {
		t.Fatal(err)
	}
	if respReview.Response == nil {
		t.Fatal("response is nil")
	}
	if respReview.Response.Result.Status != metav1.StatusSuccess {
		t.Fatalf("conversion failed: %+v", respReview.Response.Result)
	}
	if len(respReview.Response.ConvertedObjects) != 1 {
		t.Fatalf("expected 1 converted object, got %d", len(respReview.Response.ConvertedObjects))
	}

	got := &v1alpha2.Hardware{}
	if err := json.Unmarshal(respReview.Response.ConvertedObjects[0].Raw, got); err != nil {
		t.Fatal(err)
	}
	if got.Spec.AgentID != "agent-1" {
		t.Errorf("AgentID: got %q, want agent-1", got.Spec.AgentID)
	}
	if len(got.Spec.StorageDevices) != 1 || got.Spec.StorageDevices[0].Name != "/dev/sda" {
		t.Errorf("StorageDevices mismatch: %+v", got.Spec.StorageDevices)
	}
	if _, ok := got.Annotations[v1alpha1.ConversionAnnotation]; !ok {
		t.Errorf("expected conversion annotation to carry preserved tinkVersion; got annotations=%v", got.Annotations)
	}
}

func TestNew_ValidatesConfig(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
	}{
		{name: "missing bind", cfg: Config{CertFile: "c", KeyFile: "k"}},
		{name: "missing cert", cfg: Config{BindAddr: ":0", KeyFile: "k"}},
		{name: "missing key", cfg: Config{BindAddr: ":0", CertFile: "c"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(tc.cfg, nopLogger()); err == nil {
				t.Errorf("expected New(%+v) to error", tc.cfg)
			}
		})
	}
}
