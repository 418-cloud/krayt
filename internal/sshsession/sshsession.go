// Package sshsession generates and renders the per-session SSH material `krayt code` needs
// (add-vscode-remote-ssh-session.md): an ephemeral ed25519 client keypair, an ephemeral ed25519
// guest host key, and the four config files that pin them together — `sshd_config` for the guest,
// `authorized_keys`, a pre-pinned `known_hosts`, and an `ssh_config` block naming a per-run alias.
//
// It is deliberately pure: every renderer is a string-returning function of its inputs with no
// I/O, and the only functions that touch the filesystem (Write, WriteClientConfig,
// WriteAggregateConfig) write inside krayt's own `.krayt/` tree and nowhere else. krayt NEVER edits
// `~/.ssh/config` (decision 12) — it prints the one-time `Include` line for the human to add.
//
// Keys are generated in-process with crypto/ed25519 and marshalled with golang.org/x/crypto/ssh
// rather than by shelling out to `ssh-keygen` (decision 16), so the whole path is unit-testable
// with no subprocess and works identically on a host that ships no OpenSSH client tools.
//
// Nothing here is reused across sessions and nothing outlives the run dir: `krayt rm <run-id>`
// deletes the only copy of both private keys (§10).
package sshsession

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/ssh"
)

// GuestDir is where `krayt code` copies the guest's half of the material (the host key,
// authorized_keys and sshd_config) — under guestbin.GuestRoot, deliberately outside /workspace and
// /output so none of it can land in changes.patch or be collected as an artifact. Spelled as a
// literal rather than imported from internal/sandbox/guestbin so this package stays dependency-free
// and pure; orchestrator/code.go asserts the two agree.
const GuestDir = "/.krayt/ssh"

// Guest-side paths of the three files copied in. sshd reads all three as root.
const (
	GuestHostKeyPath        = GuestDir + "/host_ed25519"
	GuestAuthorizedKeysPath = GuestDir + "/authorized_keys"
	GuestSSHDConfigPath     = GuestDir + "/sshd_config"
)

// GuestSSHDPath is the OpenSSH server binary `krayt code` execs per connection. Debian's
// openssh-server package installs it here, and all three published agent images are Debian-based
// (§8.2, decision 15).
const GuestSSHDPath = "/usr/sbin/sshd"

// GuestSFTPServerPath is Debian's sftp-server, wired up as the `sftp` subsystem so `scp`, `sftp`
// and VS Code's own file operations work over the same channel.
const GuestSFTPServerPath = "/usr/lib/openssh/sftp-server"

// DirName is the per-run subdirectory holding the whole session's material.
const DirName = "ssh"

// File names inside <runDir>/ssh/. The two private keys are 0600; everything else is 0644; the
// directory itself is 0700 (Write).
const (
	ClientKeyFile      = "id_ed25519"
	ClientPubFile      = "id_ed25519.pub"
	HostKeyFile        = "host_ed25519"
	HostPubFile        = "host_ed25519.pub"
	AuthorizedKeysFile = "authorized_keys"
	KnownHostsFile     = "known_hosts"
	SSHDConfigFile     = "sshd_config"
	ClientConfigFile   = "config"
)

// Material is one session's key material plus the facts the renderers need. Both PEM fields hold
// an OpenSSH-format private key; both Authorized fields hold a single-line `ssh-ed25519 AAAA...`
// public key with no trailing newline.
type Material struct {
	// User is the SSH login user — the sandbox's own resolved user (the image's USER,
	// orchestrator.resolveSandboxUser), never root: sshd itself runs as root, but decision 3 puts
	// the LOGIN on the same unprivileged user every other krayt path uses, and sshd_config's
	// PermitRootLogin no enforces that even if the key were ever reused.
	User string
	// RunID is the krayt run id this session belongs to; Alias is derived from it.
	RunID string

	ClientPrivatePEM []byte // OpenSSH private key, 0600
	ClientAuthorized string // "ssh-ed25519 AAAA… krayt-code-<runID>"
	HostPrivatePEM   []byte // OpenSSH private key, 0600
	HostAuthorized   string // "ssh-ed25519 AAAA… krayt-code-host-<runID>"
}

// Alias is the per-run ssh alias (decision 11): `krayt-<run-id>`, matching the msb sandbox name so
// one identifier reads the same in `krayt ls`, `msb ls` and `~/.ssh/config`. It is per-run rather
// than a stable "krayt" alias on purpose — a fresh host key every session against a stable alias is
// exactly the shape that produces OpenSSH's host-key-changed warning.
func Alias(runID string) string { return "krayt-" + runID }

