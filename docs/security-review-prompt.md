# Code review — krayt (pre-release)

## Model gate — read this first, before anything else

This review is intended to be produced by **fable-5 only**. The maintainer is running a separate
review with another model and wants this one to come from fable-5, not from a fallback.

**First, state which model you are.** Then:

- **If you are fable-5:** proceed with the review below. Write your report to
  `docs/security-review-fable5.md` and nowhere else. Start that file with the line
  `Reviewed by: fable-5`.
- **If you are NOT fable-5** (for example, you are an Opus model handling this after a fable-5
  safeguard fallback): **stop — do not perform the review.** Do not analyze the code and do not write
  a report. Write exactly one line to `docs/security-review-fable5.md`:
  `fable-5 did not run this review (fell back to <your model name>); skipped.` — then end your turn.
  If that file already contains a completed fable-5 review, leave it unchanged and simply stop.

---

You are reviewing the `krayt` project for its maintainer before a public release. Read
`KRAYT_SPEC.md` (especially §10) and check that the implementation matches the design. Go through
the components below, review each for correctness and for robust handling of input that comes from
the container, the source repo, or the network, and report anything incorrect, incomplete, or
fragile, with a suggested fix. Where the code and the spec disagree, note the gap.

## Areas to review (priority order)

1. **Outbound network control** — `internal/guest/proxy/`, `cmd/krayt-proxy/`. Confirm the nftables
   rules and the proxy's allowlist logic match the design and cover IPv4, IPv6, and DNS, and that
   the rules are in place before the workload starts.
2. **Secret handling** — `internal/secrets/` and the paths secret values flow through. Confirm they
   stay on tmpfs, are kept out of log output, and don't reach the returned patch or run artifacts.
3. **Repo and patch handling** — `internal/patch/`. Confirm clone and patch generation handle
   unusual file paths, symlinks, and repository metadata safely.
4. **Guest↔host control channel** — `internal/protocol/`, `internal/controlclient/`,
   `internal/guest/service.go`. Confirm the host side validates messages received from the guest.
5. **Container configuration** — `internal/guest/runner/containerd_linux.go`,
   `internal/guest/runner.go`. Review the OCI spec: least-privilege user, capabilities, mounts,
   namespaces, and the `/run/secrets` tmpfs.
6. **VM provider** — `internal/provider/vfkit/`. Check control-socket location and permissions, and
   copy-on-write disk clone/teardown.
7. **Image acquisition** — `internal/imagestore/`, `internal/vmimage/`. Confirm images are pinned by
   digest and verified.
8. **Agent→human questions** — `internal/guest/ask/`, `cmd/krayt-ask/`,
   `internal/orchestrator/questions.go`, `internal/cli/answer.go` / `questions.go`. Confirm question
   text from inside the container is sanitized before it's displayed.
9. **Orchestrator, CLI, adapters** — `internal/orchestrator/`, `internal/cli/`, `cmd/krayt/`,
   `internal/adapter/`. Review run lifecycle, artifact writing, teardown, resource limits
   (`climit.go`), timeouts, and how the agent credential maps to an env var.
10. **Build and CI** — `flake.nix`, `images/flake.nix`, `Dockerfile.test`, `hack/*/Dockerfile`,
    `.github/workflows/`, `secrets.env`, `krayt.yaml`. Flag any secret material committed to the repo
    or over-broad privileges in CI.

## Third-party components

Review only **how krayt configures and calls** containerd (v2.3.2), vfkit (v0.6.3), runc/crun,
nftables, gRPC, oras-go, the MCP go-sdk, and the VM image — whether krayt's own configuration is
sound (least privilege, pinned, verified). Don't review the internals of those tools.

Separately, for the dependencies in `go.mod` and the tools in the VM image, note any **publicly
documented advisories** affecting the **pinned versions**, so the maintainer knows whether to
upgrade. Verify against public advisory databases and mark anything you can't confirm as unverified;
don't invent advisory IDs.

## Ground rules

- **Don't fabricate results.** Some checks can only be confirmed by booting a real VM on Apple
  Silicon (per `CLAUDE.md`, that can't run in this environment). For those, say so and describe the
  exact test a human should run.
- Cite every finding as `path/file.go:line`, and mark it **Confirmed** or **Needs runtime check**.

## Output

Write a concise Markdown report to `docs/security-review-fable5.md`, beginning with the line
`Reviewed by: fable-5`, with these sections:

1. **Summary** — does the implementation match the design? Top concerns in a few bullets.
2. **Findings**, ordered by severity (Critical / High / Medium / Low / Info). For each: title,
   `file:line`, confidence, a short description, impact, and a recommended fix.
3. **Dependency advisory notes** — table of dependency, pinned version, advisory ID, and whether it
   applies to the pinned version (or "unverified").
4. **Residual risks** — revisit the considerations listed in `KRAYT_SPEC.md` §10.
5. **Needs real Apple-Silicon hardware to verify** — each item with the exact command/test.

Be precise and terse. Every claim should trace to code or a cited advisory.
