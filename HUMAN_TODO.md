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
6. **A `krayt-dev` rebuild + repin** — the image was rebased onto the published
   `krayt-agent-claude-code` and now inherits its entrypoint, and the repo's own `krayt.yaml`
   injects both credentials at the host proxy. Neither change is in the pinned image yet
   (`[tooling]` below). Until that lands, `krayt run --config krayt.yaml` exits 78.

The host-side-proxy arc (all three steps) is **done and verified on hardware**: the egress proxy
runs host-side over vsock, terminates TLS for allowlisted hosts, and attaches the real credential
itself — a subscription token now never enters the VM (`run_df97fffa`, control `run_10fc027d`), and
Node trusts the ephemeral CA through `NODE_EXTRA_CA_CERTS` (`run_c74208b4`, with `proxy.log`
corroborating the negative control independently of the agent's own report).

---

## [tooling] rebuild `krayt-dev` on its new base, repin `krayt.yaml`, and verify the injected run

Two changes land together here, and neither is in a published image yet:

1. **`krayt-dev` was rebased** onto `ghcr.io/418-cloud/krayt-agent-claude-code:2.1.226` and now
   **has no entrypoint of its own** — `hack/krayt-dev/entrypoint.sh` is deleted, and the base's
   `krayt-agent-entrypoint` is inherited. Nothing replaced it: `gh` authenticates from the injected
   `GH_TOKEN` env var with no setup step. One `FROM`, everything else additive: Go arrives as the
   official release tarball (`ARG GO_VERSION`, new Renovate manager), and `gcc`/`libc6-dev` are
   added because the slim base has no C toolchain and `go test -race` needs cgo.
2. **`krayt.yaml` runs with `network.mitm: true`**: the Anthropic credential is injected by the
   claude-code adapter's own rule, `GH_TOKEN` by a hand-written `api.github.com` rule, so neither
   reaches the VM. The §8.2 contracts that requires now live in the shared entrypoint.

`krayt.yaml` still pins `ghcr.io/418-cloud/krayt-dev:sha-376210a`, which predates all of it. Run
the current config against the current pin and it exits 78 before Claude starts.

- **Needed:**
  1. **Confirm the base tag exists and is anonymously pullable** —
     `docker buildx imagetools inspect ghcr.io/418-cloud/krayt-agent-claude-code:2.1.226` shows
     `linux/amd64,linux/arm64`. This is a new hard dependency: `dev-image.yml` cannot build at all
     without it, on PRs included. If that tag is missing, the `FROM` needs a tag that does exist.
  2. Build and push `krayt-dev` from `main` (CI does this on merge; confirm the digest actually
     moved rather than assuming it). Check the build log shows the base being pulled, not built.
  3. **Get the base digest onto the `FROM` line** — Renovate should do it (`pinDigests: true`
     covers the dockerfile manager); confirm its first PR actually lands, or add the digest by
     hand. The line is tag-only today, deliberately: an invented digest is worse than an absent
     one. **Do not leave it that way.** `agent-images.yml` re-points `:2.1.226` on every build off
     `main`, so an unpinned `FROM` floats — krayt-dev would absorb base changes silently on any
     unrelated rebuild, and the Renovate digest PR that is the intended delivery path for a base
     change (see `dev-image.yml`'s `paths` comment) would never be opened.
  4. Repin `image:` in `krayt.yaml` to the new `sha-<short>` tag.
  5. One real run through the injected path, from the repo root:
     ```sh
     KRAYT_PROXY_LOG_REQUESTS=1 krayt run --config krayt.yaml --task ./some-krayt-task.md
     ```
     with a task that runs `go build ./... && go test ./...` (exercises the `passthrough` hosts
     under a CA-rewritten trust store) **and** one `gh api repos/418-cloud/krayt/pulls` call
     (exercises the injected `api.github.com` rule).
- **Why the agent can't:** no `docker build`/push access, no live subscription token or PAT, and no
  Apple-Silicon Mac to boot a VM on. The offline half is done and passing:
  `hack/test-entrypoint-credentials.sh` (now run by `ci.yml`) covers the shared entrypoint's
  credential paths plus the branches krayt-dev relies on — model/effort, and that no entrypoint
  touches GH_TOKEN at all (tests 12–15) — and `TestApplyConfigDogfoodsThisRepo` pins the config.
  With no entrypoint left in the gh path, that Go test is the ONLY thing wiring GitHub auth: it
  asserts both the `env.GH_TOKEN` placeholder and the `api.github.com` inject rule.
- **Verify success by**, in order — each one distinguishes a different failure:
  - every entrypoint log line carries the `[claude-code]` prefix — `authenticated via
    CLAUDE_CODE_OAUTH_TOKEN` and `trusting krayt's ephemeral MITM CA`. A `[krayt-dev]` line would
    mean the old image is still pinned. There is deliberately no gh line at all now;
  - `claude` runs with `--model claude-sonnet-5 --effort high` (the image's ENV defaults reaching
    the base entrypoint's optional selection branch), or whatever `krayt.yaml`'s `env:` says;
  - `go test -race ./...` links and runs — the check for `gcc`/`libc6-dev` actually being present
    on the slim base;
  - `/run/secrets` contains **neither** credential (the run's `report.md`/`meta.json` list both as
    injected, names only);
  - `proxy.log` shows `inject=true` on `api.anthropic.com` and `api.github.com`, and only the
    `CONNECT` line for each `passthrough` host;
  - the `gh api` call returns real PR JSON — a 401 means the `Bearer ` prefix or the header name is
    wrong for a fine-grained PAT, which is the one thing here observed only from GitHub's docs and
    not from a live krayt run;
  - `go test ./...` passes **inside** the container. This is the check that would catch the
    concatenated CA bundle being wrong: the Go toolchain talks to `passthrough` hosts, whose real
    upstream chain must still verify while `SSL_CERT_FILE` points at krayt's rewritten bundle.
- **Blocking:** yes for anyone dogfooding through `krayt.yaml` — that path cannot work until the
  image is rebuilt. Not blocking any other work; runs that pass `--image`/`--allow` by flag are
  unaffected.

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
