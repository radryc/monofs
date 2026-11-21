package main

import (
	"fmt"
	"net/http"
	"os"

	"github.com/radryc/monofs/internal/transport"
	"google.golang.org/grpc"
)

// transportDialOption resolves the STRATA_TLS_* gRPC dial option. A malformed
// contract is fatal: the CLI cannot safely fall back to plaintext when the
// operator asked for TLS.
func transportDialOption(tr transport.Config) grpc.DialOption {
	opt, err := tr.DialOption()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: transport configuration: %v\n", err)
		os.Exit(1)
	}
	return opt
}

// transportHTTPClient returns base configured with the CLI transport settings.
func transportHTTPClient(tr transport.Config, base *http.Client) *http.Client {
	client, err := tr.HTTPClient(base)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: transport configuration: %v\n", err)
		os.Exit(1)
	}
	return client
}
