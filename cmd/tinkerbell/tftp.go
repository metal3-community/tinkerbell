package main

import (
	"context"
	"fmt"
	"net/netip"

	"github.com/tinkerbell/tinkerbell/cmd/tinkerbell/flag"
	tftpserver "github.com/tinkerbell/tinkerbell/pkg/tftp/server"
)

// startTFTPServer registers all TFTP routes and starts the TFTP server.
// It blocks until ctx is cancelled.
func startTFTPServer(ctx context.Context, globals *flag.GlobalConfig, s *flag.SmeeConfig) error {
	ll := ternary((s.LogLevel != 0), s.LogLevel, globals.LogLevel)
	tftpLog := getLogger(ll).WithName("tftp")

	if !globals.EnableSmee || !s.Config.TFTP.Enabled {
		tftpLog.Info("tftp service is disabled")
		return nil
	}

	routeList := &tftpserver.Routes{}

	// Smee TFTP handlers
	if h := s.Config.BinaryTFTPHandler(tftpLog.WithName("binary")); h != nil {
		routeList.Register(patternIPXEBinary, h, "smee iPXE binary TFTP handler")
	}
	if h := s.Config.ScriptTFTPHandler(tftpLog.WithName("script")); h != nil {
		routeList.Register(patternIPXEScript, h, "smee iPXE script TFTP handler")
	}

	addrPort := netip.AddrPortFrom(s.Config.TFTP.BindAddr, s.Config.TFTP.BindPort)
	if !addrPort.IsValid() {
		return fmt.Errorf("invalid TFTP bind address: IP: %v, Port: %v", addrPort.Addr(), addrPort.Port())
	}

	mux := routeList.Mux(tftpLog)

	// OSIE TFTP handler (serves OSIE files as the default/fallback handler)
	if globals.EnableOSIE {
		mux.SetDefaultHandler(s.Config.OSIETFTPHandler(tftpLog.WithName("osie")))
	}

	srv := &tftpserver.Config{
		Anticipate:       s.Config.TFTP.Anticipate,
		BlockSize:        s.Config.TFTP.BlockSize,
		EnableSinglePort: s.Config.TFTP.SinglePort,
		Timeout:          s.Config.TFTP.Timeout,
	}

	tftpLog.Info("starting TFTP server", "addr", addrPort.String(), "registeredRoutes", routeList)
	return srv.Serve(ctx, tftpLog, addrPort.String(), mux)
}
