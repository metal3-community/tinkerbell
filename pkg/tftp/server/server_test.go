package server

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/tinkerbell/tinkerbell/pkg/tftp/handler"
)

func TestConfig_SetDefaults(t *testing.T) {
	c := &Config{}
	c.setDefaults()
	if c.Timeout != DefaultTimeout {
		t.Errorf("Timeout = %v, want %v", c.Timeout, DefaultTimeout)
	}
	if c.BlockSize != DefaultBlockSize {
		t.Errorf("BlockSize = %v, want %v", c.BlockSize, DefaultBlockSize)
	}

	c = &Config{Timeout: time.Second, BlockSize: 1468}
	c.setDefaults()
	if c.Timeout != time.Second || c.BlockSize != 1468 {
		t.Errorf("setDefaults overwrote explicit values: %+v", c)
	}
}

func TestHandleWrite_RejectsWrites(t *testing.T) {
	err := handleWrite(logr.Discard())("evil.bin", nil)
	if !errors.Is(err, os.ErrPermission) {
		t.Errorf("error = %v, want wrapped %v", err, os.ErrPermission)
	}
}

func TestServe_NilMux(t *testing.T) {
	c := &Config{}
	if err := c.Serve(context.Background(), logr.Discard(), "127.0.0.1:0", nil); err == nil {
		t.Error("expected error for nil mux")
	}
}

func TestServe_ListenError(t *testing.T) {
	mux := NewServeMux()
	mux.SetDefaultHandler(handler.NotFoundHandler())

	c := &Config{}
	err := c.Serve(context.Background(), logr.Discard(), "invalid-address", mux)
	if err == nil {
		t.Error("expected error for invalid listen address")
	}
}

func TestServe_ShutdownOnContextCancel(t *testing.T) {
	mux := NewServeMux()
	mux.SetDefaultHandler(handler.NotFoundHandler())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- (&Config{}).Serve(ctx, logr.Discard(), "127.0.0.1:0", mux)
	}()

	// Give the server a moment to start listening, then cancel.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Serve returned %v after shutdown, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after context cancellation")
	}
}
