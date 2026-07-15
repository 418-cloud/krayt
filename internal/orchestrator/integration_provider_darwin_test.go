//go:build integration && darwin

package orchestrator_test

import (
	"testing"

	"github.com/418-cloud/krayt/internal/provider"
	"github.com/418-cloud/krayt/internal/provider/vfkit"
)

// newTestProvider builds the macOS VM backend for the real-VM integration tests. It is the
// only platform-specific line in them: everything above the Provider interface — orchestrator,
// protocol, patch, secrets — is the same code on both OSes (§6.3), so the tests themselves are
// shared verbatim.
func newTestProvider(t *testing.T) provider.Provider {
	t.Helper()
	return vfkit.New("", t.TempDir())
}
