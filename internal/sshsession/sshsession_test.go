package sshsession_test

// Tests for `krayt code`'s SSH material (add-vscode-remote-ssh-session.md). Everything here runs
// offline with no msb, no sshd and no `ssh-keygen`: the package is pure Go by decision 16, and
// these tests are what makes that choice pay.

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/418-cloud/krayt/internal/sshsession"
)

func generate(t *testing.T) *sshsession.Material {
	t.Helper()
	m, err := sshsession.GenerateSession("agent", "run_abc123")
	if err != nil {
		t.Fatalf("GenerateSession: %v", err)
	}
	return m
}

// TestGenerateSessionKeysAreEd25519 checks both halves of decision 10: the keys really are
// ed25519, they really parse as OpenSSH keys (so a real `ssh` will accept them), the public half
// in authorized_keys is the private half's own, and nothing is reused between sessions.
func TestGenerateSessionKeysAreEd25519(t *testing.T) {
	m := generate(t)

	for _, tc := range []struct {
		name       string
		privatePEM []byte
		authorized string
	}{
		{"client", m.ClientPrivatePEM, m.ClientAuthorized},
		{"host", m.HostPrivatePEM, m.HostAuthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			signer, err := ssh.ParsePrivateKey(tc.privatePEM)
			if err != nil {
				t.Fatalf("ParsePrivateKey: %v", err)
			}
			if got := signer.PublicKey().Type(); got != ssh.KeyAlgoED25519 {
				t.Errorf("key type = %q, want %q", got, ssh.KeyAlgoED25519)
			}
			pub, comment, _, _, err := ssh.ParseAuthorizedKey([]byte(tc.authorized))
			if err != nil {
				t.Fatalf("ParseAuthorizedKey(%q): %v", tc.authorized, err)
			}
			if comment == "" || !strings.Contains(comment, "run_abc123") {
				t.Errorf("comment = %q, want one naming the run id", comment)
			}
			if string(pub.Marshal()) != string(signer.PublicKey().Marshal()) {
				t.Error("authorized-keys line is not this private key's own public half")
			}
		})
	}

	if m.ClientAuthorized == m.HostAuthorized {
		t.Error("client and host key are identical — they must be two independent keys")
	}
	other, err := sshsession.GenerateSession("agent", "run_abc123")
	if err != nil {
		t.Fatalf("GenerateSession (second): %v", err)
	}
	if other.ClientAuthorized == m.ClientAuthorized || other.HostAuthorized == m.HostAuthorized {
		t.Error("two sessions produced the same key — material must be ephemeral per session (decision 10)")
	}
}

func TestGenerateSessionRejectsEmptyInputs(t *testing.T) {
	if _, err := sshsession.GenerateSession("", "run_x"); err == nil {
		t.Error("GenerateSession with no user: want error (AllowUsers would be empty and sshd would refuse every login)")
	}
	if _, err := sshsession.GenerateSession("agent", ""); err == nil {
		t.Error("GenerateSession with no run id: want error")
	}
}

// TestSSHMaterialPermissions is the mode contract §8.4 records: 0700 on the directory, 0600 on
// both private keys, 0644 on everything else. ssh refuses a group- or world-readable identity
// outright ("UNPROTECTED PRIVATE KEY FILE"), so getting this wrong breaks every connection.
func TestSSHMaterialPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("windows has no unix file modes; ssh on windows uses ACLs krayt does not set")
	}
	dir := filepath.Join(t.TempDir(), "ssh")
	// Pre-create it group/world-readable so the test proves Write CHANGES the mode rather than
	// merely benefitting from a fresh directory and a lenient umask.
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := sshsession.Write(dir, generate(t)); err != nil {
		t.Fatalf("Write: %v", err)
	}

	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o700 {
		t.Errorf("dir mode = %04o, want 0700", got)
	}

	want := map[string]os.FileMode{
		sshsession.ClientKeyFile:      0o600,
		sshsession.HostKeyFile:        0o600,
		sshsession.ClientPubFile:      0o644,
		sshsession.HostPubFile:        0o644,
		sshsession.AuthorizedKeysFile: 0o644,
		sshsession.KnownHostsFile:     0o644,
		sshsession.SSHDConfigFile:     0o644,
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(want) {
		t.Errorf("Write produced %d files, want %d: %v", len(entries), len(want), entries)
	}
	for name, mode := range want {
		fi, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if got := fi.Mode().Perm(); got != mode {
			t.Errorf("%s mode = %04o, want %04o", name, got, mode)
		}
	}
}

