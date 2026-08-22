package orchestrator_test

// Host-side tests for add-tls-mitm-credential-injection.md §2 (secrets partitioning) and §5 (CA
// cert delivery): assert directly on the captured proto messages sent to the guest, not on
// downstream container effects — a spyGuestAgent wraps the real guest.Service so the rest of the
// pipeline (bundle ingest, Start, artifact collection) still runs for real over the fake
// provider, exactly like this package's other end-to-end tests.

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"

	"github.com/418-cloud/krayt/internal/guest"
	"github.com/418-cloud/krayt/internal/orchestrator"
	"github.com/418-cloud/krayt/internal/protocol/pb"
	"github.com/418-cloud/krayt/internal/provider/fake"
	"github.com/418-cloud/krayt/internal/task"
)

// spyGuestAgent wraps a real guest.Service and records the exact SecretsBundle/NetworkPolicy
// protos it receives on the wire.
type spyGuestAgent struct {
	*guest.Service
	mu      sync.Mutex
	secrets *pb.SecretsBundle
	network *pb.NetworkPolicy
}

func (s *spyGuestAgent) PushSecrets(ctx context.Context, req *pb.SecretsBundle) (*pb.Ack, error) {
	s.mu.Lock()
	s.secrets = req
	s.mu.Unlock()
	return s.Service.PushSecrets(ctx, req)
}

func (s *spyGuestAgent) SetNetworkPolicy(ctx context.Context, req *pb.NetworkPolicy) (*pb.Ack, error) {
	s.mu.Lock()
	s.network = req
	s.mu.Unlock()
	return s.Service.SetNetworkPolicy(ctx, req)
}

func (s *spyGuestAgent) capturedSecrets() *pb.SecretsBundle {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.secrets
}

func (s *spyGuestAgent) capturedNetwork() *pb.NetworkPolicy {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.network
}

// TestSecretsBundleOmitsInjectedKeys is the §2 load-bearing assertion: for a spec with injection
// configured, the SecretsBundle actually sent over the wire contains NO injected key — a
// non-injected secret in the same file still ships normally.
func TestSecretsBundleOmitsInjectedKeys(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	src := newRepo(t, map[string]string{"greeting.txt": "hello\n"})
	img := minimalImage(ctx, t)

	secretsFile := filepath.Join(t.TempDir(), "secrets.env")
	if err := os.WriteFile(secretsFile, []byte(
		"ANTHROPIC_API_KEY=sk-ant-injected-must-not-ship\n"+
			"OTHER_SECRET=other-value-should-still-ship\n",
	), 0o600); err != nil {
		t.Fatal(err)
	}

	spy := &spyGuestAgent{Service: guest.NewService(guest.WithRunner(&capturingRunner{}), guest.WithRoot(t.TempDir()))}
	p := &fake.Provider{Register: func(s *grpc.Server) { pb.RegisterGuestAgentServer(s, spy) }}

	runDir := filepath.Join(t.TempDir(), "run")
	spec := task.RunSpec{
		ID: "run_secrets_partition", ImageRef: "img@sha256:abc", RepoPath: src, BundleDepth: 1,
		TaskPrompt: []byte("task"), SecretsPath: secretsFile,
		Network: task.NetworkPolicy{
			Mode: task.NetworkAllowlist, Allow: []string{"api.anthropic.com"}, MITM: true,
			Inject: []task.InjectRule{{
				Host: "api.anthropic.com", Strip: []string{"x-api-key"},
				Set: map[string]string{"x-api-key": "ANTHROPIC_API_KEY"},
			}},
		},
	}
	if _, err := orchestrator.Run(ctx, orchestrator.Deps{Provider: p, Image: img}, spec, runDir); err != nil {
		t.Fatalf("Run: %v", err)
	}

	bundle := spy.capturedSecrets()
	if bundle == nil {
		t.Fatal("guest never received PushSecrets")
	}
	if _, present := bundle.GetValues()["ANTHROPIC_API_KEY"]; present {
		t.Error("SecretsBundle contains the injected key ANTHROPIC_API_KEY — it must be withheld (§2)")
	}
	if got := bundle.GetValues()["OTHER_SECRET"]; got != "other-value-should-still-ship" {
		t.Errorf("SecretsBundle[OTHER_SECRET] = %q, want it to still ship unmodified", got)
	}
}

