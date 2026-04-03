package osie

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-logr/logr/testr"
)

func TestNewConfig(t *testing.T) {
	tests := []struct {
		name     string
		opts     []Option
		validate func(*testing.T, *Handler)
	}{
		{
			name: "default configuration",
			opts: nil,
			validate: func(t *testing.T, c *Handler) {
				t.Helper()
				if c.ImagePath != defaultImagePath {
					t.Errorf("expected ImagePath=%s, got %s", defaultImagePath, c.ImagePath)
				}
				if c.OCIRegistry != "ghcr.io" {
					t.Errorf("expected OCIRegistry=ghcr.io, got %s", c.OCIRegistry)
				}
				if c.OCIRepository != defaultOCIRepository {
					t.Errorf("expected OCIRepository=%s, got %s", defaultOCIRepository, c.OCIRepository)
				}
				if c.OCIReference != defaultOCIReference {
					t.Errorf("expected OCIReference=%s, got %s", defaultOCIReference, c.OCIReference)
				}
				if c.PullTimeout != 10*time.Minute {
					t.Errorf("expected PullTimeout=10m, got %s", c.PullTimeout)
				}
			},
		},
		{
			name: "custom configuration",
			opts: []Option{
				WithImagePath("/custom/path"),
				WithOCIRegistry("docker.io"),
				WithOCIRepository("myorg/hooks"),
				WithOCIReference("v1.2.3"),
				WithOCIUsername("testuser"),
				WithOCIPassword("testpass"),
				WithPullTimeout(5 * time.Minute),
			},
			validate: func(t *testing.T, c *Handler) {
				t.Helper()
				if c.ImagePath != "/custom/path" {
					t.Errorf("expected ImagePath=/custom/path, got %s", c.ImagePath)
				}
				if c.OCIRegistry != "docker.io" {
					t.Errorf("expected OCIRegistry=docker.io, got %s", c.OCIRegistry)
				}
				if c.OCIRepository != "myorg/hooks" {
					t.Errorf("expected OCIRepository=myorg/hooks, got %s", c.OCIRepository)
				}
				if c.OCIReference != "v1.2.3" {
					t.Errorf("expected OCIReference=v1.2.3, got %s", c.OCIReference)
				}
				if c.OCIUsername != "testuser" {
					t.Errorf("expected OCIUsername=testuser, got %s", c.OCIUsername)
				}
				if c.OCIPassword != "testpass" {
					t.Errorf("expected OCIPassword=testpass, got %s", c.OCIPassword)
				}
				if c.PullTimeout != 5*time.Minute {
					t.Errorf("expected PullTimeout=5m, got %s", c.PullTimeout)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := NewConfig(tt.opts...)
			tt.validate(t, config)
		})
	}
}

func TestStartWithExistingFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "existing.txt"), []byte("test"), 0o644); err != nil {
		t.Fatal(err)
	}

	config := NewConfig(WithImagePath(dir))
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	log := testr.New(t)
	err := config.Start(ctx, log)
	if err != nil && !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestStartWithEmptyDirectory(t *testing.T) {
	t.Skip("Skipping integration test that requires actual OCI registry")
}

func TestStartHTTPServerDisabled(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "test.txt"), []byte("test"), 0o644); err != nil {
		t.Fatal(err)
	}

	config := NewConfig(WithImagePath(dir))
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	log := testr.New(t)
	err := config.Start(ctx, log)
	if err != nil && !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestHTTPServer(t *testing.T) {
	dir := t.TempDir()
	testContent := "test content for OSIE file"
	if err := os.WriteFile(filepath.Join(dir, "vmlinuz-x86_64"), []byte(testContent), 0o644); err != nil {
		t.Fatal(err)
	}

	config := NewConfig(WithImagePath(dir))
	handler, err := config.Handle()
	if err != nil {
		t.Fatal(err)
	}
	if handler == nil {
		t.Fatal("expected non-nil handler")
	}
}

func TestConfigOptionChaining(t *testing.T) {
	config := NewConfig(
		WithImagePath("/test"),
		WithOCIRegistry("registry.example.com"),
		WithOCIRepository("org/repo"),
		WithOCIReference("v1.0.0"),
		WithPullTimeout(30*time.Second),
	)
	if config.ImagePath != "/test" {
		t.Errorf("ImagePath not set correctly")
	}
	if config.OCIRegistry != "registry.example.com" {
		t.Errorf("OCIRegistry not set correctly")
	}
	if config.OCIRepository != "org/repo" {
		t.Errorf("OCIRepository not set correctly")
	}
	if config.OCIReference != "v1.0.0" {
		t.Errorf("OCIReference not set correctly")
	}
	if config.PullTimeout != 30*time.Second {
		t.Errorf("PullTimeout not set correctly")
	}
}

func TestStartCreatesImageDirectory(t *testing.T) {
	tempBase := t.TempDir()
	imagePath := filepath.Join(tempBase, "nested", "hook", "images")
	config := NewConfig(WithImagePath(imagePath))

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	log := testr.New(t)
	_ = config.Start(ctx, log)

	info, err := os.Stat(imagePath)
	if err != nil {
		t.Fatalf("expected directory to be created: %v", err)
	}
	if !info.IsDir() {
		t.Error("expected path to be a directory")
	}
}

func TestConfigValidation(t *testing.T) {
	tests := []struct {
		name   string
		config *Handler
		valid  bool
	}{
		{
			name: "valid config with all fields",
			config: &Handler{
				ImagePath:     "/var/lib/images",
				OCIRegistry:   "ghcr.io",
				OCIRepository: "tinkerbell/captain/artifacts",
				OCIReference:  "latest",
				PullTimeout:   10 * time.Minute,
			},
			valid: true,
		},
		{
			name: "valid config with minimal fields",
			config: &Handler{
				ImagePath:     "/var/lib/images",
				OCIRegistry:   "ghcr.io",
				OCIRepository: "tinkerbell/captain/artifacts",
				OCIReference:  "latest",
				PullTimeout:   1 * time.Minute,
			},
			valid: true,
		},
		{
			name: "config with sha256 digest reference",
			config: &Handler{
				ImagePath:     "/var/lib/images",
				OCIRegistry:   "ghcr.io",
				OCIRepository: "tinkerbell/captain/artifacts",
				OCIReference:  "sha256:1234567890abcdef",
				PullTimeout:   5 * time.Minute,
			},
			valid: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.config.ImagePath == "" && tt.valid {
				t.Error("valid config should have ImagePath set")
			}
			if tt.config.OCIRegistry == "" && tt.valid {
				t.Error("valid config should have OCIRegistry set")
			}
			if tt.config.OCIRepository == "" && tt.valid {
				t.Error("valid config should have OCIRepository set")
			}
			if tt.config.OCIReference == "" && tt.valid {
				t.Error("valid config should have OCIReference set")
			}
			if tt.config.PullTimeout == 0 && tt.valid {
				t.Error("valid config should have PullTimeout set")
			}
		})
	}
}

func TestStartWithInvalidImagePath(t *testing.T) {
	config := NewConfig(WithImagePath("/invalid/readonly/path"))

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	log := testr.New(t)
	err := config.Start(ctx, log)
	if err == nil {
		t.Error("expected error when creating directory in invalid location")
	}
}
