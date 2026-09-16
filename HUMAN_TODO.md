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

**Before picking up any entry below, read this.** The msb cutover
(`run-tasks-on-microsandbox.md`, hardware-verified 2026-09-04) deleted `internal/proxy`,
`network.mitm`, and the ephemeral per-run CA that several entries here still reference
(`mitm: true`, `proxy.log`, `openssl s_client -proxy 127.0.0.1:3128`). Those entries — the
opencode/gemini-cli `NODE_EXTRA_CA_CERTS` verification, the trixie/rtk credential checks —
describe a procedure that no longer applies as written: msb substitutes credentials at its own TLS
boundary, with no `krayt.yaml` `mitm:` key and no `proxy.log` artifact. They have not been
rewritten; whoever picks one up needs to re-derive the msb-equivalent steps first.

1. **`krayt-agent-opencode`** — the one image never published or exercised. Its entry below covers
   the publish check, an onboarding run, the `ask_human` question round-trip, and the
   `NODE_EXTRA_CA_CERTS` check re-homed from §14 Phase 9.
2. **`krayt-agent-gemini-cli`'s question channel** — the one clause of that image's verification
   still unrun; publish and onboarding are confirmed by real runs.
3. **A known defect** in the agent images' `/output/report.md` contract (`[BUG]` below), which will
   corrupt the opencode verification the same way it corrupted a gemini one unless the task writes
   somewhere else.