// GenerateSession produces one session's ephemeral material: an ed25519 client keypair the human's
// ssh uses to log in as user, and an ed25519 host key the guest's sshd presents. Both are freshly
// generated per call from crypto/rand; nothing is read from or written to the user's own ~/.ssh.
func GenerateSession(user, runID string) (*Material, error) {
	if user == "" {
		return nil, fmt.Errorf("sshsession: no sandbox user resolved for run %q", runID)
	}
	if runID == "" {
		return nil, fmt.Errorf("sshsession: empty run id")
	}
	clientPEM, clientPub, err := generateKey("krayt-code-" + runID)
	if err != nil {
		return nil, fmt.Errorf("sshsession: client key: %w", err)
	}
	hostPEM, hostPub, err := generateKey("krayt-code-host-" + runID)
	if err != nil {
		return nil, fmt.Errorf("sshsession: host key: %w", err)
	}
	return &Material{
		User: user, RunID: runID,
		ClientPrivatePEM: clientPEM, ClientAuthorized: clientPub,
		HostPrivatePEM: hostPEM, HostAuthorized: hostPub,
	}, nil
}

// generateKey returns one ed25519 keypair as (OpenSSH private key PEM, single-line authorized-keys
// form with comment appended).
func generateKey(comment string) (privPEM []byte, authorized string, err error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, "", err
	}
	// MarshalPrivateKey wants the key by pointer — an ed25519.PrivateKey passed by value marshals
	// as an unsupported type.
	block, err := ssh.MarshalPrivateKey(&priv, comment)
	if err != nil {
		return nil, "", err
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return nil, "", err
	}
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub)))
	return pem.EncodeToMemory(block), line + " " + comment, nil
}

// RenderAuthorizedKeys is the guest's /.krayt/ssh/authorized_keys: exactly one key, this session's
// client key, and nothing else. It is never merged into the sandbox user's own ~/.ssh — sshd is
// pointed at this file by AuthorizedKeysFile instead, so the session leaves no trace in $HOME and
// no state that could outlive the sandbox.
func RenderAuthorizedKeys(m *Material) string {
	return m.ClientAuthorized + "\n"
}

// RenderKnownHosts pins this session's host key against its alias, BEFORE the first connection is
// ever made (decision 11) — which is what lets the generated ssh_config set
// `StrictHostKeyChecking yes` and still connect on the first try with no prompt. There is no
// trust-on-first-use window here at all: krayt generated both halves, so the client knows the host
// key before the host has been asked for it.
func RenderKnownHosts(m *Material) string {
	return Alias(m.RunID) + " " + m.HostAuthorized + "\n"
}

// RenderSSHDConfig is the guest's sshd_config, read by `sshd -i` per connection. Every directive
// is explicit: `sshd -i` still reads /etc/ssh/sshd_config's defaults for anything unset, and this
// session must not inherit an image's own SSH policy.
//
// Two directives are load-bearing enough to name here:
//
//   - `StrictModes no` — the key material lives under /.krayt/ssh, not in the user's $HOME, so
//     sshd's ownership/mode check on ~/.ssh has nothing to check and would only reject a layout
//     krayt chose deliberately (§8.4: none of it may land in /workspace).
//   - `AllowTcpForwarding yes` — this is how VS Code forwards a dev server running INSIDE the
//     sandbox back to the human's browser. It rides the existing SSH connection, which is itself a
//     pipe through `msb exec`, so it needs no published port and no ingress either (§6.6): the
//     forwarded traffic never leaves the stdio channel krayt already owns.
func RenderSSHDConfig(m *Material) string {
	var b strings.Builder
	b.WriteString("# krayt code — generated per session, read by `sshd -i` (add-vscode-remote-ssh-session.md).\n")
	b.WriteString("# Ephemeral: this file and both keys die with the run dir. Do not edit.\n")
	fmt.Fprintf(&b, "HostKey %s\n", GuestHostKeyPath)
	fmt.Fprintf(&b, "AuthorizedKeysFile %s\n", GuestAuthorizedKeysPath)
	fmt.Fprintf(&b, "AllowUsers %s\n", m.User)
	b.WriteString("PermitRootLogin no\n")
	b.WriteString("PubkeyAuthentication yes\n")
	b.WriteString("PasswordAuthentication no\n")
	b.WriteString("KbdInteractiveAuthentication no\n")
	b.WriteString("UsePAM no\n")
	b.WriteString("StrictModes no\n")
	b.WriteString("PidFile none\n")
	b.WriteString("X11Forwarding no\n")
	b.WriteString("PrintMotd no\n")
	b.WriteString("AllowTcpForwarding yes\n")
	fmt.Fprintf(&b, "Subsystem sftp %s\n", GuestSFTPServerPath)
	return b.String()
}

