# HUMAN_TODO

Single handoff log for steps the coding agent cannot complete itself (credentials, real
hardware, a Linux builder, live secrets). Template per `KRAYT_SPEC.md` §14.

**Entries are deleted once verified, not marked done** (§14, `CLAUDE.md`): record the outcome in the
§14 phase checkbox / `docs/ai-tasks/README.md` / the relevant code comment first, then remove the
entry. This file lists only what is still outstanding; `git log` holds everything that was here.

---

## Status

Everything closed has been **deleted from this file** rather than kept as ✅ entries — the full
record of what was verified, and how, lives in `git log` (this file's history through #115),
`KRAYT_SPEC.md` §14's phase checklists, and `docs/ai-tasks/README.md`'s status table. What is left:

1. **`krayt-agent-opencode`** — the one image never published or exercised. Its entry below covers
   the publish check, an onboarding run, the `ask_human` question round-trip, and the
   `NODE_EXTRA_CA_CERTS` check re-homed from §14 Phase 9.
2. **`krayt-agent-gemini-cli`'s question channel** — the one clause of that image's verification
   still unrun; publish and onboarding are confirmed by real runs.
3. **Two `hadolint` passes** — gemini-cli and opencode Dockerfiles.
4. **A known defect** in the agent images' `/output/report.md` contract (`[BUG]` below), which will
   corrupt the opencode verification the same way it corrupted a gemini one unless the task writes
   somewhere else.
5. **One live-run security check** — that the egress-proxy child's real environment on macOS carries
   no operator credentials (`[security]` below, report §6 item 4).

The host-side-proxy arc (all three steps) is **done and verified on hardware**: the egress proxy
runs host-side over vsock, terminates TLS for allowlisted hosts, and attaches the real credential
itself — a subscription token now never enters the VM (`run_df97fffa`, control `run_10fc027d`), and
Node trusts the ephemeral CA through `NODE_EXTRA_CA_CERTS` (`run_c74208b4`, with `proxy.log`
corroborating the negative control independently of the agent's own report).

---

## [security] confirm the live egress-proxy child carries no operator credentials
- Needed: on the Mac, **during a run**, check the real child process's environment. This is item 4
  of `docs/security-review-host-proxy-report.md` §6 — the live-run half of the F6 fix that gave the
  child an explicit, minimal environment (`egressProxyChildEnvKeys`,
  `internal/orchestrator/egressproxy.go`).
- Why the agent can't: needs a live `krayt run` on real hardware; `ps -E` is macOS-only, and the
  offline tests can only assert what this process constructs, not what a Mac kernel shows.
- Exact steps/commands:
  ```sh
  # on the Mac, during a run:
  pgrep -f '__egress-proxy' | head -1 | xargs -I{} ps -E -p {} | tr ' ' '\n' | grep -E 'KEY|TOKEN|SECRET|AWS|PASS'
  ```
  Worth running from a shell that has `ANTHROPIC_API_KEY` (or any such variable) exported, so the
  check can actually fail if the fix regressed.
- Verify success by: **no matches.** The child's full environment should be only `PATH`, `HOME`,
  and whichever of `SSL_CERT_FILE`/`SSL_CERT_DIR`/`KRAYT_PROXY_LOG_REQUESTS`/
  `KRAYT_PROXY_LOG_HEADER_VALUES` were set on the `krayt run` invocation. Drop the `grep` to see
  the whole list.
- Blocking: no — the offline half (`TestSpawnEgressProxySecretNeverInArgvEnvOrOutput`,
  `TestSpawnEgressProxyForwardsLogRequestsEnv`) already guards the code path in CI.

---

## [BUG] the agent images tee stdout over `/output/report.md`, so an agent that writes it corrupts it

Each entrypoint runs `<agent> … | tee "$OUTPUT_DIR/report.md"`, so `tee` owns that file for the
whole run. `KRAYT_SPEC.md` §8.2 nevertheless documents the agent writing `/output/report.md` — and
when a task instructs it to, the two writers interleave. Observed in `run_bd851ac2`: the agent's
final stdout line was spliced mid-sentence into its own report (`…but npm immed` + the summary line
+ `n under 200 milliseconds.`), and the agent burned several turns "fixing corruption" and ran
`ps -ef` hunting for a nonexistent background daemon.

- **Workaround that works today** (proven in `run_c74208b4`): have the task write its evidence to a
  DIFFERENT file under `/output` — krayt collects the whole directory, so `/output/<name>-report.md`
  comes back intact — and leave `/output/report.md` to `tee`.
- **The fix:** either stop teeing over a file the agent may write (tee to a temp path, move it into
  place only if the agent wrote nothing), or amend §8.2 to say the agent must NOT write
  `/output/report.md` in these images and name the file it should use instead. Same pattern in all
  three entrypoints.
- **Why a human is needed:** the code change is small and agent-doable, but confirming it needs an
  image rebuild plus a real run with an agent that writes a report.
- **Blocking:** no — but do the workaround before the opencode run below, or its evidence will be
  corrupted the same way.

---

## [tooling] `krayt-agent-gemini-cli` — the `--on-question=wait` round-trip

Publish and onboarding are **confirmed** (the image pulls anonymously from GHCR and has completed
several real runs, most recently `run_c74208b4`). What has never run is the question channel.

- **Needed:** one `--on-question=wait` run against `ghcr.io/418-cloud/krayt-agent-gemini-cli`, with a
  task prompt that instructs the agent to ask a clarifying question before proceeding. This is the
  one piece of MCP wiring that cannot be verified without a real `gemini` process talking to a real
  socket — the `ask_human` server registered via the entrypoint's runtime `~/.gemini/settings.json`
  rewrite.
- **Why the agent can't:** no live Gemini API key here, and no way to drive a real round-trip.
- **Verify success by:** `krayt ls` shows the run `waiting`; `krayt questions <run-id>` lists the
  real question; `krayt answer` resumes it to completion with `EXIT 0`.
- **Blocking:** no.

---

## [tooling] Publish and verify `krayt-agent-opencode`

The only agent image never published or exercised. **The publish itself may already have happened:**
`agent-images.yml` builds ALL matrix images on every trigger and pushes on `main`, and #115 touched
`images/agents/**` — so check before assuming it needs a run.

- **Needed:**
  1. **Confirm the publish.** `docker buildx imagetools inspect
     ghcr.io/418-cloud/krayt-agent-opencode:latest` shows `linux/amd64,linux/arm64`; the tags
     `:latest`, `:sha-<short>`, `:1.18.16` exist; the GHCR package page loads without auth. Also
     confirm the build-time models.dev catalog snapshot actually landed at
     `/home/agent/.cache/opencode/models.json` and isn't empty or stale.
  2. **A live onboarding run** as the image README's quickstart shows, with one real provider key:
     ```sh
     krayt run --image ghcr.io/418-cloud/krayt-agent-opencode --agent opencode \
       --task ./task.md --repo . --secrets ./secrets.env --allow api.anthropic.com
     ```
     This doubles as an egress check: with only `api.anthropic.com` allowlisted, opencode must NOT
     need `models.opencode.ai` (the baked-in snapshot + `OPENCODE_DISABLE_MODELS_FETCH=true` should
     make that unnecessary). A run that stalls or fails on a blocked catalog fetch means that
     mitigation does not hold and needs revisiting.
  3. **One `--on-question=wait` run**, confirming the `ask_human` MCP server — registered via the
     entrypoint's runtime `OPENCODE_CONFIG` file — surfaces a real question to `krayt questions` /
     `krayt answer` and resumes. First exercise of this image's MCP config shape
     (`type: "local"`, array `command`) against a live binary rather than against docs.
  4. **The `NODE_EXTRA_CA_CERTS` check** (§14 Phase 9's last clause, re-homed here because it is
     gated on this image existing, not on any MITM code). Not optional: opencode is node-based and
     Node reads no system trust store, so if this image's CA plumbing is wrong then **every** TLS
     call from it fails whenever `network.mitm` is on.

     Run it exactly as gemini-cli was verified in `run_c74208b4` — `mitm: true`,
     `registry.npmjs.org` allowlisted, task writing to `/output/opencode-report.md` (**not**
     `report.md`, per the `[BUG]` above) — with all four pieces:
     - a real `npm install` through the proxy that **succeeds**, with `npm config get strict-ssl`
       confirmed `true` or the check proves nothing;
     - **the negative control**: the same install with only `NODE_EXTRA_CA_CERTS` removed, which
       must **fail** `SELF_SIGNED_CERT_IN_CHAIN`. Use a throwaway cache dir
       (`--cache /tmp/empty-cache`) rather than `npm cache clean --force` — cleaner, and it is the
       cache that produces a false pass: a cached package installs with no network request at all,
       which fooled the first gemini attempt in both runs before the agent caught it;
     - `openssl s_client -proxy 127.0.0.1:3128` against the registry, confirming the issuer is
       `krayt ephemeral MITM CA (run_…)`;
     - **`proxy.log` as independent corroboration** — the failing arm shows up host-side as
       `MITM registry.npmjs.org:443: TLS handshake failed: EOF`, which does not depend on anything
       the agent wrote. This is the strongest single piece of evidence; check it.

     Check first whether opencode has a headless-refusal gate like gemini-cli's folder-trust one —
     same class of problem, different CLI.
- **Why the agent can't:** no `docker build`/push access, no live provider key, and no way to drive
  a real `--on-question=wait` round-trip here.
- **Verify success by:** all four items above, with the artifacts (not the agent's prose) as the
  evidence: `krayt ls` reaching `done`/`EXIT 0`, `changes.patch` applying cleanly, the collected
  `/output/opencode-report.md`, and `proxy.log`.
- **Blocking:** no.

---

## [tooling] `hadolint` the gemini-cli Dockerfile
- Needed: `images/agents/gemini-cli/Dockerfile` was hand-reviewed against common hadolint rules
  (pinned `USER` before `ENTRYPOINT`, JSON-form `ENTRYPOINT`, apt lists cleaned up, no `latest`
  tag) but never actually run through `hadolint` — the binary isn't installed in this
  environment and the sandbox's egress proxy blocks the GitHub release-asset host it ships from
  (`release-assets.githubusercontent.com` → `403`), so it couldn't be fetched either.
- Why the agent can't: no package manager access or reachable download host for the `hadolint`
  binary in this sandbox.
- Exact steps/commands: `hadolint images/agents/gemini-cli/Dockerfile` (or
  `docker run --rm -i hadolint/hadolint < images/agents/gemini-cli/Dockerfile`).
- Verify success by: no unexpected findings (the image intentionally mirrors
  `images/agents/claude-code/Dockerfile`'s already-passing shape).
- Blocking: no — this is a lint pass, not a build/runtime correctness issue.

---

## [tooling] `hadolint` the opencode Dockerfile
- Needed: `images/agents/opencode/Dockerfile` was hand-reviewed against common hadolint rules
  (pinned `USER` before `ENTRYPOINT`, JSON-form `ENTRYPOINT`, apt lists cleaned up, no `latest`
  tag, digest-pinned `FROM`) but never actually run through `hadolint` — same tooling gap as the
  gemini-cli entry above (binary not installed, release-asset host unreachable from this sandbox).
- Why the agent can't: no package manager access or reachable download host for the `hadolint`
  binary in this sandbox.
- Exact steps/commands: `hadolint images/agents/opencode/Dockerfile` (or
  `docker run --rm -i hadolint/hadolint < images/agents/opencode/Dockerfile`).
- Verify success by: no unexpected findings (the image intentionally mirrors
  `images/agents/claude-code/Dockerfile`'s already-passing shape, modulo the `TARGETARCH` case
  statement it shares with `hack/krayt-dev/Dockerfile`'s already-reviewed pattern instead).
- Blocking: no — this is a lint pass, not a build/runtime correctness issue.
