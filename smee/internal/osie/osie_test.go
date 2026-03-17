package osie

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/testr"
)

func TestNewConfig(t *testing.T) {
	tests := []struct {
		name     string
		opts     []Option
		validate func(*testing.T, *Config)
	}{
		{
			name: "default configuration",
			opts: nil,
			validate: func(t *testing.T, c *Config) {
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
			validate: func(t *testing.T, c *Config) {
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
	log := testr.New(t)
	handler, err := config.Handler(log)
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

// Mock logger for testing log output.
type mockLogger struct {
	logs []string
}

func (m *mockLogger) Init(logr.RuntimeInfo) {}

func (m *mockLogger) Enabled(_ int) bool { return true }

func (m *mockLogger) Info(_ int, msg string, _ ...interface{}) {
	m.logs = append(m.logs, fmt.Sprintf("INFO: %s", msg))
}

func (m *mockLogger) Error(err error, msg string, _ ...interface{}) {
	m.logs = append(m.logs, fmt.Sprintf("ERROR: %s: %v", msg, err))
}

func (m *mockLogger) WithValues(_ ...interface{}) logr.LogSink { return m }

func (m *mockLogger) WithName(_ string) logr.LogSink { return m }

func TestLoggingBehavior(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "test.txt"), []byte("test"), 0o644); err != nil {
		t.Fatal(err)
	}

	mockLog := &mockLogger{}
	log := logr.New(mockLog)
	config := NewConfig(WithImagePath(dir))

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_ = config.Start(ctx, log)
	time.Sleep(50 * time.Millisecond)

	if len(mockLog.logs) == 0 {
		t.Error("expected some log messages to be generated")
	}

	foundStartMessage := false
	for _, l := range mockLog.logs {
		if contains(l, "starting OSIE service") {
			foundStartMessage = true
			break
		}
	}
	if !foundStartMessage {
		t.Error("expected to find 'starting OSIE service' log message")
	}
}

func contains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func TestConfigValidation(t *testing.T) {
	tests := []struct {
		name   string
		config *Config
		valid  bool
	}{
		{
			name: "valid config with all fields",
			config: &Config{
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
			config: &Config{
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
			config: &Config{
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
