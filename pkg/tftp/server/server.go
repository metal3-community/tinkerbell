// Package server provides a TFTP server for Tinkerbell.
package server

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/go-logr/logr"
	"github.com/pin/tftp/v3"
	"github.com/tinkerbell/tinkerbell/pkg/tftp/handler"
	"github.com/tinkerbell/tinkerbell/pkg/tftp/middleware"
)

const (
	// DefaultTimeout is the default TFTP transfer timeout.
	DefaultTimeout = 5 * time.Second
	// DefaultBlockSize is the default TFTP block size.
	DefaultBlockSize = 512
	// DefaultAnticipate is the default number of blocks to send ahead.
	DefaultAnticipate = 0
)

// Config is the configuration for the TFTP server.
type Config struct {
	// Anticipate is the number of blocks to send ahead of ACKs.
	Anticipate uint
	// BlockSize is the TFTP block size in bytes.
	BlockSize int
	// EnableSinglePort enables single-port mode for TFTP.
	EnableSinglePort bool
	// Timeout is the TFTP transfer timeout.
	Timeout time.Duration
}

// Option configures a Config.
type Option func(*Config)

// NewConfig returns a Config with sensible defaults, modified by the given options.
func NewConfig(opts ...Option) *Config {
	cfg := &Config{
		Timeout:   DefaultTimeout,
		BlockSize: DefaultBlockSize,
	}
	for _, opt := range opts {
		opt(cfg)
	}
	return cfg
}

func (c *Config) setDefaults() {
	if c.Timeout == 0 {
		c.Timeout = DefaultTimeout
	}
	if c.BlockSize == 0 {
		c.BlockSize = DefaultBlockSize
	}
}

// Serve starts the TFTP server on the given address with the provided ServeMux
// and blocks until ctx is cancelled.
func (c *Config) Serve(ctx context.Context, log logr.Logger, addr string, mux *ServeMux) error {
	c.setDefaults()

	server := tftp.NewServer(mux.ServeTFTP, handleWrite(log))
	server.SetTimeout(c.Timeout)
	server.SetBlockSize(c.BlockSize)
	server.SetAnticipate(c.Anticipate)
	server.SetHook(middleware.NewTransferHook(log))

	if c.EnableSinglePort {
		server.EnableSinglePort()
	}

	go func() {
		<-ctx.Done()
		server.Shutdown()
	}()

	log.Info("starting tftp server", "addr", addr)
	if err := server.ListenAndServe(addr); err != nil {
		return fmt.Errorf("tftp server error: %w", err)
	}
	return nil
}

// handleWrite returns a TFTP write handler that always rejects writes.
func handleWrite(log logr.Logger) func(string, io.WriterTo) error {
	return func(filename string, _ io.WriterTo) error {
		err := fmt.Errorf("access_violation: %w", os.ErrPermission)
		log.Error(err, "tftp write request rejected", "filename", filename)
		return err
	}
}

// DefaultHandler is a convenience type alias for use when setting a default handler on the mux.
type DefaultHandler = handler.Handler
