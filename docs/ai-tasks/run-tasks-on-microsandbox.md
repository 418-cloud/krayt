# Task: run tasks on microsandbox — the cut-over, and the deletion of krayt's own sandbox layer

**Read `CLAUDE.md`, the whole of `docs/adr-microsandbox-sandbox-layer.md`, and `KRAYT_SPEC.md` §3,
§5, §6.2–§6.6, §6.10–§6.12, §7, §10, §14 first.** Give a short plan (the new `Run` spine, which test
files are rewritten against which seam, the deletion list) and proceed.

**This is the big one.** It is the only task in the arc that changes behaviour, and its diff is
mostly deletion — roughly 12,000 lines removed against a few hundred added, because
`add-msb-sandbox-driver.md`, `translate-network-policy-to-msb.md`, `hand-secrets-to-msb.md`,
`add-krayt-guest-helper.md` and `dial-ask-channel-over-vsock.md` have already built every piece and
left it unwired. **Do those five first.** If any is missing, stop and say so rather than
re-implementing it here.

**Blocked on `probe-microsandbox-feasibility.md` P1 and P2.** P1 decides whether the ask channel
works at all; P2 decides `--security restricted` versus the helper's privilege separation.

## What changes

`orchestrator.Run` stops provisioning a VM and starts renting one. The lifecycle, from the ADR:

1. **create** — `msb create` with the image, one `--secret NAME@host` per credential (values in
   `cmd.Env`, never argv), the full explicit network policy, `--tls-intercept` when any secret is
   declared, `--vsock` for `ask_human`, `--user agent`, resources, and `--max-duration`.
   Everything policy-shaped is create-time: `--secret` and `--vsock` are create flags, and msb
   requires a restart to add a secret to a running sandbox. The env must be set on the invocation
   that *starts* the sandbox — msb reads the value at start time, not config-load time.
2. **copy in** — the git bundle, `/task/prompt.md`, `krayt-helper` and `krayt-ask`.
3. **exec (helper, as root)** — `krayt-helper setup`: clone from the bundle, tag `krayt-baseline`,
   snapshot the root-only patch-git, relax the workspace for the agent user.
4. **exec (agent, as `agent`)** — the adapter's command, `--stream`, logs to the run's log sink;
   `krayt-ask` rides vsock back to the host.
5. **exec (helper, as root)** — `krayt-helper finish`: diff against the baseline, assemble `/output`.
6. **copy out** — `/output/*`, including the agent's own `report.md` if it wrote one.
7. **host** — scan the collected patch for secret values (host-side now), render `report.md` and
   write `meta.json`. Neither is an exec.
8. **stop + rm** — always, on every path, under `context.WithoutCancel`.

Everything above step 7 that reads "host" is unchanged code: the bundle/baseline/patch model (§6.7),
the report and meta schemas (§8.4), `ask_human` (§6.13), the adapters and their exactly-one
credential rule (§6.14), concurrency and run records (§6.2), `krayt apply`. That is the part with
the most design in it and msb provides none of it. Do not touch it beyond rewiring its inputs.

## Decisions already made (do not re-litigate)

1. **Option B1.** Not B2 — `internal/proxy` goes. Not C — there is no `Sandbox` seam with two
   implementations and no way to select the old backends. The vfkit and Firecracker providers are
   deleted in this change, not deprecated behind a flag.
2. **Delete in this change** (nothing that executes may survive in two implementations):
   `internal/provider` (2,983), `internal/guest` (2,506), `internal/protocol` (1,841),
   `internal/proxy` (4,030), `internal/controlclient` (213), `internal/imagestore` (411),
   `cmd/krayt-agent` (73), `cmd/krayt-vsock-forward` (368), `internal/orchestrator/egressproxy.go`,
   `internal/cli/egressproxy.go`, `internal/adapter/anthropic_wire.go` and its golden tests, and the
   `internal/cli/run_{darwin,linux,other}.go` split — which collapses to one OS-agnostic file,
   because `newRunDeps` no longer has an OS-specific provider to choose.
   Also delete `hack/linux-net-setup.sh` and the `krayt doctor` checks for vfkit, firecracker and
   `/dev/kvm`, and drop the `docker FORWARD`-policy check that existed only because of the
   Firecracker tap setup.
   `make proto`, the `protoc`/`buf` dev-shell pins in `flake.nix`, and §9.2's codegen section go
   with `internal/protocol`.
