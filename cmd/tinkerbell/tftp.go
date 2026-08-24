package main

import (
	"context"
	"fmt"
	"net/netip"

	"github.com/tinkerbell/tinkerbell/cmd/tinkerbell/flag"
	tftpserver "github.com/tinkerbell/tinkerbell/pkg/tftp/server"
)

// startTFTPServer starts the TFTP server with Smee's route chain.
// It blocks until ctx is cancelled.
func startTFTPServer(ctx context.Context, globals *flag.GlobalConfig, s *flag.SmeeConfig) error {
	ll := ternary((s.LogLevel != 0), s.LogLevel, globals.LogLevel)
	tftpLog := getLogger(ll).WithName("tftp")

	if !globals.EnableSmee || !s.Config.TFTP.Enabled {
		tftpLog.Info("tftp service is disabled")
		return nil
	}

	addrPort := netip.AddrPortFrom(s.Config.TFTP.BindAddr, s.Config.TFTP.BindPort)
	if !addrPort.IsValid() {
		return fmt.Errorf("invalid TFTP bind address: IP: %v, Port: %v", addrPort.Addr(), addrPort.Port())
	}

	h := s.Config.TFTPHandler(tftpLog)
	if h == nil {
		tftpLog.Info("no tftp routes enabled; not starting tftp server")
		return nil
	}

	mux := tftpserver.NewServeMux()
	mux.SetLogger(tftpLog)
	mux.SetDefaultHandler(h)

	srv := &tftpserver.Config{
		Anticipate:       s.Config.TFTP.Anticipate,
		BlockSize:        s.Config.TFTP.BlockSize,
		EnableSinglePort: s.Config.TFTP.SinglePort,
		Timeout:          s.Config.TFTP.Timeout,
	}

	return srv.Serve(ctx, tftpLog, addrPort.String(), mux)
}
