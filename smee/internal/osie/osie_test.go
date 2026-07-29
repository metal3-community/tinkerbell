package osie

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-logr/logr/testr"
)

func TestNewConfigDefaults(t *testing.T) {
	c := NewConfig()
	if c.ImagePath != defaultImagePath {
		t.Errorf("expected ImagePath=%s, got %s", defaultImagePath, c.ImagePath)
	}
	if c.URLPrefix != DefaultURLPrefix {
		t.Errorf("expected URLPrefix=%s, got %s", DefaultURLPrefix, c.URLPrefix)
	}
}

func TestNewConfigOptions(t *testing.T) {
	c := NewConfig(
		WithImagePath("/custom/path"),
		WithURLPrefix("/assets/"),
	)
	if c.ImagePath != "/custom/path" {
		t.Errorf("expected ImagePath=/custom/path, got %s", c.ImagePath)
	}
	if c.URLPrefix != "/assets/" {
		t.Errorf("expected URLPrefix=/assets/, got %s", c.URLPrefix)
	}
}

func TestHandleServesFiles(t *testing.T) {
	dir := t.TempDir()
	content := "test kernel content"
	if err := os.WriteFile(filepath.Join(dir, "vmlinuz-x86_64"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	c := NewConfig(WithImagePath(dir), WithURLPrefix("/images/"))
	h, err := c.Handle()
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("GET", "/images/vmlinuz-x86_64", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != 200 {
		t.Errorf("expected 200, got %d", rr.Code)
	}
	if rr.Body.String() != content {
		t.Errorf("unexpected body: %q", rr.Body.String())
	}
}

func TestTFTPHandlerNotNil(t *testing.T) {
	log := testr.New(t)
	c := NewConfig()
	h := c.TFTPHandler(log)
	if h == nil {
		t.Error("expected non-nil TFTP handler")
	}
}