4. **`krayt-dev`'s floating base pin** — the rebuild, the repin, and the injected-run verification
   are all done (`krayt.yaml` runs `sha-cbca700`, built from `main`'s tip). What's left is that
   `hack/krayt-dev/Dockerfile`'s `FROM` is still tag-only, so the base floats, plus two
   CA-sensitive checks nothing has exercised yet (`[tooling]` below).
5. **The trixie base bump + rtk install** — landed and verified end-to-end **for Claude Code on
   arm64**: `rtk 0.45.0` runs in the published image, and two real `krayt-dev` runs
   (`run_9e0a56de` on, `run_378dac2d` with `KRAYT_RTK=off`) prove the hook intercepts live tool
   calls and that the opt-out is honoured rather than silently absent. What remains needs live
   Gemini/OpenCode credentials — the same proof for those two agents — plus the amd64/other-image
   manifest check. See the `[tooling]` entry below.
6. **`seed-agent-first-run-config.md`** — code done, offline-verified; every hardware check is
   unrun (no real Mac/msb/live credential here). See its own `[HUMAN]` entry below.

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

## [HUMAN] real `krayt run` on a linux/arm64 host with KVM

`expand-platforms-under-msb.md` Part A adds linux/arm64 to the release matrix
(`release-please.yml`), teaches `krayt upgrade` the new asset name (`internal/selfupdate`, unit
tested against an `httptest` fixture), lists it in `README.md`'s supported-platforms paragraph,
and closes the open question in `KRAYT_SPEC.md` §15. CI now builds and unit-tests the whole repo
natively on a hosted `ubuntu-24.04-arm` runner (`.github/workflows/ci.yml`'s `test-linux-arm64`
job) rather than merely cross-compiling it — but that runner has no KVM (it's itself a VM), so it
cannot prove the one thing that actually matters: a real msb sandbox booting on arm64 KVM.

- **Needed:** on a real arm64 Linux host with `/dev/kvm` and `msb` installed, run `krayt doctor`
  (all four msb checks pass) then a plain `krayt run --image <agent image> --task <file> --repo
  .`, through to `done`/`exit 0` with a non-empty `changes.patch` — the same bar
  `run-tasks-on-microsandbox.md`'s hardware pass set for the amd64/Apple-Silicon case
  (`run_d25279fb`).
- **Why the agent can't:** no arm64 Linux host with KVM available in this environment; the hosted
  CI runner above only proves the binary builds and unit-tests correctly on the arch, not that a
  sandbox boots.
- **Verify success by:** `krayt ls` reaching `done`, `changes.patch` applying cleanly
  (`krayt apply`), and `krayt doctor` passing on that host.
- **Blocking:** no — Part A's non-hardware criteria are all met and shippable without this; this
  closes the loop the way §14 Phase 11's hardware pass did for the msb cutover.

---

## [HUMAN] real `krayt run` on Windows 11 with WHP, including a `--on-question=wait` round trip

`expand-platforms-under-msb.md` Part B ports krayt's small OS-specific seam to Windows: the
cross-process concurrency lock (`LockFileEx`), the ask_human channel (a named pipe via
`github.com/Microsoft/go-winio` instead of a unix socket), where the ask/control sockets live, the
detached-supervisor process attributes, and the RAM/disk preflight probe
(`GlobalMemoryStatusEx`/`GetDiskFreeSpaceEx`). `GOOS=windows GOARCH=amd64 go build ./...`/`go vet
./...` are green, and CI runs `go build`/`go test ./...` natively on a hosted `windows-latest`
runner — but that runner has no WHP available (nested virtualization isn't exposed there), so it
proves the port compiles and the OS-agnostic suite passes, not that a real sandbox boots.

The native Windows `go test ./...` job (`build + test (windows/amd64, native)`) is green as of
`e13af71` — all 12 of this PR's CI checks pass (`gh pr checks`). That confirms the OS-agnostic
suite and the fixes below compile and pass on a real Windows runner; it does not touch WHP, which
that hosted runner doesn't expose (see the intro above), so the hardware needs below still stand.

- **Needed:**
  1. `krayt doctor` on a real Windows 11 host with WHP enabled and `msb` installed — all msb
     checks pass, including `msb doctor`'s own WHP report.
  2. A plain `krayt run --image <agent image> --task <file> --repo .` through to `done`/`exit 0`
     with a non-empty `changes.patch` — the same bar `run-tasks-on-microsandbox.md`'s hardware pass
     set for the amd64/Apple-Silicon case (`run_d25279fb`).
  3. **One `--on-question=wait` run**, confirming the full `ask_human` round trip over the
     Windows-specific path this task added: the guest dials vsock as it always does, msb bridges
     that to the named pipe `internal/askbridge.Listen` created (not a unix socket), and `krayt
     answer` resolves the question via the (unix-domain, unchanged) run-control socket. This is the
     one piece of this port with no offline equivalent — `listen_windows_test.go` proves the
     listener round-trips a connection, but only real msb vsock-to-pipe bridging proves the wiring
     end to end (the same gap `hack/msb-probes/p1-vsock-nonroot.sh` closed for macOS's unix-socket
     path in §14 Phase 11).

     Note that the host end of this route was wrong until now and is worth re-reading before the
     run: `orchestrator.go` hardcoded the `--vsock` route's `HostPath` to
     `filepath.Join(askDir, "ask.sock")`, which on Windows named a file nothing ever bound — the
     ask channel is the named pipe `askbridge.Listen` created. It now passes `lis.Addr().String()`
     (identical on unix, where `Listen` binds exactly that path), and `internal/askclient` grew a
     `dialLocal` seam so a `\\.\pipe\` address is dialed through `winio.DialPipe` rather than
     `net.Dial("unix", ...)`. Both are **compile- and unit-test-verified only**; this run is what
     proves them against real msb.
  4. **A `krayt stop` on a live run**, to confirm the documented residual (§12): it should
     hard-terminate the supervisor (no graceful msb teardown), so check afterward whether `msb ls`
     still shows the sandbox running — expected, and the point of recording this here rather than
     letting it surprise someone as a "stop doesn't work" bug report.
  5. Optionally, exercise `krayt upgrade` on Windows once a real release exists — confirm the `.zip`
     asset resolves and that `selfupdate.installBinary`'s Windows path (rename the running
     `krayt.exe` out of the way, then rename the new binary into place) actually succeeds against a
     mapped executable. This is the standard Windows self-update technique, but — like the rest of
     this entry — it is unverified on real hardware.
- **Why the agent can't:** no Windows host with WHP available in this environment (or any Windows
  host at all); the hosted CI runner above only proves the binary builds and unit-tests correctly,
  not that WHP boots a real sandbox or that msb's vsock-to-named-pipe bridge behaves the way
  `KRAYT_SPEC.md` §12 assumes.
- **Verify success by:** `krayt ls` reaching `done`, `changes.patch` applying cleanly, `krayt
  questions`/`krayt answer` resolving a real waiting question, and `krayt doctor` passing.
- **Blocking:** no — Part B's non-hardware criteria (build, vet, unit tests, CI) are all met and
  shippable without this; it closes the loop the way §14 Phase 11's hardware pass did for the msb
  cutover, and the way the linux/arm64 entry above closes it for that platform.

---

## [HUMAN] `krayt shell` — every "Verify first" check and every hardware Done-when criterion (`add-interactive-shell-session.md`, `KRAYT_SPEC.md` §14 Phase 12)

`krayt shell` (host code, `orchestrator.Shell`/`AttachShell`/`PatchLiveShell`, `krayt-agent-shellenv`
in all three published agent images, the spec/README amendments) is done and offline-verified —
`go build`/`go vet`/`go test`/`golangci-lint` are all green, and the `Shell`/`AttachShell`
teardown-on-error-with-`--keep` matrix is unit-tested against the fake `msb`
(`internal/orchestrator/shell_test.go`). **Nothing in it has run against a real sandbox.** The
task requires four numbered "Verify first" checks *before* trusting the design at all, plus its
own seven-point Done-when — none of which this environment can run.

- **Needed:** a real Apple-Silicon Mac with `msb` (≥ `0.6.16`) installed and a live model-provider
  credential (`ANTHROPIC_API_KEY` or `CLAUDE_CODE_OAUTH_TOKEN`), to run the following in order —
  each decides how much of the next one even applies, so don't skip ahead:
  1. **"Verify first" #1 — is `msb exec -t` a usable terminal when krayt inherits stdio?** Boot
     `ghcr.io/418-cloud/krayt-agent-claude-code` by hand
     (`krayt shell --image ghcr.io/418-cloud/krayt-agent-claude-code --repo <some-repo>`) and check:
     window resize reflows (`SIGWINCH` — resize your terminal mid-session and run `stty size`
     inside), `Ctrl-C` interrupts the foreground command (`sleep 100` then Ctrl-C — you should stay
     in the shell, not lose the session) rather than killing the session, a full-screen TUI
     (`top`, `vim`) renders and exits cleanly, and 256-colour (`echo -e '\e[38;5;196mred\e[0m'`)
     survives. This is decision 10's whole premise — if it fails, the fallback is hand-rolled raw
     mode via `golang.org/x/sys` (already pinned) and needs a redesign of `sandbox.ExecTTY`, not
     just a note.
  2. **"Verify first" #2 — does `msb exec` inherit the sandbox's create-time environment?**
     `CreateSpec.Env` sets it at `msb create`; `TTYExecSpec` (deliberately) carries no `Env` field
     at all. Inside the shell, `echo $SOME_TEST_VAR` after `krayt shell --image ... --repo ...`
     where the image sets a test env var, or more directly: does the model-provider credential env
     var (`ANTHROPIC_API_KEY`/`CLAUDE_CODE_OAUTH_TOKEN`) already show up in `env` inside the shell
     with no help from `krayt-agent-shellenv`? If yes, `krayt-agent-shellenv`'s scope (currently
     just `safe.directory`) is confirmed complete. If no, find out what's actually missing and add
     it there, scoped as narrowly as `safe.directory` is.
  3. **"Verify first" #3 — what does the `--secret` placeholder look like inside an exec'd shell,
     and does `claude` started by hand actually authenticate?** Inside the shell:
     `claude -p "say hello"` (with `--secrets` naming a real `ANTHROPIC_API_KEY` and `--allow
     api.anthropic.com` on the `krayt shell` invocation) should reach the real API — confirms §8.2's
     placeholder-substitution contract holds for an interactively-started agent, not just the
     headless entrypoint's `claude -p`. Note (2026-09-16): under msb, `--allow api.anthropic.com`
     alone is not enough — the credential also needs a `network.inject` scope, or
     `ValidateNetworkPolicyForMsb` rejects the run before it boots. As of this date that scope is
     resolved automatically whenever `krayt.yaml` sets `agent.adapter: claude-code` (`shell` now
     calls the same adapter secret-scoping `run` does, KRAYT_SPEC.md §13's 2026-09-16 amendment) —
     so the simplest repro is `krayt shell --image ... --config krayt.yaml --secrets <file>` with
     `agent: { adapter: claude-code }` in that config, no hand-written `network.inject` needed. A
     bare `--allow api.anthropic.com` with no `agent.adapter` and no hand-written
     `network.inject` should still fail pre-flight, the same way `krayt run` would.
  4. **Answered, 2026-09-16, by the first real attempt (`krayt shell --image
     ghcr.io/418-cloud/krayt-agent-claude-code --config krayt.yaml --skip-resource-check`, no
     `--task`): with no `Command`, `msb exec --tty` re-execs the image's `ENTRYPOINT`
     (`krayt-agent-entrypoint`), not a shell.** The session showed
     `[claude-code] authenticated via CLAUDE_CODE_OAUTH_TOKEN` immediately followed by
     `[claude-code] task file /task/prompt.md not found` and ended with exit 66 — the headless
     entrypoint running and failing because `krayt shell` (no `--task`) never copies in
     `/task/prompt.md`, not a shell prompt with `krayt-agent-shellenv` sourced. **Fixed**:
     `internal/orchestrator/shell.go` no longer leaves `TTYExecSpec.Command` empty —
     `ttyCommand`/`defaultShellCommand` resolve `$SHELL`, else `/bin/bash`, else `/bin/sh`
     explicitly (`KRAYT_SPEC.md` §6.15 and Phase 12 updated with the finding and the fix). **Still
     needs a hardware re-run** to confirm the fix actually lands at a shell prompt this time, and
     then: `echo $0` and `git config --global --get-all safe.directory` (should show `/workspace`
     and `*`, proving `krayt-agent-shellenv` ran) — if neither hook fires even now, that's a
     separate real bug in `images/agents/*/Dockerfile`'s two `RUN printf ... /etc/...` lines.
  5. **Done-when 1-2:** `krayt shell --image ghcr.io/418-cloud/krayt-agent-claude-code --repo .`
     drops you at a shell with the repo present; edit a file, `exit`; confirm
     `.krayt/runs/<id>/changes.patch` (and `meta.json` with `"kind": "shell"`) contains the edit,
     and `krayt apply <run-id>` lands it on the host repo cleanly.
  6. **Done-when 3-4:** `krayt shell --keep`, edit a file, `exit` — confirm the sandbox is still
     listed by `msb ls`. `krayt shell --attach <run-id>` from a second terminal — confirm the edit
     from step 5 is still there, make a second edit, exit again — confirm both edits are now in
     `changes.patch`. `krayt stop <run-id>` — confirm `msb ls` no longer lists the sandbox and
     `krayt ls` shows the record as `done`. Separately, repeat the ephemeral (no `--keep`) case and
     kill the `krayt shell` process (`kill` its pid, or close the terminal) mid-session, plus once
     with a deliberately bad `--image` (to force a failed `krayt-helper setup`) — confirm `msb ls`
     shows no leaked sandbox in either case (this is the one part the fake-`msb` unit tests
     *cannot* prove — they prove krayt calls `msb stop`/`msb rm`, not that a real `msb` actually
     tears the VM down when asked).
  7. **Done-when 5:** from a second terminal while a `krayt shell` session (no `--keep` needed) is
     live, run `krayt patch <run-id>` — confirm it prints a fresh `changes.patch` reflecting
     whatever's currently in `/workspace`, without ending the session; run it again after another
     edit — confirm the patch updates.
  8. **Done-when 6:** manually create an orphan (`msb create --name krayt-test-orphan <image>`,
     no matching `.krayt/runs/` entry) and run `krayt doctor --repo .` — confirm it reports a
     `[warn]` naming `krayt-test-orphan` and the `msb stop`/`msb rm` command to remove it, and does
     **not** remove it itself. Then `krayt doctor --repo .` again with no orphan present — confirm
     it stays silent (no `[warn]` line at all).
  9. **Done-when 7:** confirmed by step 3 above, if `claude -p` authenticates and reaches
     `api.anthropic.com` under the run's allowlist.
- **Why the agent can't:** no real hardware — no Apple-Silicon Mac (or Linux/KVM host) with `msb`
  installed anywhere in this environment, and every one of the nine checks above needs a real
  sandbox boot, a real pty, or a real credential reaching a real API.
- **Verify success by:** each numbered item above has its own inline check. Record the outcome of
  each "Verify first" check explicitly in `KRAYT_SPEC.md` §14 Phase 12 (the phase currently says
  "not met; every criterion below needs a real Apple-Silicon Mac") and in this file's history —
  if any of them turns up a real gap (most likely #2/#3, whether `krayt-agent-shellenv` needs more
  than `safe.directory`, or #4, which hook actually fires), fix it and re-verify before checking
  the phase done, per `CLAUDE.md`'s "never fabricate a result" rule.
- **Windows:** genuinely untested and **not claimed to work** — inherited stdio is the same code
  path as macOS, but nobody in this environment can confirm ConPTY through msb without a Windows
  box at all (a lesser bar than the WHP entry above even needs, since `krayt shell` needs a real
  interactive terminal, not just a boot). Track this as a separate follow-up once the macOS pass
  above lands; don't fold it into this entry.
- **Blocking:** no for shipping the code (it's additive, off by default in the sense that nobody
  who doesn't run `krayt shell` is affected, and every non-hardware Done-when criterion is met) —
  but yes for closing `KRAYT_SPEC.md` §14 Phase 12 and for trusting `krayt shell` in anger. Until
  this lands, treat `krayt shell` as "compiles, passes its offline tests, never run for real."

---

## [HUMAN] `seed-agent-first-run-config.md` — five hardware checks, real Mac + msb 0.6.16 + a live credential each

Seeding each agent's first-run guest config from its adapter (`internal/adapter.Plan.ConfigSeeds`,
`internal/orchestrator`'s shared `applyConfigSeeds` step wired into both `Run` and `Shell`,
`internal/configseed`'s fill-in-never-override merge) is done and offline-verified: `go build`
(both `GOOS`), `go vet`, `go test -race`, and `golangci-lint` are all green, and the merge/adapter/
sandbox/orchestrator/CLI behavior is unit-tested — including against the fake `msb`
(`internal/orchestrator/fakemsb_test.go`, extended to actually read/write files inside the fake
sandbox root, honor a create-time `--env` value for the `DirEnv` probe, and accept piped stdin) —
for every case the task's own test list names: seeds written as the agent user before the agent/
tty exec, an existing file's other keys surviving the merge, an unchanged merge performing no
write, invalid JSON left byte-identical with a warning, a failing seed exec not failing the run,
`DirEnv` redirecting the path, an invalid `DirEnv` name rejected before any exec, and
`AttachShell` performing no seed exec at all. **Nothing in it has run against a real guest.**

- **Needed:** on a real Apple-Silicon Mac with `msb` (≥ `0.6.16`) installed:
  1. **`CLAUDE_CODE_OAUTH_TOKEN`:** `krayt shell --image ghcr.io/418-cloud/krayt-agent-claude-code
     --config krayt.yaml` (with `agent: { adapter: claude-code }` in that config, and the token in
     the secrets file), then run `claude` by hand inside the shell. Expect no onboarding screen and
     an authenticated prompt; a one-line request (`claude -p "say hello"` or the interactive
     equivalent) succeeds.
  2. **`ANTHROPIC_API_KEY`:** the same flow with an API key instead. Expect no "Detected a custom
     API key in your environment" approval dialog, and authentication succeeds. Inside the shell,
     run `printenv ANTHROPIC_API_KEY` and confirm it prints exactly `$MSB_ANTHROPIC_API_KEY` — if
     it prints anything else, `sandbox.SecretPlaceholder`'s assumed default is wrong and the
     seeded `customApiKeyResponses.approved` value (its last 20 characters) needs to be read from
     the guest instead of assumed.
  3. **Whether `platform.claude.com` is still needed.** With onboarding seeded
     (`hasCompletedOnboarding: true`), the connectivity preflight that used to contact it no longer
     runs. Test with it removed from `krayt.yaml`'s `allow`/`passthrough` and confirm `claude`
     still authenticates; record the answer in `images/agents/claude-code/README.md`'s "Required
     `--allow` hosts" section either way.
  4. **`gemini-cli` with `GEMINI_API_KEY`:** `krayt shell --image ghcr.io/418-cloud/krayt-agent-gemini-cli
     --config krayt.yaml` (`agent: { adapter: gemini-cli }`), then start `gemini` interactively
     inside the shell. Expect no auth dialog (`security.auth.selectedType` seeded) and no
     folder-trust dialog (`GEMINI_CLI_TRUST_WORKSPACE=true` in `Plan.Env`).
  5. **A minimal image with no krayt entrypoint at all** — e.g. `debian:trixie-slim` plus the
     official Claude Code installer, run as a non-root user named `agent`, no
     `krayt-agent-entrypoint`/`krayt-agent-shellenv` baked in. Repeat check 1 against it. This is
     the check that actually proves the "image-agnostic" claim: every other check above uses a
     published krayt image, which could in principle still be passing for some unrelated reason.
- **Why the agent can't:** no real hardware — no Apple-Silicon Mac (or Linux/KVM host) with `msb`
  installed anywhere in this environment — and no live Anthropic or Gemini credential to test
  authentication against a real API.
- **Verify success by:** each numbered item above has its own inline check. Record the outcome of
  each explicitly in `KRAYT_SPEC.md` §14 Phase 12's follow-up bullet for this task (currently every
  hardware box is unchecked) and in this file's history, per `CLAUDE.md`'s "never fabricate a
  result" rule — if any of them turns up a real gap (most likely #2, whether
  `sandbox.SecretPlaceholder`'s assumed `$MSB_<NAME>` default still matches a real msb 0.6.16
  install, or #5, whether a bare image needs something `krayt-agent-shellenv` normally provides
  that this task didn't anticipate), fix it and re-verify before checking any box done.
- **Also open, logged per the task's "out of scope" section — verify with a live key, don't fix
  without one:** Gemini's `GOOGLE_API_KEY` credential is scoped to
  `generativelanguage.googleapis.com` (this task's adapter change), but the gemini-cli entrypoint's
  own comment says that credential shape actually goes through Vertex AI Express
  (`aiplatform.googleapis.com`). If that's right, msb never substitutes it and a `GOOGLE_API_KEY`
  run silently sends the placeholder string to Google. Confirm with a live `GOOGLE_API_KEY` which
  host it actually calls, and file a follow-up to fix the adapter's `Hosts` if the entrypoint's
  comment is right.
- **Blocking:** no for shipping the code (additive; every non-hardware criterion is met) — but yes
  for closing `KRAYT_SPEC.md` §14 Phase 12's follow-up bullet and for trusting that a `krayt shell`
  user can actually start `claude`/`gemini` by hand without hitting onboarding or an auth dialog.