// TestRenderSSHDConfigGolden pins the exact sshd_config `krayt code` generates — decision 10's
// list in full. It is a golden test because every line is load-bearing: `sshd -i` falls back to
// /etc/ssh/sshd_config for anything unset, so a silently dropped directive means the session
// inherits the image's own SSH policy instead of krayt's.
func TestRenderSSHDConfigGolden(t *testing.T) {
	m := generate(t)
	got := sshsession.RenderSSHDConfig(m)
	want := `# krayt code — generated per session, read by ` + "`sshd -i`" + ` (add-vscode-remote-ssh-session.md).
# Ephemeral: this file and both keys die with the run dir. Do not edit.
HostKey /.krayt/ssh/host_ed25519
AuthorizedKeysFile /.krayt/ssh/authorized_keys
AllowUsers agent
PermitRootLogin no
PubkeyAuthentication yes
PasswordAuthentication no
KbdInteractiveAuthentication no
UsePAM no
StrictModes no
PidFile none
X11Forwarding no
PrintMotd no
AllowTcpForwarding yes
Subsystem sftp /usr/lib/openssh/sftp-server
`
	if got != want {
		t.Errorf("RenderSSHDConfig =\n%s\nwant\n%s", got, want)
	}

	// The login user is the sandbox's own, whatever the image's USER is (§8.2) — never hardcoded.
	node, err := sshsession.GenerateSession("node", "run_node")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sshsession.RenderSSHDConfig(node), "AllowUsers node\n") {
		t.Error("AllowUsers does not follow the resolved sandbox user")
	}
}

// TestRenderSSHConfigGolden pins the client config, including the ProxyCommand that IS the
// transport: no port, no HostName that resolves to anything, just krayt piping bytes.
func TestRenderSSHConfigGolden(t *testing.T) {
	got := sshsession.RenderSSHConfig(sshsession.ClientConfig{
		RunID:    "run_abc123",
		User:     "agent",
		Dir:      "/repo/.krayt/runs/run_abc123/ssh",
		KraytExe: "/usr/local/bin/krayt",
		RepoPath: "/repo",
	})
	want := `# krayt code — session run_abc123. Generated; do not edit. Dies with the run dir.
Host krayt-run_abc123
    HostName krayt-run_abc123
    User agent
    HostKeyAlias krayt-run_abc123
    IdentityFile "/repo/.krayt/runs/run_abc123/ssh/id_ed25519"
    IdentitiesOnly yes
    UserKnownHostsFile "/repo/.krayt/runs/run_abc123/ssh/known_hosts"
    StrictHostKeyChecking yes
    ProxyCommand "/usr/local/bin/krayt" code --stdio run_abc123 --repo "/repo"
    ForwardAgent no
    ServerAliveInterval 30
`
	if got != want {
		t.Errorf("RenderSSHConfig =\n%s\nwant\n%s", got, want)
	}
	// Decision 17's guard at this layer: the config must not name a port or a published address —
	// there is none, and adding one would mean ingress had been opened somewhere.
	for _, forbidden := range []string{"\n    Port ", "localhost", "127.0.0.1"} {
		if strings.Contains(got, forbidden) {
			t.Errorf("ssh config contains %q — krayt code publishes no port (decision 17)", forbidden)
		}
	}
}

// TestRenderSSHConfigQuotesSpacedPaths covers the ordinary macOS case (`/Users/Jane Doe/...`):
// ssh_config splits values on whitespace unless they are quoted.
func TestRenderSSHConfigQuotesSpacedPaths(t *testing.T) {
	got := sshsession.RenderSSHConfig(sshsession.ClientConfig{
		RunID: "run_x", User: "agent",
		Dir:      "/Users/Jane Doe/p/.krayt/runs/run_x/ssh",
		KraytExe: "/Users/Jane Doe/bin/krayt",
		RepoPath: "/Users/Jane Doe/p",
	})
	for _, want := range []string{
		`IdentityFile "/Users/Jane Doe/p/.krayt/runs/run_x/ssh/id_ed25519"`,
		`ProxyCommand "/Users/Jane Doe/bin/krayt" code --stdio run_x --repo "/Users/Jane Doe/p"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("ssh config missing %q:\n%s", want, got)
		}
	}
}

func TestWriteClientConfigRefusesUnquotablePaths(t *testing.T) {
	dir := t.TempDir()
	err := sshsession.WriteClientConfig(dir, sshsession.ClientConfig{
		RunID: "run_x", User: "agent", Dir: dir,
		KraytExe: `/opt/kr"ayt/krayt`, RepoPath: "/repo",
	})
	if err == nil {
		t.Fatal("WriteClientConfig with a quote in a path: want an error, not a silently broken config")
	}
}