3. **`internal/vmimage`, `images/`, the image CI workflows and `krayt image` stay for one more
   task.** They build and publish an artifact nothing consumes any more, which is dead weight but
   not a second execution path — `retire-vm-image-pipeline.md` removes them. Keeping them here is
   what makes this diff reviewable. Say plainly in your report that they are knowingly left.
4. **The test seam changes from `fakeProvider` to the fake `msb` binary.** `KRAYT_SPEC.md` §14's
   test strategy is explicit that the `Provider` interface is what makes the core testable without a
   VM; that sentence is now false and must be rewritten, not quietly outlived. The replacement is
   the scriptable fake `msb` from `add-msb-sandbox-driver.md`: the orchestrator's unit tests drive a
   real `exec.Cmd` against a re-exec'd test binary that plays msb. Rewrite
   `internal/orchestrator/{orchestrator,manager,report,question,secret_artifacts}_test.go` against
   it. **Coverage must not shrink**: every behaviour those tests assert today — teardown on every
   path, timeout classification, artifact collection, question round-trip, redaction, run-record
   state transitions — must still be asserted. If a behaviour genuinely no longer exists, delete the
   test *and say which and why* in your report; do not let it evaporate.
5. **Resource and container policy mapping.** `resources.cpus` → `--cpus`, `resources.memory` →
   `--memory`, `resources.disk` → `--root-disk`, `resources.timeout` → `--max-duration` **and**
   krayt's own `context.WithTimeout` (belt and braces: the ctx is what makes teardown deterministic,
   `--max-duration` is what stops a wedged guest outliving it).
   `container.add_capabilities`, `container.seccomp` and `container.readonly_rootfs` have no msb
   equivalent — msb offers `--security default|restricted` and nothing finer. They become
   **removed keys that hard-error**, naming `--security` as the replacement, on the same reasoning
   as `network.mitm`: a config that sets them is a config reasoning about hardening, and silently
   dropping it is a posture regression. Amend §6.10 and `harden-container-oci-spec.md`'s status row
   to record that its OCI-spec knobs are superseded by msb's profile.
6. **Exit-code disambiguation must be structural, not a guess.** `msb exec` propagates the guest's
   code via `std::process::exit` (`crates/cli/lib/commands/exec.rs:125-127`) while msb's own failures
   also exit non-zero, so exit `1` is ambiguous. Use the `ErrMsbFailed` distinction
   `add-msb-sandbox-driver.md` defines: a non-zero exit with no terminal output event observed is a
   driver failure, and must surface as a failed run (`StateFailed` with a real message), never as
   "the agent exited 1". A run that reports the agent's exit code when msb never started the agent
   is a defect.
7. **Boot and system diagnostics replace the console log.** `writeConsoleLog` copied the guest's
   serial console into the run's logs dir before teardown, deliberately registered so it survived a
   failure at any later step. Keep that property with `msb logs --source system --json` written to
   the same place, captured in a defer ordered before `rm`. When a sandbox fails to start, `msb logs`
   prepends a reconstructed error block from `boot-error.json` — that is the new equivalent of a
   boot failure's console output and is worth capturing for exactly the same reason.
8. **Sandbox naming and concurrency.** One sandbox per run, named from the run id
   (`krayt-<run-id>`), plus `--label` entries for attribution. `orchestrator.AcquireSlot`'s flock
   concurrency limit is unchanged and still correct — it bounds krayt's runs, not msb's sandboxes.
   Reap orphans defensively: a `krayt-` sandbox whose run record is terminal should be removable by
   `krayt` rather than requiring the user to learn `msb rm`.
9. **§2's identity claim survives and should be restated, not dropped.** It is still a full VM
   boundary per task, still not shared-kernel sandboxing — libkrun rather than
   Virtualization.framework or Firecracker. What changes is who builds the boundary, and §3.1's
   "krayt builds its own sandbox" statements are what have to go.

## Spec amendments (required — `CLAUDE.md`: the spec wins until amended)

B1 contradicts §3.1, §6.3–§6.6, §6.10–§6.12 and §11 outright, and overturns §6.6.1's stdin rule.
Rewrite, do not diverge:

