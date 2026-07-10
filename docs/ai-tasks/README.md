# AI tasks

Self-contained task/prompt markdown files written for an AI coding agent — hand one to Claude
directly, or run it in a krayt sandbox with `krayt run --task docs/ai-tasks/<file>.md` (dogfooding).

Each file should be self-contained: enough context that a fresh agent with no prior conversation
can act on it. Name them descriptively in kebab-case after the outcome (e.g.
`build-krayt-dev-image.md`).

| Task | What it does | Status |
|---|---|---|
| [`build-krayt-dev-image.md`](./build-krayt-dev-image.md) | Build the multi-arch `krayt-dev` agent image (Claude Code + the krayt dev toolchain) and its GHCR publish workflow, for dogfooding krayt on krayt. | 🚧 In progress — image/workflow written; first real build/push + dogfood run still pending (`HUMAN_TODO.md`) |
| [`task-prompt-from-stdin.md`](./task-prompt-from-stdin.md) | Add `krayt run --task -` to read the task prompt from stdin (host-side CLI only; no image rebuild). | ✅ Done |

## Security-review remediation (from the pre-release secure code review)

Ordered by severity. The two Criticals should land before any public release. Tasks that need a real
Apple-Silicon Mac to fully verify write the exact test and hand it off via `HUMAN_TODO.md`.

| Task | Severity | Finding it fixes | Status |
|---|---|---|---|
| [`harden-container-oci-spec.md`](./harden-container-oci-spec.md) | Critical + High | Drops container caps (closes the `CAP_SETUID`→proxyd egress bypass), enforces non-root, adds seccomp, opt-in read-only rootfs. Covers findings #1 and #3. | ✅ Done — verified on hardware (`TestContainerHardening`, `TestRootImageFailsClosed`) |
| [`fix-guest-git-config-rce.md`](./fix-guest-git-config-rce.md) | Critical | World-writable `.git` lets the container inject git config the root guest-agent executes; isolate patch generation in a root-only git dir. Finding #2. | ✅ Done — verified on hardware (`TestGuestGitConfigInjectionInert`) |
| [`fix-egress-allowlist-bypass.md`](./fix-egress-allowlist-bypass.md) | Critical | Verifies + locks the egress allowlist against the proxyd-uid bypass; adds the regression test. Finding #1 (depends on the OCI-spec task). | ✅ Done — closed by the OCI-spec fix + `TestEgressRulesetShape` regression guard |
| [`redact-secrets-in-artifacts.md`](./redact-secrets-in-artifacts.md) | Medium | Redaction only covered live logs; extend it to `report.md` + question text and scan the patch (warn, don't corrupt). | ✅ Done — verified on hardware (`TestSecretConfinementInArtifacts`) |
| [`document-single-layer-egress.md`](./document-single-layer-egress.md) | Medium | Docs-only: record that egress is enforced only in-guest (no host backstop). | ✅ Done — §6.6/§10 now state the single-layer model (#39) |
| [`add-proxy-ssrf-guard.md`](./add-proxy-ssrf-guard.md) | Low | Refuse proxy targets that resolve to link-local/metadata (always) or private ranges (unless `full`). | ✅ Done — post-resolution `Control` guard (`checkDialAddr`) on every upstream dial; `TestCheckDialAddr` + `TestGuardBlocksResolvedIP` cover all range/mode combinations |
| [`harden-vfkit-socket-dir.md`](./harden-vfkit-socket-dir.md) | Low | Verify the `/tmp/krayt` socket root is a private, self-owned `0700` dir; fail closed otherwise. | ⬜ Open |
| [`fix-krayt-yaml-tracking.md`](./fix-krayt-yaml-tracking.md) | Low | The tracked `krayt.yaml` falsely claims to be gitignored; make it truly local (or a labeled example). | ✅ Done — kept tracked as the shared dev config (task's Option B): fixed the misleading comment, documented it in §8.1 |
