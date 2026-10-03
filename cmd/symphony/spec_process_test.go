package main

import (
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"os"
	"testing"
)

// This subprocess harness adds only the fixture CA, then executes the real CLI
// entry point. Release-binary TLS defaults and OS trust stores remain untouched.
func TestSpecCLIProcessMain(t *testing.T) {
	if os.Getenv("SPEC_CLI_PROCESS") != "1" {
		return
	}
	certificates, err := os.ReadFile(os.Getenv("SPEC_CLI_CA_FILE"))
	if err != nil {
		t.Fatal(err)
	}
	roots, err := x509.SystemCertPool()
	if err != nil {
		t.Fatal(err)
	}
	if !roots.AppendCertsFromPEM(certificates) {
		t.Fatal("fixture CA has no certificates")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{RootCAs: roots}
	http.DefaultTransport = transport
	os.Args = []string{"symphony", os.Getenv("SPEC_CLI_WORKFLOW")}
	main()
	// Prevent Go's test runner from emitting a PASS banner in CLI output.
	os.Exit(0)
}
