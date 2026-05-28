package main

import (
	"fmt"
	"os"
	"path"
	"strings"

	"github.com/tinkerbell/tinkerbell/cmd/tinkerbell/flag"
	"github.com/tinkerbell/tinkerbell/pkg/backend/kube"
	"k8s.io/client-go/rest"
)

// readCABundle loads a PEM-encoded CA bundle from disk and returns its
// bytes. Callers pass it to crd.WithConversionWebhook so the kube
// apiserver can validate the conversion webhook's TLS certificate.
func readCABundle(filePath string) ([]byte, error) {
	if filePath == "" {
		return nil, fmt.Errorf("CA bundle file path is empty")
	}
	b, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("reading CA bundle %s: %w", filePath, err)
	}
	if len(b) == 0 {
		return nil, fmt.Errorf("CA bundle %s is empty", filePath)
	}
	return b, nil
}

func ternary[T any](condition bool, valueIfTrue, valueIfFalse T) T {
	if condition {
		return valueIfTrue
	}
	return valueIfFalse
}

func numEnabled(globals *flag.GlobalConfig) int {
	n := 0
	if globals.EnableSmee {
		n++
	}
	if globals.EnableTootles {
		n++
	}
	if globals.EnableTinkServer {
		n++
	}
	if globals.EnableTinkController {
		n++
	}
	if globals.EnableRufio {
		n++
	}
	if globals.EnableSecondStar {
		n++
	}
	if globals.EnableUI {
		n++
	}
	return n
}

func enabledIndexes(smeeEnabled, tootlesEnabled, tinkServerEnabled, secondStarEnabled bool) map[kube.IndexType]kube.Index {
	idxs := make(map[kube.IndexType]kube.Index, 0)

	if smeeEnabled {
		idxs = flag.KubeIndexesSmee
	}
	if tootlesEnabled {
		for k, v := range flag.KubeIndexesTootles {
			idxs[k] = v
		}
	}
	if tinkServerEnabled {
		for k, v := range flag.KubeIndexesTinkServer {
			idxs[k] = v
		}
	}
	if secondStarEnabled {
		for k, v := range flag.KubeIndexesSecondStar {
			idxs[k] = v
		}
	}

	return idxs
}

// normalizeURLPrefix ensures a URL prefix is valid for use with http.ServeMux.
// It trims whitespace, ensures a leading "/", cleans the path (collapsing repeated
// slashes, resolving ".." etc.), and ensures a trailing "/" so the mux matches all
// sub-paths.
func normalizeURLPrefix(prefix string) string {
	pattern := strings.TrimSpace(prefix)
	if !strings.HasPrefix(pattern, "/") {
		pattern = "/" + pattern
	}
	pattern = path.Clean(pattern)
	if !strings.HasSuffix(pattern, "/") {
		pattern += "/"
	}
	return pattern
}

// inCluster checks if we are running in cluster.
func inCluster() bool {
	if _, err := rest.InClusterConfig(); err == nil {
		return true
	}
	return false
}