// ClientConfig is everything RenderSSHConfig needs that is not key material: absolute host paths
// and the exact ProxyCommand that reaches this session's sandbox.
type ClientConfig struct {
	RunID string
	User  string
	// Dir is the absolute <runDir>/ssh directory holding the identity and known_hosts.
	Dir string
	// KraytExe is the absolute path to the krayt binary ssh will invoke as the ProxyCommand —
	// resolved with os.Executable rather than assumed to be on PATH, since ssh (and VS Code's own
	// ssh) runs with an environment krayt does not control.
	KraytExe string
	// RepoPath is the absolute host repo whose .krayt/ holds this run, passed to the ProxyCommand
	// so it resolves the same state dir regardless of ssh's working directory.
	RepoPath string
}

// RenderSSHConfig is the per-run ssh client config written to <runDir>/ssh/config (decision 12).
// It is self-contained: `ssh -F <that file> krayt-<id>` works with no other configuration, and the
// stable aggregate at <stateDir>/ssh/config Includes it for editors (VS Code Remote-SSH among
// them) that read only the user's own ~/.ssh/config and offer no -F equivalent.
//
// The ProxyCommand is the whole mechanism: ssh runs `krayt code --stdio <run-id>`, which pipes its
// own stdin/stdout through `msb exec --stream` to a `sshd -i` in the sandbox (§6.16). No port is
// published, no ingress is opened, and the sandbox has no listener — this works even under
// `--net none`.
func RenderSSHConfig(c ClientConfig) string {
	alias := Alias(c.RunID)
	var b strings.Builder
	fmt.Fprintf(&b, "# krayt code — session %s. Generated; do not edit. Dies with the run dir.\n", c.RunID)
	fmt.Fprintf(&b, "Host %s\n", alias)
	fmt.Fprintf(&b, "    HostName %s\n", alias)
	fmt.Fprintf(&b, "    User %s\n", c.User)
	// HostKeyAlias pins which known_hosts line applies, independently of how ssh would otherwise
	// derive a name for a host it reaches through a ProxyCommand.
	fmt.Fprintf(&b, "    HostKeyAlias %s\n", alias)
	fmt.Fprintf(&b, "    IdentityFile %s\n", sshQuote(filepath.Join(c.Dir, ClientKeyFile)))
	b.WriteString("    IdentitiesOnly yes\n")
	fmt.Fprintf(&b, "    UserKnownHostsFile %s\n", sshQuote(filepath.Join(c.Dir, KnownHostsFile)))
	// Safe because krayt pinned the host key into that known_hosts before the first connection —
	// strictly better than the usual accept-on-first-use prompt (decision 11).
	b.WriteString("    StrictHostKeyChecking yes\n")
	fmt.Fprintf(&b, "    ProxyCommand %s code --stdio %s --repo %s\n",
		sshQuote(c.KraytExe), c.RunID, sshQuote(c.RepoPath))
	b.WriteString("    ForwardAgent no\n")
	b.WriteString("    ServerAliveInterval 30\n")
	return b.String()
}

// VSCodeURI is the URI that opens dir inside the session in VS Code — printed, never launched
// (decision 13): krayt depends on no `code` binary being on PATH and ships no editor integration
// beyond this string. Cursor accepts the same shape; JetBrains Gateway and plain `ssh` need only
// the alias.
func VSCodeURI(alias, dir string) string {
	return "vscode-remote://ssh-remote+" + alias + dir
}

// sshQuote renders a path for an ssh_config value. ssh_config splits on whitespace unless the
// value is double-quoted, and a run dir under a host path with a space in it (`/Users/Jane Doe/…`)
// is entirely ordinary on macOS — so quote unconditionally rather than only when it looks needed.
// A double quote or backslash inside a path has no escape in ssh_config's own quoting, so those
// are rejected by the caller (Write) rather than silently mangled here.
func sshQuote(s string) string { return `"` + s + `"` }

