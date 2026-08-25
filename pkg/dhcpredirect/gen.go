//go:build bpfgen

// The bpfgen build tag keeps this directive out of `go generate ./...`.
// Compiling bpf/dhcp_redirect.c needs clang with a BPF target plus the libbpf
// and kernel uapi headers, which the ordinary build and CI do not have and do
// not need: the compiled object is committed alongside the Go bindings it is
// embedded in. Regenerate with `make generate-bpf` after editing the C source.

package dhcpredirect

//go:generate go tool bpf2go -cc clang -cflags "-O2 -g -Wall -Werror -Wno-address-of-packed-member" -target bpfel,bpfeb -tags linux dhcpRedirect ./bpf/dhcp_redirect.c
