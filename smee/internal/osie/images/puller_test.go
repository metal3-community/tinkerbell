package images

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-logr/logr/testr"
)

func TestNewPuller(t *testing.T) {
	tests := []struct {
		name    string
		opts    []Option
		setup   func(t *testing.T) string
		wantErr bool
		ready   bool
	}{
		{
			name: "directory with existing files marks ready immediately",
			setup: func(t *testing.T) string {
				t.Helper()
				dir := t.TempDir()
				if err := os.WriteFile(filepath.Join(dir, "vmlinuz"), []byte("kernel"), 0o644); err != nil {
					t.Fatal(err)
				}
				return dir
			},
			ready: true,
		},
		{
			name: "empty directory starts background puller",
			setup: func(t *testing.T) string {
				t.Helper()
				return t.TempDir()
			},
			ready: false,
		},
		{
			name: "creates destination directory if missing",
			setup: func(t *testing.T) string {
				t.Helper()
				return filepath.Join(t.TempDir(), "nested", "images")
			},
			ready: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := tt.setup(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			log := testr.New(t)
			p, err := NewPuller(ctx, log, WithDestDir(dir))
			if (err != nil) != tt.wantErr {
				t.Fatalf("NewPuller() error = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil {
				return
			}
			if got := p.Ready(); got != tt.ready {
				t.Errorf("Ready() = %v, want %v", got, tt.ready)
			}
		})
	}
}

func TestPullerHasFiles(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T) string
		want  bool
	}{
		{
			name: "empty directory",
			setup: func(t *testing.T) string {
				t.Helper()
				return t.TempDir()
			},
			want: false,
		},
		{
			name: "directory with files",
			setup: func(t *testing.T) string {
				t.Helper()
				dir := t.TempDir()
				if err := os.WriteFile(filepath.Join(dir, "test.txt"), []byte("test"), 0o644); err != nil {
					t.Fatal(err)
				}
				return dir
			},
			want: true,
		},
		{
			name: "directory with subdirectories only",
			setup: func(t *testing.T) string {
				t.Helper()
				dir := t.TempDir()
				if err := os.Mkdir(filepath.Join(dir, "subdir"), 0o755); err != nil {
					t.Fatal(err)
				}
				return dir
			},
			want: false,
		},
		{
			name: "non-existent directory",
			setup: func(t *testing.T) string {
				t.Helper()
				return filepath.Join(t.TempDir(), "nonexistent")
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := tt.setup(t)
			p := &Puller{destDir: dir}
			if got := p.hasFiles(); got != tt.want {
				t.Errorf("hasFiles() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestProcessImagePullRequestsSkipsWhenReady(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "existing"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	log := testr.New(t)
	p := &Puller{
		destDir: dir,
		log:     log,
		ready:   true,
	}
	// Should be a no-op when already ready.
	p.processImagePullRequests(context.Background())
	if !p.Ready() {
		t.Error("puller should still be ready after processImagePullRequests")
	}
}

func TestProcessImagePullRequestsRetriesOnFailure(t *testing.T) {
	dir := t.TempDir()
	log := testr.New(t)
	p := &Puller{
		destDir:     dir,
		registry:    "invalid.example.com",
		repository:  "test/hook",
		reference:   "nonexistent",
		pullTimeout: 100 * time.Millisecond,
		log:         log,
	}
	// First call should fail but not panic.
	p.processImagePullRequests(context.Background())
	if p.Ready() {
		t.Error("puller should not be ready after failed pull")
	}
	// Second call should also fail (retry behavior).
	p.processImagePullRequests(context.Background())
	if p.Ready() {
		t.Error("puller should not be ready after second failed pull")
	}
}

func TestPullerOptions(t *testing.T) {
	p := &Puller{}
	opts := []Option{
		WithRegistry("docker.io"),
		WithRepository("myorg/myrepo"),
		WithReference("v2.0.0"),
		WithUsername("user"),
		WithPassword("pass"),
		WithDestDir("/tmp/test"),
		WithPullTimeout(5 * time.Minute),
	}
	for _, o := range opts {
		o(p)
	}
	if p.registry != "docker.io" {
		t.Errorf("registry = %q, want docker.io", p.registry)
	}
	if p.repository != "myorg/myrepo" {
		t.Errorf("repository = %q, want myorg/myrepo", p.repository)
	}
	if p.reference != "v2.0.0" {
		t.Errorf("reference = %q, want v2.0.0", p.reference)
	}
	if p.username != "user" {
		t.Errorf("username = %q, want user", p.username)
	}
	if p.password != "pass" {
		t.Errorf("password = %q, want pass", p.password)
	}
	if p.destDir != "/tmp/test" {
		t.Errorf("destDir = %q, want /tmp/test", p.destDir)
	}
	if p.pullTimeout != 5*time.Minute {
		t.Errorf("pullTimeout = %v, want 5m", p.pullTimeout)
	}
}

func TestNewPullerCreatesDirectory(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "nested", "images")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	log := testr.New(t)
	_, err := NewPuller(ctx, log, WithDestDir(dir))
	if err != nil {
		t.Fatalf("NewPuller() error = %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("directory not created: %v", err)
	}
	if !info.IsDir() {
		t.Error("expected path to be a directory")
	}
}
