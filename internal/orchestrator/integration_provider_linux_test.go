//go:build integration && linux

package orchestrator_test

import (
	"testing"

	"github.com/418-cloud/krayt/internal/provider"
	"github.com/418-cloud/krayt/internal/provider/firecracker"
)

// newTestProvider builds the Linux VM backend for the real-VM integration tests. It is the only
// platform-specific line in them: everything above the Provider interface — orchestrator,
// protocol, patch, secrets — is the same code on both OSes (§6.3), so the tests themselves are
// shared verbatim. That is precisely what Phase 7's "Done when" asks us to demonstrate.
//
// The test binary must hold CAP_NET_ADMIN to create each VM's tap device, and the invoking user
// must be able to open /dev/kvm — see the header of integration_test.go.
func newTestProvider(t *testing.T) provider.Provider {
	t.Helper()
	return firecracker.New("", t.TempDir())
}
