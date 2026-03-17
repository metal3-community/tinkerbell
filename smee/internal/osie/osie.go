package osie

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"github.com/tinkerbell/tinkerbell/pkg/tftp/handler"
	"github.com/tinkerbell/tinkerbell/smee/internal/osie/images"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

const (
	// DefaultURLPrefix is the default URI path prefix for all OSIE file requests.
	DefaultURLPrefix     = "/images/"
	defaultOCIReference  = "v0.0.0-9ea7a56"
	defaultOCIRepository = "tinkerbell/captain/artifacts"
	defaultImagePath     = "/var/lib/images"
)

// serialOrMacPattern matches path prefixes with a serial number or MAC address: <serial-or-mac>/
// Firmware may prepend the serial number (8-12 hex chars) or MAC address (dash-separated)
// to all TFTP file requests during netboot.
var (
	serialOrMacPattern = regexp.MustCompile(`^(([0-9a-fA-F]{2}[-]?){4,6})/{1,2}(.+)$`)
)

// Config holds the configuration for the OSIE service.
type Config struct {
	DebugMode bool
	URLPrefix string
	// ImagePath is the directory where OSIE images are stored.
	ImagePath string
	// OCIRegistry is the OCI registry URL (e.g., "ghcr.io").
	OCIRegistry string
	// OCIRepository is the repository path (e.g., "tinkerbell/captain/artifacts").
	OCIRepository string
	// OCIReference is the image tag or digest (e.g., "latest", "v1.2.3", "sha256:...").
	OCIReference string
	// OCIUsername is the optional username for OCI registry authentication.
	OCIUsername string
	// OCIPassword is the optional password for OCI registry authentication.
	OCIPassword string
	// PullTimeout for pulling OCI images.
	PullTimeout time.Duration
}

// Option functions for configuring the OSIE service.
type Option func(*Config)

func WithURLPrefix(prefix string) Option {
	return func(c *Config) {
		c.URLPrefix = prefix
	}
}

func WithImagePath(path string) Option {
	return func(c *Config) {
		c.ImagePath = path
	}
}

func WithOCIRegistry(registry string) Option {
	return func(c *Config) {
		c.OCIRegistry = registry
	}
}

func WithOCIRepository(repository string) Option {
	return func(c *Config) {
		c.OCIRepository = repository
	}
}

func WithOCIReference(reference string) Option {
	return func(c *Config) {
		c.OCIReference = reference
	}
}

func WithPullTimeout(timeout time.Duration) Option {
	return func(c *Config) {
		c.PullTimeout = timeout
	}
}

func WithOCIUsername(username string) Option {
	return func(c *Config) {
		c.OCIUsername = username
	}
}

func WithOCIPassword(password string) Option {
	return func(c *Config) {
		c.OCIPassword = password
	}
}

// NewConfig creates a new OSIE service configuration with defaults.
func NewConfig(opts ...Option) *Config {
	defaults := &Config{
		DebugMode:     false,
		URLPrefix:     DefaultURLPrefix,
		ImagePath:     defaultImagePath,
		OCIRegistry:   "ghcr.io",
		OCIRepository: defaultOCIRepository,
		OCIReference:  defaultOCIReference,
		PullTimeout:   10 * time.Minute,
	}
	for _, opt := range opts {
		opt(defaults)
	}
	return defaults
}

// Start initializes and starts the OSIE image puller service.
// It creates an images.Puller that periodically checks whether the image directory
// has files and, if not, pulls the configured OCI image in the background.
func (c *Config) Start(ctx context.Context, log logr.Logger) error {
	log.Info("starting OSIE service")

	_, err := images.NewPuller(ctx, log,
		images.WithRegistry(c.OCIRegistry),
		images.WithRepository(c.OCIRepository),
		images.WithReference(c.OCIReference),
		images.WithUsername(c.OCIUsername),
		images.WithPassword(c.OCIPassword),
		images.WithDestDir(c.ImagePath),
		images.WithPullTimeout(c.PullTimeout),
	)
	if err != nil {
		return fmt.Errorf("creating image puller: %w", err)
	}

	<-ctx.Done()
	return nil
}

// Handler returns an http.Handler that serves OSIE files from the filesystem.
// It strips the URLPrefix from incoming request paths so the FileServer resolves
// files relative to ImagePath (e.g. /images/vmlinuz-x86_64 -> <ImagePath>/vmlinuz-x86_64).
func (c *Config) Handler(log logr.Logger) (http.Handler, error) {
	return http.StripPrefix(c.URLPrefix, http.FileServerFS(os.DirFS(c.ImagePath))), nil
}

// TFTPHandler returns a TFTP handler that serves OSIE files from the filesystem.
func (c *Config) TFTPHandler(log logr.Logger) handler.Handler {
	return &tftpHandler{
		log:      log,
		cacheDir: c.ImagePath,
	}
}

// tftpHandler serves OSIE files over TFTP.
type tftpHandler struct {
	log      logr.Logger
	cacheDir string
}

func (h *tftpHandler) ServeTFTP(filename string, rf io.ReaderFrom) error {
	log := h.log.WithValues("event", "osie_file", "filename", filename)
	log.Info("handling OSIE file request")

	if h.cacheDir == "" {
		return fmt.Errorf("cache directory not configured")
	}

	tracer := otel.Tracer("TFTP-OSIE")
	_, span := tracer.Start(context.Background(), "TFTP OSIE file serve",
		trace.WithSpanKind(trace.SpanKindServer))
	defer span.End()

	// Strip serial/MAC prefix from the filename.
	// e.g. "dc-a6-32-01-02-03/vmlinuz-aarch64" or "abcdef01/initramfs-aarch64".
	cleanName := filename
	if matches := serialOrMacPattern.FindStringSubmatch(filename); matches != nil {
		cleanName = matches[len(matches)-1]
		log.Info("stripped serial/MAC prefix from filename", "original", filename, "cleaned", cleanName)
	}

	filePath := filepath.Join(h.cacheDir, cleanName)
	// Security check - ensure the file is within the configured directory
	if !strings.HasPrefix(filepath.Clean(filePath), filepath.Clean(h.cacheDir)) {
		err := fmt.Errorf("invalid file path: %s", filename)
		span.SetStatus(codes.Error, err.Error())
		return err
	}

	f, err := os.Open(filePath) // #nosec G304 -- path validated above
	if err != nil {
		if os.IsNotExist(err) {
			err := fmt.Errorf("OSIE file not found: %s", filename)
			span.SetStatus(codes.Error, err.Error())
			return err
		}
		log.Error(err, "failed to open OSIE file from filesystem")
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("failed to open OSIE file from filesystem: %w", err)
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		log.Error(err, "failed to stat OSIE file")
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("failed to stat OSIE file: %w", err)
	}

	if transfer, ok := rf.(interface{ SetSize(int64) }); ok {
		transfer.SetSize(fi.Size())
	}

	n, err := rf.ReadFrom(f)
	if err != nil {
		log.Error(err, "failed to serve OSIE file content")
		span.SetStatus(codes.Error, err.Error())
		return fmt.Errorf("failed to serve OSIE file content: %w", err)
	}

	log.Info("successfully served OSIE file", "filename", filename, "bytes_transferred", n)
	span.SetStatus(codes.Ok, "file served successfully")
	return nil
}