// TestNetworkPolicyCarriesCACertWhenMITMEnabled proves the guest receives the run's ephemeral
// MITM CA public certificate over the existing NetworkPolicy path when network.mitm is true —
// and that what it receives is exactly ONE PEM CERTIFICATE block and nothing else. "Non-empty"
// alone would be satisfied by a bundle with the CA's PRIVATE KEY block appended, which is the
// shape that would matter: the parent validates it (isCACertPEM, egressproxy.go), so this is the
// property being pinned where it is actually observable on the wire.
func TestNetworkPolicyCarriesCACertWhenMITMEnabled(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	src := newRepo(t, map[string]string{"greeting.txt": "hello\n"})
	img := minimalImage(ctx, t)

	spy := &spyGuestAgent{Service: guest.NewService(guest.WithRunner(&capturingRunner{}), guest.WithRoot(t.TempDir()))}
	p := &fake.Provider{Register: func(s *grpc.Server) { pb.RegisterGuestAgentServer(s, spy) }}

	runDir := filepath.Join(t.TempDir(), "run")
	spec := task.RunSpec{
		ID: "run_ca_cert_pushed", ImageRef: "img@sha256:abc", RepoPath: src, BundleDepth: 1,
		TaskPrompt: []byte("task"),
		Network:    task.NetworkPolicy{Mode: task.NetworkAllowlist, Allow: []string{"api.anthropic.com"}, MITM: true},
	}
	if _, err := orchestrator.Run(ctx, orchestrator.Deps{Provider: p, Image: img}, spec, runDir); err != nil {
		t.Fatalf("Run: %v", err)
	}

	np := spy.capturedNetwork()
	if np == nil {
		t.Fatal("guest never received SetNetworkPolicy")
	}
	if len(np.GetCaCert()) == 0 {
		t.Fatal("NetworkPolicy.ca_cert is empty, want the run's ephemeral MITM CA public cert")
	}
	block, rest := pem.Decode(np.GetCaCert())
	if block == nil {
		t.Fatal("NetworkPolicy.ca_cert is not PEM")
	}
	if block.Type != "CERTIFICATE" {
		t.Errorf("ca_cert PEM block type = %q, want CERTIFICATE", block.Type)
	}
	if _, err := x509.ParseCertificate(block.Bytes); err != nil {
		t.Errorf("ca_cert does not parse as an X.509 certificate: %v", err)
	}
	// No second block of ANY type — a trailing PRIVATE KEY is the whole point of checking.
	if extra, _ := pem.Decode(rest); extra != nil {
		t.Errorf("ca_cert carries a second PEM block of type %q; want the certificate alone", extra.Type)
	}
	if len(bytes.TrimSpace(rest)) != 0 {
		t.Errorf("ca_cert carries %d trailing bytes after the certificate block", len(bytes.TrimSpace(rest)))
	}
}

// TestNetworkPolicyOmitsCACertWhenMITMDisabled is the mitm:false byte-identical guarantee's
// proto-level half: no CA in the pushed NetworkPolicy when the feature is off.
func TestNetworkPolicyOmitsCACertWhenMITMDisabled(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	src := newRepo(t, map[string]string{"greeting.txt": "hello\n"})
	img := minimalImage(ctx, t)

	spy := &spyGuestAgent{Service: guest.NewService(guest.WithRunner(&capturingRunner{}), guest.WithRoot(t.TempDir()))}
	p := &fake.Provider{Register: func(s *grpc.Server) { pb.RegisterGuestAgentServer(s, spy) }}

	runDir := filepath.Join(t.TempDir(), "run")
	spec := task.RunSpec{
		ID: "run_ca_cert_absent", ImageRef: "img@sha256:abc", RepoPath: src, BundleDepth: 1,
		TaskPrompt: []byte("task"),
		Network:    task.NetworkPolicy{Mode: task.NetworkAllowlist, Allow: []string{"api.anthropic.com"}}, // MITM left false
	}
	if _, err := orchestrator.Run(ctx, orchestrator.Deps{Provider: p, Image: img}, spec, runDir); err != nil {
		t.Fatalf("Run: %v", err)
	}

	np := spy.capturedNetwork()
	if np == nil {
		t.Fatal("guest never received SetNetworkPolicy")
	}
	if len(np.GetCaCert()) != 0 {
		t.Errorf("NetworkPolicy.ca_cert = %d bytes, want empty when mitm is off", len(np.GetCaCert()))
	}
}
