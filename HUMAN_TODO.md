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
3. **A known defect** in the agent images' `/output/report.md` contract (`[BUG]` below), which will
   corrupt the opencode verification the same way it corrupted a gemini one unless the task writes
   somewhere else.
4. **One live-run security check** — that the egress-proxy child's real environment on macOS carries
   no operator credentials (`[security]` below, report §6 item 4).
5. **`krayt-dev`'s floating base pin** — the rebuild, the repin, and the injected-run verification
   are all done (`krayt.yaml` runs `sha-cbca700`, built from `main`'s tip). What's left is that
   `hack/krayt-dev/Dockerfile`'s `FROM` is still tag-only, so the base floats, plus two
   CA-sensitive checks nothing has exercised yet (`[tooling]` below).
6. **The trixie base bump + rtk install** — landed and verified end-to-end **for Claude Code on
   arm64**: `rtk 0.45.0` runs in the published image, and two real `krayt-dev` runs
   (`run_9e0a56de` on, `run_378dac2d` with `KRAYT_RTK=off`) prove the hook intercepts live tool
   calls and that the opt-out is honoured rather than silently absent. What remains needs live
   Gemini/OpenCode credentials — the same proof for those two agents — plus the amd64/other-image
   manifest check. See the `[tooling]` entry below.

(The two `hadolint`-the-{gemini-cli,opencode}-Dockerfile entries formerly here are resolved: this
task's own Verify step ran `hadolint` against both — clean, same pre-existing warnings as
`claude-code`'s already-passing shape — see `docs/ai-tasks/README.md`'s `add-rtk-to-agent-images.md`
row.)

The host-side-proxy arc (all three steps) is **done and verified on hardware**: the egress proxy
runs host-side over vsock, terminates TLS for allowlisted hosts, and attaches the real credential
itself — a subscription token now never enters the VM (`run_df97fffa`, control `run_10fc027d`), and
Node trusts the ephemeral CA through `NODE_EXTRA_CA_CERTS` (`run_c74208b4`, with `proxy.log`
corroborating the negative control independently of the agent's own report).

---

## [tooling] pin `krayt-dev`'s `FROM` to a base digest, and close two CA-sensitive checks

The rebuild and repin this entry was opened for are **done**: `krayt-dev` is rebased onto
`ghcr.io/418-cloud/krayt-agent-claude-code:2.1.226` with no entrypoint of its own, it builds and
publishes from `main`, and `krayt.yaml` pins `ghcr.io/418-cloud/krayt-dev:sha-cbca700` — built from
`cbca700`, `main`'s tip (#138). Real runs through the injected `network.mitm: true` path succeed
(`run_9e0a56de`, `run_378dac2d`), and their `console.log` carries the discriminating evidence:
every line `[claude-code]`-prefixed with no `[krayt-dev]` line (so the new image is what ran) and
no gh line at all, plus `authenticated via CLAUDE_CODE_OAUTH_TOKEN`, `trusting krayt's ephemeral
MITM CA (network.mitm enabled)`, and `running claude -p in /workspace (model: claude-sonnet-5,
effort: high)`. Neither credential reaches the container. The `gh api` half is confirmed too — the
`fix-pr-review-comments` task has run several times against a live fine-grained PAT through the
`api.github.com` inject rule, so the `Bearer ` prefix and header name are right in practice, not
just per GitHub's docs. `KRAYT_RTK=off` inside a real krayt-dev container is likewise confirmed
(`run_378dac2d`).

- **Needed:**
  1. **Get the base digest onto the `FROM` line.** `hack/krayt-dev/Dockerfile:28` is still
     `ghcr.io/418-cloud/krayt-agent-claude-code:2.1.226` — tag-only. That was deliberate at first
     (an invented digest is worse than an absent one), but **it must not stay that way**:
     `agent-images.yml` re-points `:2.1.226` on every build off `main`, so an unpinned `FROM`
     floats — krayt-dev absorbs base changes silently on any unrelated rebuild, and the Renovate
     digest PR that is the intended delivery path for a base change (see `dev-image.yml`'s `paths`
     comment) never gets opened. Renovate should do it (`pinDigests: true` covers the dockerfile
     manager); confirm its PR lands, or add the digest by hand.
  2. **The two CA-bundle-sensitive checks, in one run.** No run so far has exercised the Go
     toolchain inside the container, which is what would catch a wrong concatenated CA bundle:
     the toolchain talks to `passthrough` hosts whose real upstream chain must still verify while
     `SSL_CERT_FILE` points at krayt's rewritten bundle. One run with a task that does
     `go build ./... && go test ./...` **and** `go test -race ./...` (the latter also being the
     check that `gcc`/`libc6-dev` are actually present on the slim base) closes both. Run it as
     `KRAYT_PROXY_LOG_REQUESTS=1 krayt run --config krayt.yaml --task <file>` so `proxy.log`
     records `inject=true` on `api.anthropic.com`/`api.github.com` and only a `CONNECT` line per
     `passthrough` host — the one remaining observation from the original checklist.
- **Why the agent can't:** no `docker build`/push access, no live subscription token or PAT, and no
  Apple-Silicon Mac to boot a VM on. The offline half is done and passing:
  `hack/test-entrypoint-credentials.sh` (run by `ci.yml`) covers the shared entrypoint's
  credential paths plus the branches krayt-dev relies on — model/effort, and that no entrypoint
  touches GH_TOKEN at all (tests 12–15) — and `TestApplyConfigDogfoodsThisRepo` pins the config.
- **Blocking:** no longer — dogfooding through `krayt.yaml` works today. The floating `FROM` is a
  reproducibility risk, not an outage.

---

## [tooling] publish the trixie-based agent images and verify rtk end-to-end

Base-OS bump (all Debian/Node bases in the repo moved from bookworm to trixie) + rtk (Rust Token
Killer) installed and wired into all three published agent images
(`docs/ai-tasks/add-rtk-to-agent-images.md`). Everything checkable without Docker, a registry, or
a live agent credential is done: `hadolint` is clean on all five changed Dockerfiles, `go build
./...`/`go test ./...` are unaffected, the offline entrypoint-credential suite
(`hack/test-entrypoint-credentials.sh`) is green, `renovate.json` parses, and the gemini-cli
settings.json merge fix is verified offline against a fixture that already has rtk's
`hooks.BeforeTool` entry present (the merge retains both that key and the new `mcpServers` key —
decision 5's own repro case, simulated since this sandbox has neither `node` nor `python3` to run
the actual snippet or the task's own `python3 -c "import json; …"` check; a Go program produced
the same JSON-parse-and-assert instead).

**One correction to the task's own Background**, found by reading rtk's real v0.45.0 source
(pulled via `codeload.github.com`, since `release-assets.githubusercontent.com` — where the
compiled release tarballs actually live — returns `403` from this sandbox's egress proxy, so the
binary itself could never be fetched or run here): the task states "every agent integration … is
a thin delegate that shells out to `rtk rewrite`." That's only true for OpenCode's plugin
(`hooks/opencode/rtk.ts`). Claude Code's and Gemini CLI's hooks are native in-process handlers
(`rtk hook claude` / `rtk hook gemini`, registered directly as the `PreToolUse`/`BeforeTool`
command in `src/hooks/constants.rs`/`src/hooks/init.rs`) that replaced the older
shells-out-to-`rtk rewrite` shape. A wrapper that only intercepted `rtk rewrite` would leave
`KRAYT_RTK=off` a no-op for Claude Code and Gemini specifically — silently, since both would keep
resolving `rtk hook <agent>` straight through to the real binary. The `images/agents/*/rtk`
wrapper in this change intercepts all three registered shapes (`rewrite`, `hook claude`, `hook
gemini`), each mimicking rtk's own "no rewrite" output for that integration (confirmed against
`hook_cmd.rs`) — this is implemented, offline-tested (a fake binary + all three
`KRAYT_RTK=off`/`on` combinations), and **since verified against the real binary in a real run**
for the Claude Code shape (see "Already verified" below). The same source read also settled two
things the task asked to verify rather
than assume: `rtk init --auto-patch`'s default Claude path does **not** write a
`~/.claude/hooks/rtk-rewrite.sh` script (so no `jq` dependency is needed in any image), and
Gemini's settings.json patch needs `--auto-patch` too, not just Claude's (its default `Ask` mode
reads stdin, which a non-interactive `docker build` can't answer — confirmed in
`src/hooks/init.rs`).
- **Already verified — do not redo:** `rtk 0.45.0` runs inside the published arm64
  `krayt-agent-claude-code` (`podman run --rm --platform linux/arm64 --entrypoint rtk
  ghcr.io/418-cloud/krayt-agent-claude-code:latest --version`), which settles the glibc ≥ 2.39
  premise the whole trixie bump exists for, proves that image published on arm64, and proves
  Claude Code's native installer ran on trixie. It also supersedes the `objdump -p` check this
  entry used to carry: that was only ever a *predictor* of whether the binary would start on this
  glibc, and it demonstrably starts. Renovate pinned the four trixie `FROM` digests in #137.
- **Also verified — the whole Claude Code path, on arm64, in real runs.** Two `krayt-dev` runs of
  `docs/common-tasks/verify-rtk-integration.md` against `krayt-dev:sha-cbca700`, positive and
  negative control, both `exit 0`:
  - `run_9e0a56de` (rewriting on): the `PreToolUse` entry is a bare `rtk hook claude`, so it
    resolves through the `/usr/local/bin/rtk` wrapper rather than around it; all five wrapper
    contract rows matched against the **real** binary, including the two over-broad-wrapper traps;
    and three plain, unprefixed Bash calls moved `rtk gain`'s independent counters by exactly +3
    total / +1 on each matching row. That is the hook demonstrably intercepting live tool calls.
  - `run_378dac2d` (`KRAYT_RTK=off` for the run): the hook still fired on every Bash call
    (`rtk session`'s Cmds counter grew 40→52) while contributing **zero** rewrites (`rtk gain`
    flat at 9, `history.db` mtime unchanged), and plain commands came back raw. The discriminator
    matters: bare "output looks unrewritten" cannot tell an honoured opt-out apart from a hook
    that never ran.
  - These also settle "Claude Code still *runs* on trixie" — a 5m47s and a 3m50s session,
    `claude 2.1.226`, on the trixie chain. Only arm64; amd64 rolls into item 1.
- **Needed:**
  1. **The other two images, and the amd64 side.** Only arm64 has been pulled and run, and only
     `krayt-agent-claude-code` (via `krayt-dev`). Confirm the rest actually moved rather than
     assuming a green `agent-images.yml` run means it happened: `docker buildx imagetools inspect
     ghcr.io/418-cloud/krayt-agent-{gemini-cli,opencode}:latest` (or `podman manifest inspect`)
     showing `linux/amd64,linux/arm64`, plus one `claude --version` on an amd64 pull.
  2. **Rewriting actually happens in a real run — for gemini-cli and opencode.** Claude Code is
     done (above); these two have never been exercised against a live binary, only reasoned about
     from source, and both need live Gemini/OpenCode credentials. Same shape as the Claude runs: a
     run log showing an `rtk`-prefixed command executing, plus the `KRAYT_RTK=off` negative
     control. `docs/common-tasks/verify-rtk-integration.md` is Claude-specific as written (it
     reads `~/.claude/settings.json` and `rtk hook claude`) — adapt it per agent rather than
     running it as-is.
  3. **The gemini-cli `--on-question=wait` round-trip AND the settings.json merge, together.**
     This task's fix means a questions-enabled run should now keep BOTH the `ask_human`
     `mcpServers` entry and rtk's `BeforeTool` hook — confirm the real, built image's
     `~/.gemini/settings.json` has both after a `--on-question=wait` run (this doubles as the
     still-outstanding gemini-cli question-channel check from this file's other `[tooling]`
     entry — do them together rather than as two separate runs).
  4. **The OpenCode plugin loads without network access.**
     `~/.config/opencode/plugins/rtk.ts` type-imports `@opencode-ai/plugin` (erased at parse time
     by a TS-aware runtime, confirmed by reading the file — it's a genuine `import type`, not a
     value import) and otherwise uses only the plugin host's injected `$` — so it *should* need no
     `npm install`. But if opencode resolves plugin dependencies at load time regardless, the
     run's allowlist blocks the npm registry and this would need to be pre-baked into the image.
     Unproven either way — this is the same open question opencode's own outstanding
     `[tooling]` entry below already carries; resolve both together.
- **Why the agent can't:** no `docker build`/push access; no live Anthropic/Gemini/OpenCode
  credential; and this sandbox's own egress proxy allowlists only `github.com`/`api.github.com`/
  `codeload.github.com`/`api.anthropic.com` — `registry-1.docker.io`, `ghcr.io`, and
  `release-assets.githubusercontent.com` (where rtk's actual compiled binaries live) all return a
  `403` on the CONNECT tunnel. `rtk`'s *source* was reachable (via `codeload.github.com`, a GitHub
  source-archive host, not a release-asset host) and is what grounded the corrections above; the
  compiled binary itself was not.
- **Verify success by:** all four items above, with real command output / a real build log / a
  real `krayt ls` → `done` as the evidence — not the agent's prose.
- **Blocking:** no — the offline half (hadolint, `go build`/`go test`, the entrypoint-credential
  suite, the gemini merge simulation) already guards the code paths that can be guarded without
  a build.

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

