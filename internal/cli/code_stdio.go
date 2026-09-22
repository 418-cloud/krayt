package cli

// The `krayt code --stdio <run-id>` ProxyCommand path (add-vscode-remote-ssh-session.md decision
// 5) — the entire host→guest transport for `krayt code`, and the reason the feature needs no
// published port and no ingress.
//
// `ssh` runs this as its ProxyCommand and then speaks the SSH protocol over its stdin/stdout. This
// process does one thing: hand those two pipes to `msb exec --stream`, which runs `sshd -i`
// (OpenSSH's inetd mode — serve exactly one connection on stdin/stdout, then exit) inside the
// sandbox. Nothing listens in the guest, no port is published, and the same code works under
// `--net none`.

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/418-cloud/krayt/internal/orchestrator"
	"github.com/418-cloud/krayt/internal/sandbox"
	"github.com/418-cloud/krayt/internal/sshsession"
)

// sshdLogName is where this session's sshd stderr lands, under the run dir's logs/ like every
// other krayt log. Appended to across connections, so a failed handshake can be read after the
// fact next to the run's other artifacts.
const sshdLogName = "sshd.log"

// sshdExecSpec is the one `msb exec` a ProxyCommand issues. Three properties are load-bearing and
// each is asserted by a test:
//
//   - `--stream`, never `--tty` (decision 2): the two are mutually exclusive by msb's own clap
//     config, and this path needs the separated, byte-exact pipes only --stream gives. A pty would
//     mangle the SSH binary protocol with echo and CRLF translation within one round trip.
//   - `--user root` (decision 3): OpenSSH needs root for its privilege separation, to set up the
//     login user's session and environment correctly, and to run the sftp subsystem. krayt already
//     execs as root for `krayt-helper setup` (§7 step 2), so this adds no new privilege — and the
//     LOGIN user is still the sandbox's own unprivileged user, enforced by sshd_config's
//     `AllowUsers` + `PermitRootLogin no`.
//   - Stdout and Stderr are DIFFERENT writers (decision 4). Stdout is the SSH protocol; sshd's own
//     diagnostics (it is invoked with -e, "log to stderr") go to a file. Mixing them corrupts
//     every connection — one stray log line inside the binary stream and the handshake fails.
//     ExecSpec keeps the two separate end to end and msb does too.
func sshdExecSpec(sandboxName string, stdin io.Reader, stdout, stderr io.Writer) sandbox.ExecSpec {
	return sandbox.ExecSpec{
		Name: sandboxName,
		User: "root",
		Command: []string{
			sshsession.GuestSSHDPath,
			"-i", // inetd mode: this connection, on stdin/stdout, then exit
			"-e", // log to stderr, which is a file here — never the SSH byte stream
			"-f", sshsession.GuestSSHDConfigPath,
		},
		Stdin: stdin, Stdout: stdout, Stderr: stderr,
	}
}

// runCodeStdio resolves runID to a live sandbox and pipes this process's stdin/stdout through it
// to `sshd -i`.
//
// On EOF and close ordering: it deliberately does nothing clever. `sandbox.Client.Exec` hands a
// real *os.File straight to the child when stdin is one (which it is here — ssh's own pipe), so
// there is no intermediate copier to lose a final read or to hold the process open after the peer
// hangs up, and Exec does not return until msb has exited and both output streams have drained.
// That is the "linger until the peer closes" discipline probe p1 measured as the only one that hit
// 25/25 round trips on msb 0.6.16; the shapes that dropped 16-20 of 25 were the ones that closed
// or abandoned a stream early. Nothing here closes stdout before Exec returns.
func runCodeStdio(cmd *cobra.Command, repo, runID string) error {
	sd, err := stateDir(repo)
	if err != nil {
		return err
	}
	runDir := orchestrator.RunDir(sd, runID)
	rec, err := orchestrator.ReadRecord(runDir)
	if err != nil {
		return fmt.Errorf("no such run %q under %s: %w", runID, sd, err)
	}
	if rec.EffectiveKind() != orchestrator.KindCode {
		return fmt.Errorf("run %q is not a `krayt code` session (kind=%q)", runID, rec.EffectiveKind())
	}
	if rec.SandboxName == "" {
		return fmt.Errorf("run %q has no recorded sandbox name", runID)
	}
	if rec.Terminal() {
		return fmt.Errorf("code session %q has ended (state=%q) — its sandbox is gone; start a new `krayt code`", runID, rec.State)
	}

	logPath := filepath.Join(runDir, "logs", sshdLogName)
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return err
	}
	logFile, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open %s: %w", logPath, err)
	}
	defer func() { _ = logFile.Close() }()

	sb, err := sandbox.NewClient()
	if err != nil {
		return err
	}
	// cmd.InOrStdin()/OutOrStdout() are os.Stdin/os.Stdout in production — indirected only so a
	// test can assert the stderr/stdout separation without a real terminal. Nothing wraps or
	// buffers them: the SSH stream must stay byte-exact.
	res, err := sb.Exec(cmd.Context(), sshdExecSpec(rec.SandboxName, cmd.InOrStdin(), cmd.OutOrStdout(), logFile))
	if err != nil {
		return fmt.Errorf("krayt code --stdio %s: %w (sshd stderr: %s)", runID, err, logPath)
	}
	if res.ExitCode != 0 {
		// Exit with the child's own status, silently: ssh reads the ProxyCommand's exit status,
		// and sshd has already said whatever it had to say — into logPath, deliberately not into
		// the byte stream ssh is reading.
		return ExitCodeError{Code: res.ExitCode}
	}
	return nil
}

// ExitCodeError asks main to exit with a child process's own status instead of krayt's generic 1,
// and to print nothing extra on the way out. Its one user is the `krayt code --stdio` ProxyCommand
// (code_stdio.go): `ssh` interprets the ProxyCommand's exit status, and any message krayt added on
// stderr there would surface as noise in the middle of an editor's connection attempt — the detail
// is in the run's own logs/sshd.log.
type ExitCodeError struct{ Code int }

func (e ExitCodeError) Error() string { return fmt.Sprintf("exited with status %d", e.Code) }