// TestRenderKnownHostsPinsHostKey is decision 11's payoff: the host key is in known_hosts before
// the first connection, keyed by the per-run alias, so `StrictHostKeyChecking yes` connects with
// no prompt and no trust-on-first-use window.
func TestRenderKnownHostsPinsHostKey(t *testing.T) {
	m := generate(t)
	got := sshsession.RenderKnownHosts(m)

	marker, hosts, pub, _, rest, err := ssh.ParseKnownHosts([]byte(got))
	if err != nil {
		t.Fatalf("ParseKnownHosts(%q): %v", got, err)
	}
	if marker != "" {
		t.Errorf("marker = %q, want none (not a @cert-authority/@revoked line)", marker)
	}
	if len(hosts) != 1 || hosts[0] != "krayt-run_abc123" {
		t.Errorf("hosts = %v, want [krayt-run_abc123] (the per-run alias, decision 11)", hosts)
	}
	if len(strings.TrimSpace(string(rest))) != 0 {
		t.Errorf("known_hosts has %d trailing bytes, want exactly one pinned key", len(rest))
	}

	hostSigner, err := ssh.ParsePrivateKey(m.HostPrivatePEM)
	if err != nil {
		t.Fatal(err)
	}
	if string(pub.Marshal()) != string(hostSigner.PublicKey().Marshal()) {
		t.Error("known_hosts pins a key that is not the guest host key krayt generated")
	}
	clientSigner, err := ssh.ParsePrivateKey(m.ClientPrivatePEM)
	if err != nil {
		t.Fatal(err)
	}
	if string(pub.Marshal()) == string(clientSigner.PublicKey().Marshal()) {
		t.Error("known_hosts pins the CLIENT key — the host key and the login key must not be the same key")
	}
}

func TestRenderAuthorizedKeysHoldsOnlyTheClientKey(t *testing.T) {
	m := generate(t)
	got := sshsession.RenderAuthorizedKeys(m)
	if got != m.ClientAuthorized+"\n" {
		t.Errorf("RenderAuthorizedKeys = %q, want the client key alone", got)
	}
	if strings.Contains(got, m.HostAuthorized) {
		t.Error("authorized_keys contains the host key — a host key must never authorize a login")
	}
}

func TestVSCodeRemoteURI(t *testing.T) {
	if got, want := sshsession.VSCodeURI(sshsession.Alias("run_abc123"), "/workspace"),
		"vscode-remote://ssh-remote+krayt-run_abc123/workspace"; got != want {
		t.Errorf("VSCodeURI = %q, want %q", got, want)
	}
	if got, want := sshsession.Alias("run_abc123"), "krayt-run_abc123"; got != want {
		t.Errorf("Alias = %q, want %q (same name as the msb sandbox)", got, want)
	}
}

func TestAggregateConfigIncludesLiveSessions(t *testing.T) {
	stateDir := t.TempDir()
	path, err := sshsession.WriteAggregateConfig(stateDir, []string{"/a/ssh/config", "/b/ssh/config"})
	if err != nil {
		t.Fatalf("WriteAggregateConfig: %v", err)
	}
	if want := sshsession.AggregateConfigPath(stateDir); path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	for _, want := range []string{
		"#     Include " + path + "\n",
		"Include /a/ssh/config\n",
		"Include /b/ssh/config\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("aggregate config missing %q:\n%s", want, got)
		}
	}

	// Rewritten from scratch each time, so a session that has gone away stops being offered.
	if _, err := sshsession.WriteAggregateConfig(stateDir, nil); err != nil {
		t.Fatalf("WriteAggregateConfig (empty): %v", err)
	}
	b, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "Include /a/ssh/config") {
		t.Error("aggregate config still Includes a session that is no longer live")
	}
}
