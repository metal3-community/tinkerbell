package server

import (
	"errors"
	"io"
	"sync"
	"testing"

	"github.com/go-logr/logr"
	"github.com/tinkerbell/tinkerbell/pkg/tftp/handler"
)

type nopReaderFrom struct{}

func (nopReaderFrom) ReadFrom(io.Reader) (int64, error) { return 0, nil }

// namedHandler returns a handler that records its name into got when served.
func namedHandler(name string, got *string) handler.Handler {
	return handler.HandlerFunc(func(string, io.ReaderFrom) error {
		*got = name
		return nil
	})
}

func TestServeMux_PatternDispatch(t *testing.T) {
	var got string
	mux := NewServeMux()
	mux.Handle(`\.(efi|kpxe|pxe)$`, namedHandler("binary", &got))
	mux.Handle(`^pxelinux\.cfg/`, namedHandler("script", &got))

	tests := []struct {
		filename string
		want     string
	}{
		{"snp.efi", "binary"},
		{"undionly.kpxe", "binary"},
		{"pxelinux.cfg/default", "script"},
	}
	for _, tt := range tests {
		got = ""
		if err := mux.ServeTFTP(tt.filename, nopReaderFrom{}); err != nil {
			t.Fatalf("ServeTFTP(%q) unexpected error: %v", tt.filename, err)
		}
		if got != tt.want {
			t.Errorf("ServeTFTP(%q) dispatched to %q, want %q", tt.filename, got, tt.want)
		}
	}
}

func TestServeMux_FirstMatchWins(t *testing.T) {
	var got string
	mux := NewServeMux()
	mux.Handle(`\.efi$`, namedHandler("first", &got))
	mux.Handle(`snp\.efi$`, namedHandler("second", &got))

	if err := mux.ServeTFTP("snp.efi", nopReaderFrom{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "first" {
		t.Errorf("dispatched to %q, want %q", got, "first")
	}
}

func TestServeMux_DefaultHandler(t *testing.T) {
	var got string
	mux := NewServeMux()
	mux.Handle(`\.efi$`, namedHandler("binary", &got))
	mux.SetDefaultHandler(namedHandler("default", &got))

	if err := mux.ServeTFTP("vmlinuz-x86_64", nopReaderFrom{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "default" {
		t.Errorf("dispatched to %q, want %q", got, "default")
	}
}

func TestServeMux_NoHandler(t *testing.T) {
	mux := NewServeMux()
	mux.Handle(`\.efi$`, handler.NotFoundHandler())

	err := mux.ServeTFTP("vmlinuz-x86_64", nopReaderFrom{})
	if !errors.Is(err, handler.ErrNotFound) {
		t.Errorf("error = %v, want %v", err, handler.ErrNotFound)
	}
}

func TestServeMux_HandleInvalidPatternPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("expected panic for invalid pattern")
		}
	}()
	NewServeMux().Handle(`(unclosed`, handler.NotFoundHandler())
}

func TestServeMux_HandleNilHandlerPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("expected panic for nil handler")
		}
	}()
	NewServeMux().Handle(`\.efi$`, nil)
}

func TestServeMux_HandleFunc(t *testing.T) {
	called := false
	mux := NewServeMux()
	mux.HandleFunc(`\.efi$`, func(string, io.ReaderFrom) error {
		called = true
		return nil
	})

	if err := mux.ServeTFTP("snp.efi", nopReaderFrom{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !called {
		t.Error("handler function was not called")
	}
}

func TestRoutes_Mux(t *testing.T) {
	var got string
	routes := &Routes{}
	routes.Register(`\.efi$`, namedHandler("binary", &got), "")

	if desc := (*routes)[0].Description; desc != "No description provided" {
		t.Errorf("empty description defaulted to %q", desc)
	}

	mux := routes.Mux(logr.Discard())
	if err := mux.ServeTFTP("snp.efi", nopReaderFrom{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "binary" {
		t.Errorf("dispatched to %q, want %q", got, "binary")
	}
}

// TestServeMux_ConcurrentAccess exercises registration, configuration, and
// serving concurrently; it exists to be run with the race detector.
func TestServeMux_ConcurrentAccess(_ *testing.T) {
	mux := NewServeMux()
	mux.Handle(`\.efi$`, handler.NotFoundHandler())

	var wg sync.WaitGroup
	for range 4 {
		wg.Add(3)
		go func() {
			defer wg.Done()
			mux.Handle(`\.pxe$`, handler.NotFoundHandler())
		}()
		go func() {
			defer wg.Done()
			mux.SetLogger(logr.Discard())
			mux.SetDefaultHandler(handler.NotFoundHandler())
		}()
		go func() {
			defer wg.Done()
			for range 100 {
				_ = mux.ServeTFTP("snp.efi", nopReaderFrom{})
			}
		}()
	}
	wg.Wait()
}