- **§3.1 / §4** — krayt consumes a sandbox rather than building one. The `Provider` decision is
  superseded; record it as superseded with the ADR's date and name, in the style §15 already uses.
- **§5** — the architecture diagram loses the guest agent, the control protocol, the in-VM egress
  proxy and the image pipeline, and gains the msb process.
- **§6.3–§6.5, §6.10–§6.12** — deleted sections. Leave a one-line stub pointing at the ADR where a
  reader would otherwise wonder whether the section was lost by accident.
- **§6.6 / §6.6.1** — rewritten around msb's policy engine and host-side substitution, carrying
  forward the findings the earlier tasks recorded: the never-empty-policy rule, the explicit
  `--tls-intercept`, the DNS change, and the un-stripped-header regression in §10.
- **§7** — the run lifecycle above, replacing the current step list.
- **§9.1/§9.2** — drop the gRPC/protobuf/vsock-server dependencies that no longer have a consumer,
  and the codegen section.
- **§14** — Phase 11 (microsandbox migration), with its own `Done when (hardware)` checkboxes, in
  the shape Phases 8–10 use. Phases 1, 7, 8 and 9's `Done when` criteria describe hardware that no
  longer exists in the design; annotate them as historical rather than deleting the record.
- **§12** — macOS specifics: vfkit is no longer a prerequisite; msb is.
- **§15** — the msb-as-`Provider` rejection recorded there was answering a narrower question. Add
  the ADR's resolution beneath it so a reader does not conclude msb was rejected outright.

Also update `README.md` (install now needs `msb`, not `vfkit`/Firecracker; the "Agent images"
quickstart is unchanged), `SECURITY.md` if it names the boundary, and `docs/ai-tasks/README.md`'s
status rows for every task whose subject this deletes — `move-egress-proxy-to-host.md`,
`add-tls-mitm-credential-injection.md`, `inject-claude-oauth-token-at-proxy.md`,
`harden-container-oci-spec.md`, `fix-guest-git-config-rce.md`, `fix-egress-allowlist-bypass.md`,
`add-proxy-ssrf-guard.md`, `harden-vfkit-socket-dir.md`, `document-single-layer-egress.md`,
`automate-vmimage-releases.md`, `compress-vmimage-rootfs.md`. Their Status column is a durable
record; say that the code they describe was retired by this arc and which property replaced it,
rather than leaving rows that point at deleted files.

## Done when

- `go build ./...` on darwin **and** linux, `go test -race ./...`, and `golangci-lint run` are green.
- `grep -r "internal/provider\|internal/guest\|internal/protocol\|internal/proxy\|internal/controlclient" --include=*.go .` returns nothing.
- No `//go:build darwin` or `//go:build linux` file remains outside `cmd/krayt-helper`,
  `cmd/krayt-ask`'s vsock dialer, and `internal/cli/resources_*.go` — the OS-specific seam is gone
  because the OS-specific work is msb's now.
- The orchestrator suite passes against the fake `msb` with no coverage regression (decision 4).
- A test asserts teardown (`stop` + `rm`) runs on **every** exit path: success, agent failure, msb
  failure, wall-clock timeout, and `ctx` cancellation.
- A test asserts no secret value appears in any argv on any invocation of the whole lifecycle.
- `krayt doctor` on a host with `msb` installed reports a healthy environment with no vfkit or
  firecracker rows.
- **Hardware (`HUMAN_TODO.md`, blocking for `retire-vm-image-pipeline.md`)**: a real end-to-end
  `krayt run` on an Apple-Silicon Mac against `ghcr.io/418-cloud/krayt-agent-claude-code`, producing
  a non-empty `changes.patch`, a rendered `report.md`, and a `meta.json` — plus one
  `--on-question=wait` run exercising the `ask_human` round trip. Write the exact commands; do not
  claim the result.

## Out of scope

- `internal/vmimage`, `images/`, the image CI workflows, `krayt image` — the next task.
- linux/arm64, Windows, `sandbox.extra_conf`, snapshots — later tasks, each dependent on this one.
- Supplying krayt's own interception CA (deliberately not in this arc).
- Any performance work. Make it correct and deletable-from; `warm-start-msb-sandboxes.md` makes it fast.
