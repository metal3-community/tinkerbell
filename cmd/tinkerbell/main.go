package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

// CRD generation: one invocation per custom-resource API version so each
// version's CRDs land in its own subdir (bases/v1alpha1, bases/v1alpha2),
// matching the //go:embed layout in crd/crd.go. The crdVersions flag below
// is the apiextensions.k8s.io CRD object version (always v1) — it is NOT
// for selecting custom-resource versions; those come from each package's
// GroupVersion.
//
//go:generate go tool controller-gen crd:crdVersions=v1 paths="../../api/v1alpha1/..." output:crd:artifacts:config=../../crd/bases/v1alpha1
//go:generate go tool controller-gen crd:crdVersions=v1 paths="../../api/v1alpha2/..." output:crd:artifacts:config=../../crd/bases/v1alpha2
//go:generate go tool controller-gen crd:crdVersions=v1 paths="../../api/..." output:crd:artifacts:config=../../crd/bases/merged
//go:generate go tool controller-gen paths="../../..." object:headerFile="../../script/boilerplate.go.txt"
//go:generate go tool buf generate ../.. --config ../../buf.yaml --template ../../buf.gen.yaml --output ../../
func main() {
	ctx, done := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGHUP, syscall.SIGTERM)

	if err := Execute(ctx, done, os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "%s\n", err.Error())
		done()
		os.Exit(1)
	}

	done()
}
