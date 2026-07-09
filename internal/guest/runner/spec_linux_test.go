//go:build linux

package runner

import (
	"context"
	"testing"

	"github.com/containerd/containerd/v2/pkg/oci"
	specs "github.com/opencontainers/runtime-spec/specs-go"

	"github.com/418-cloud/krayt/internal/guest"
)

// seededSpec mimics the state containerd's default spec + WithImageConfig would leave: a non-root
// process with a populated capability set and a non-empty ambient set, a Linux section, and a
// writable root. The security opts must reduce this to least privilege.
func seededSpec(uid uint32) *specs.Spec {
	full := []string{"CAP_CHOWN", "CAP_SETUID", "CAP_SETGID", "CAP_NET_BIND_SERVICE"}
	return &specs.Spec{
		Version: specs.Version,
		Process: &specs.Process{
			User:            specs.User{UID: uid},
			NoNewPrivileges: true,
			Capabilities: &specs.LinuxCapabilities{
				Bounding:    full,
				Effective:   full,
				Permitted:   full,
				Inheritable: full,
				Ambient:     []string{"CAP_CHOWN"},
			},
		},
		Linux: &specs.Linux{},
		Root:  &specs.Root{},
	}
}

func applyOpts(t *testing.T, s *specs.Spec, opts []oci.SpecOpts) error {
	t.Helper()
	for _, o := range opts {
		if err := o(context.Background(), nil, nil, s); err != nil {
			return err
		}
	}
	return nil
}

func TestSecuritySpecOptsDropsAllCapsByDefault(t *testing.T) {
	s := seededSpec(1000)
	if err := applyOpts(t, s, securitySpecOpts(guest.RunConfig{})); err != nil {
		t.Fatalf("apply: %v", err)
	}
	caps := s.Process.Capabilities
	for name, set := range map[string][]string{
		"Bounding": caps.Bounding, "Effective": caps.Effective,
		"Permitted": caps.Permitted, "Inheritable": caps.Inheritable, "Ambient": caps.Ambient,
	} {
		if len(set) != 0 {
			t.Errorf("%s = %v, want empty (drop all)", name, set)
		}
	}
	// Default applies the seccomp profile and keeps NoNewPrivileges + a writable rootfs.
	if s.Linux.Seccomp == nil {
		t.Error("default must apply a seccomp profile (Linux.Seccomp = nil)")
	}
	if !s.Process.NoNewPrivileges {
		t.Error("NoNewPrivileges must remain true")
	}
	if s.Root.Readonly {
		t.Error("default rootfs must be writable")
	}
}

func TestSecuritySpecOptsOptInCapability(t *testing.T) {
	s := seededSpec(1000)
	cfg := guest.RunConfig{AddCapabilities: []string{"CAP_NET_BIND_SERVICE"}}
	if err := applyOpts(t, s, securitySpecOpts(cfg)); err != nil {
		t.Fatalf("apply: %v", err)
	}
	c := s.Process.Capabilities
	if len(c.Bounding) != 1 || c.Bounding[0] != "CAP_NET_BIND_SERVICE" {
		t.Errorf("Bounding = %v, want [CAP_NET_BIND_SERVICE]", c.Bounding)
	}
	if len(c.Effective) != 1 || len(c.Permitted) != 1 {
		t.Errorf("effective/permitted not set to the opt-in cap: %+v", c)
	}
	// The opt-in grants Effective/Permitted/Bounding but never re-populates Ambient (§10).
	if len(c.Ambient) != 0 {
		t.Errorf("Ambient = %v, want empty", c.Ambient)
	}
}

func TestEnforceNonRootRejectsUID0(t *testing.T) {
	// A root image (uid 0, e.g. unset USER) must fail the run.
	s := seededSpec(0)
	if err := applyOpts(t, s, securitySpecOpts(guest.RunConfig{})); err == nil {
		t.Error("expected an error for a uid-0 (root) image")
	}
	// A non-root image passes.
	s = seededSpec(1000)
	if err := applyOpts(t, s, securitySpecOpts(guest.RunConfig{})); err != nil {
		t.Errorf("non-root image should not error: %v", err)
	}
}

func TestSecuritySpecOptsSeccompOptOut(t *testing.T) {
	s := seededSpec(1000)
	if err := applyOpts(t, s, securitySpecOpts(guest.RunConfig{SeccompUnconfined: true})); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if s.Linux.Seccomp != nil {
		t.Error("seccomp: unconfined must leave Linux.Seccomp = nil")
	}
}

func TestSecuritySpecOptsReadonlyRootfs(t *testing.T) {
	s := seededSpec(1000)
	if err := applyOpts(t, s, securitySpecOpts(guest.RunConfig{ReadonlyRootfs: true})); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !s.Root.Readonly {
		t.Error("ReadonlyRootfs=true must set Root.Readonly")
	}
}

// TestContractMountsReadonlyOrdering guards the shadowing bug: with a read-only rootfs the tmpfs
// /run mount must precede the /run/secrets bind, or the tmpfs would hide the secrets (§8.2).
func TestContractMountsReadonlyOrdering(t *testing.T) {
	cfg := guest.RunConfig{
		WorkspaceDir: "/w", TaskPath: "/t/prompt.md", OutputDir: "/o",
		SecretsDir: "/s", ReadonlyRootfs: true,
	}
	mounts := contractMounts(cfg)
	runIdx, secretsIdx, tmpIdx := -1, -1, -1
	for i, m := range mounts {
		switch m.Destination {
		case "/run":
			runIdx = i
		case "/tmp":
			tmpIdx = i
		case guest.ContainerSecrets: // /run/secrets
			secretsIdx = i
		}
	}
	if tmpIdx < 0 || runIdx < 0 || secretsIdx < 0 {
		t.Fatalf("missing mounts: tmp=%d run=%d secrets=%d in %+v", tmpIdx, runIdx, secretsIdx, mounts)
	}
	if runIdx > secretsIdx {
		t.Errorf("/run tmpfs (idx %d) must come before /run/secrets bind (idx %d) or it shadows the secrets", runIdx, secretsIdx)
	}
	// A writable rootfs (default) must NOT inject the tmpfs mounts.
	if got := contractMounts(guest.RunConfig{WorkspaceDir: "/w", TaskPath: "/t/prompt.md", OutputDir: "/o", SecretsDir: "/s"}); hasDest(got, "/tmp") || hasDest(got, "/run") {
		t.Error("default (writable) rootfs must not add tmpfs /tmp or /run mounts")
	}
}

func hasDest(mounts []specs.Mount, dest string) bool {
	for _, m := range mounts {
		if m.Destination == dest {
			return true
		}
	}
	return false
}
