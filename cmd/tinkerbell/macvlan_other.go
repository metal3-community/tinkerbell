//go:build !linux

package main

import (
	"errors"
	"runtime"

	"github.com/go-logr/logr"
)

// errNotLinux is returned by the stubs below. Macvlan setup and default route
// detection are implemented with netlink and network namespaces, both of which
// are Linux-only. The rest of the binary builds and runs on other platforms;
// only these DHCP interface features are unavailable.
var errNotLinux = errors.New("not supported on " + runtime.GOOS + ": requires Linux netlink and network namespaces")

func macvlanIfaceName() (string, error) { return "", errNotLinux }

func setupMacvlan(logr.Logger, string, string) (func(), error) { return nil, errNotLinux }

func defaultRouteInterface() (string, error) { return "", errNotLinux }
