// Package conversionwebhook serves CRD conversion requests from the
// Kubernetes API server. It hosts a TLS HTTPS endpoint at /convert that
// routes ConversionReview requests to the controller-runtime conversion
// machinery, which in turn calls the ConvertTo/ConvertFrom methods on
// registered Convertible types.
//
// Currently registered: Hardware (v1alpha1 ↔ v1alpha2). See
// api/v1alpha1/tinkerbell/conversion_hardware.go for the conversion logic.
package conversionwebhook

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/go-logr/logr"
	v1alpha1 "github.com/tinkerbell/tinkerbell/api/v1alpha1/tinkerbell"
	v1alpha2 "github.com/tinkerbell/tinkerbell/api/v1alpha2/tinkerbell"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/conversion"
)

// Config configures the conversion webhook server.
type Config struct {
	// BindAddr is the host:port to listen on. Required.
	BindAddr string
	// CertFile is the path to the TLS certificate (PEM). Required.
	CertFile string
	// KeyFile is the path to the TLS private key (PEM). Required.
	KeyFile string
	// ReadHeaderTimeout protects against Slowloris-style attacks. Defaults
	// to 10s when zero.
	ReadHeaderTimeout time.Duration
	// ShutdownTimeout caps the time http.Server.Shutdown is allowed to
	// run. Defaults to 5s when zero.
	ShutdownTimeout time.Duration
}

// Server is a TLS HTTPS server that serves CRD conversion requests.
type Server struct {
	cfg Config
	log logr.Logger

	mu     sync.Mutex
	server *http.Server
}

// New constructs a Server. It does not start listening; call Start to do so.
func New(cfg Config, log logr.Logger) (*Server, error) {
	if cfg.BindAddr == "" {
		return nil, errors.New("conversionwebhook: BindAddr is required")
	}
	if cfg.CertFile == "" || cfg.KeyFile == "" {
		return nil, errors.New("conversionwebhook: CertFile and KeyFile are required (CRD conversion webhooks require TLS)")
	}
	if cfg.ReadHeaderTimeout == 0 {
		cfg.ReadHeaderTimeout = 10 * time.Second
	}
	if cfg.ShutdownTimeout == 0 {
		cfg.ShutdownTimeout = 5 * time.Second
	}
	return &Server{cfg: cfg, log: log}, nil
}

// NewScheme builds the runtime.Scheme registered with all Tinkerbell API
// versions that participate in conversion. Exposed so callers (tests, the
// webhook server, the data-migration controllers) all share the same
// registered types.
func NewScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	utilruntime.Must(addToScheme(s))
	return s
}

func addToScheme(s *runtime.Scheme) error {
	// v1alpha1 Hardware
	s.AddKnownTypes(
		v1alpha1.GroupVersion,
		&v1alpha1.Hardware{},
		&v1alpha1.HardwareList{},
	)
	metav1.AddToGroupVersion(s, v1alpha1.GroupVersion)

	// v1alpha2 Hardware (the hub)
	s.AddKnownTypes(
		v1alpha2.GroupVersion,
		&v1alpha2.Hardware{},
		&v1alpha2.HardwareList{},
	)
	metav1.AddToGroupVersion(s, v1alpha2.GroupVersion)
	return nil
}

// Start begins serving until the context is canceled. It blocks until the
// server exits cleanly or returns an error.
//
// Errors returned by http.Server.ListenAndServeTLS *after* a clean
// context cancellation (i.e. http.ErrServerClosed) are suppressed; any
// other error is returned.
func (s *Server) Start(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.Handle("/convert", conversion.NewWebhookHandler(NewScheme()))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	srv := &http.Server{
		Addr:              s.cfg.BindAddr,
		Handler:           mux,
		ReadHeaderTimeout: s.cfg.ReadHeaderTimeout,
		// MinTLS is set on the TLSConfig produced by the Go stdlib defaults
		// for ListenAndServeTLS, but we tighten it explicitly to TLS 1.2.
		TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12},
	}

	s.mu.Lock()
	s.server = srv
	s.mu.Unlock()

	errCh := make(chan error, 1)
	go func() {
		s.log.Info("conversion webhook listening", "addr", s.cfg.BindAddr)
		err := srv.ListenAndServeTLS(s.cfg.CertFile, s.cfg.KeyFile)
		if !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("conversion webhook: %w", err)
			return
		}
		errCh <- nil
	}()

	select {
	case <-ctx.Done():
		// Use a fresh context.Background-rooted timeout because the parent
		// is already canceled; we still want a bounded grace period for
		// in-flight conversions to finish before forcing close.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), s.cfg.ShutdownTimeout)
		defer cancel()
		// Shutdown returns the same http.ErrServerClosed once Start returns;
		// the goroutine above handles propagating any other error.
		_ = srv.Shutdown(shutdownCtx)
		return <-errCh
	case err := <-errCh:
		return err
	}
}