// Write lays out <dir> (typically <runDir>/ssh) with this session's material and the modes sshd
// and ssh both require: 0700 on the directory, 0600 on each private key, 0644 on the rest. The
// guest's own copies get their modes re-applied in-sandbox after `msb copy`, whose mode
// preservation is not a pinned contract (orchestrator/code.go).
func Write(dir string, m *Material) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("sshsession: create %s: %w", dir, err)
	}
	// MkdirAll leaves an already-existing directory's mode alone, and umask applies to the one it
	// creates — so set the mode explicitly rather than trusting either.
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("sshsession: chmod %s: %w", dir, err)
	}
	files := []struct {
		name string
		data string
		mode os.FileMode
	}{
		{ClientKeyFile, string(m.ClientPrivatePEM), 0o600},
		{HostKeyFile, string(m.HostPrivatePEM), 0o600},
		{ClientPubFile, m.ClientAuthorized + "\n", 0o644},
		{HostPubFile, m.HostAuthorized + "\n", 0o644},
		{AuthorizedKeysFile, RenderAuthorizedKeys(m), 0o644},
		{KnownHostsFile, RenderKnownHosts(m), 0o644},
		{SSHDConfigFile, RenderSSHDConfig(m), 0o644},
	}
	for _, f := range files {
		if err := writeFile(filepath.Join(dir, f.name), f.data, f.mode); err != nil {
			return err
		}
	}
	return nil
}

// WriteClientConfig writes <dir>/config, the per-run ssh client config. Separate from Write
// because it needs host facts (the krayt binary's own path, the repo) that the key material knows
// nothing about.
func WriteClientConfig(dir string, c ClientConfig) error {
	for _, p := range []string{c.Dir, c.KraytExe, c.RepoPath} {
		if strings.ContainsAny(p, "\"\\") {
			return fmt.Errorf("sshsession: path %q contains a character ssh_config quoting cannot "+
				"express (\" or \\); move the repo or krayt binary to a path without one", p)
		}
	}
	return writeFile(filepath.Join(dir, ClientConfigFile), RenderSSHConfig(c), 0o644)
}

func writeFile(path, data string, mode os.FileMode) error {
	if err := os.WriteFile(path, []byte(data), mode); err != nil {
		return fmt.Errorf("sshsession: write %s: %w", path, err)
	}
	// WriteFile applies umask to the mode on create, and leaves an existing file's mode alone —
	// neither of which is acceptable for a 0600 private key.
	if err := os.Chmod(path, mode); err != nil {
		return fmt.Errorf("sshsession: chmod %s: %w", path, err)
	}
	return nil
}

// RenderAggregateConfig is the stable <stateDir>/ssh/config (decision 12): one `Include` per live
// session's own per-run config, so the human adds exactly one line to ~/.ssh/config, once, and
// every later `krayt code` session is reachable through it without touching that file again.
// selfPath is this file's own absolute path (named in the comment so the line the human must add
// can be copied straight out of the file); configs are absolute per-run config paths, newest first.
func RenderAggregateConfig(selfPath string, configs []string) string {
	var b strings.Builder
	b.WriteString("# krayt — generated by `krayt code`; do not edit. Regenerated on every session.\n")
	b.WriteString("# Add this one line to ~/.ssh/config (krayt never edits it itself):\n")
	b.WriteString("#     Include " + selfPath + "\n")
	if len(configs) == 0 {
		b.WriteString("# (no live krayt code sessions right now)\n")
		return b.String()
	}
	for _, c := range configs {
		b.WriteString("Include " + c + "\n")
	}
	return b.String()
}

// AggregateConfigPath is the stable path `Include`d from ~/.ssh/config: <stateDir>/ssh/config.
func AggregateConfigPath(stateDir string) string {
	return filepath.Join(stateDir, DirName, ClientConfigFile)
}

// WriteAggregateConfig writes RenderAggregateConfig's output to <stateDir>/ssh/config, creating
// the directory if needed, and returns the path. Everything it writes is inside `.krayt/`
// (decision 12).
func WriteAggregateConfig(stateDir string, configs []string) (string, error) {
	dir := filepath.Join(stateDir, DirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("sshsession: create %s: %w", dir, err)
	}
	path := AggregateConfigPath(stateDir)
	// Write-then-rename, unlike the per-run files: this one path is shared by every concurrent
	// `krayt code` in the repo, and `ssh` may be reading it at any moment. A torn read here would
	// be a broken ssh_config for an unrelated session. (Two sessions racing can still lose an
	// update — each rewrites the list it read — but that self-heals on the next session's refresh,
	// and every session also writes a per-run config that works standalone with `ssh -F`.)
	tmp := path + ".tmp"
	if err := writeFile(tmp, RenderAggregateConfig(path, configs), 0o644); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", fmt.Errorf("sshsession: commit %s: %w", path, err)
	}
	return path, nil
}
