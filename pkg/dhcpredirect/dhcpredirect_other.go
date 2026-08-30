//go:build !linux

package dhcpredirect

import (
	"context"
	"errors"
	"runtime"
	"time"

	"github.com/go-logr/logr"
)

// errNotLinux is returned by every entry point below. TC eBPF programs,
// network namespaces and netlink are all Linux-only. The rest of the binary
// builds and runs elsewhere; only the DHCP broadcast redirect is unavailable.
var errNotLinux = errors.New("not supported on " + runtime.GOOS + ": the DHCP broadcast redirect needs Linux TC eBPF and network namespaces")

// Redirector is the handle [Setup] would return on Linux. It exists here so
// callers compile on other platforms.
type Redirector struct{}

// Setup always fails on this platform.
func Setup(logr.Logger, Config) (*Redirector, error) { return nil, errNotLinux }

// Close is a no-op on this platform.
func (r *Redirector) Close() error { return nil }

// Info is empty on this platform.
func (r *Redirector) Info() Info { return Info{} }

// LogCounters is a no-op on this platform.
func (r *Redirector) LogCounters(context.Context, time.Duration) {}

// Stats always fails on this platform.
func (r *Redirector) Stats() (Stats, error) { return Stats{}, errNotLinux }

// DefaultRouteInterface always fails on this platform.
func DefaultRouteInterface() (string, error) { return "", errNotLinux }
