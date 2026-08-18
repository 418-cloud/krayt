# HUMAN_TODO

Single handoff log for steps the coding agent cannot complete itself (credentials, real
hardware, a Linux builder, live secrets). Template per `KRAYT_SPEC.md` §14.

---

## Status

**✅ DONE — `inject-claude-oauth-token-at-proxy.md` (step 3) is verified on hardware.**
`run_df97fffa` on 2026-08-18, with its `mitm: false` control `run_10fc027d`: a live
`CLAUDE_CODE_OAUTH_TOKEN` run returned **200** on `POST /v1/messages` with the real 108-byte token
attached host-side, while the container held only a 28-byte placeholder and had no `/run/secrets`
at all. The control run — same task, same image, token delivered the ordinary way — found the token
in `/run/secrets`, which is what makes the first run's absence mean something. Details, including
the two facts that run observed for the first time, are in the `[hardware]` entry below.

**One follow-up, non-blocking:** republish the three agent images so the fixed entrypoint ships
(`[BUG]` entry below). Today's `:latest` works via the `KRAYT_INJECTED_CREDENTIAL` compatibility
branch — which is why the placeholder on the wire was the entrypoint's 28-byte string rather than
krayt's own 41-byte self-describing one.

**Open (two loose ends, both small):** `add-tls-mitm-credential-injection.md` (Phase 9, §14) is
**complete and its §14 checkbox is ticked** — verified with live credentials across two providers
(`run_c654e575` Anthropic injection, `run_117d6f75` the `mitm: false` mirror, `run_e19488dd`
Gemini/Node), each with the negative control that makes its positive mean something. See the
`[hardware]` entry below for the evidence. What that phase left behind:

1. **The `gemini-cli` entrypoint fix has never actually run** — the folder-trust bug (`[BUG]`
   entry) was worked around config-side with `env: {GEMINI_CLI_TRUST_WORKSPACE: "true"}` to
   unblock the verification, so the fix committed to `entrypoint.sh` is still unexercised. It
   needs an `agent-images.yml` republish, then one run against `:latest` **without** the
   config-side override to confirm the image stands on its own.
2. **The `opencode` `NODE_EXTRA_CA_CERTS` check** was re-homed to that image's `[tooling]` entry
   (gated on it being published, not on any MITM code). Recorded there as required, with the full
   method — it is the one clause of Phase 9's "each of the three agent images" not yet run.

**Open:** the `krayt-agent-claude-code`, `krayt-agent-gemini-cli`, and (new)
`krayt-agent-opencode` published images each need a real CI run + a real live onboarding run —
see the `[tooling]` entries below. The gemini-cli image also needs its `node:24-bookworm-slim`
base digest pinned (currently tag-only — this sandbox's egress proxy has no route to a Docker
registry) and a `hadolint` pass (binary unreachable here too); the opencode image needs the same
`hadolint` pass, for the same reason. All non-blocking (nothing downstream depends on them yet;
the codex agent-image task builds on the *code* landing here, not on these verifications).

The rootfs-compression handoff (ratio/timing + a real post-decompress boot) is
now fully confirmed — see the `[tooling/CI]` entry below it.

Everything else is shipped: all three integration-test-runner handoffs are confirmed — two on real
hardware, and `integration-linux` is now green in CI. The `gh` CLI + `GH_TOKEN` +
`fix-pr-review-comments` change is also fully confirmed now — real image build (both arches), a
real read-only fine-grained PAT authenticating and reading review comments (and genuinely refused
on a write attempt), and a real end-to-end run against a real PR. See the three `[tooling]` /
`[GitHub]` entries below. The vmimage RC/graduate workflows are confirmed too — real PR-triggered
RC publish, a real graduate dispatch with matching digest, and concurrent-PR queuing under the
`vmimage-rc-tag` concurrency group. See the `[tooling/CI]` entry below.

Phases 0–7 are complete and released as
[`v0.5.0`](https://github.com/418-cloud/krayt/releases/tag/v0.5.0) — krayt runs a real coding
agent in an isolated micro-VM over an untrusted repo and hands back a reviewable patch, with
egress control, secrets redaction, concurrency, park-and-walk-away, and an agent↔human question
channel, on **both** macOS/vfkit and Linux/firecracker behind the same `Provider` interface. All
security-review findings (Critical, High, Medium, and Low) are fixed and verified on hardware —
see `docs/ai-tasks/README.md` for the fix-by-fix status table. The multi-arch base VM image and
all seven probe images are published and public on GHCR, and a real Claude Code agent run has
completed on both backends against the same pinned image digest.

The detailed phase-by-phase and finding-by-finding history that used to live in this file has been
pruned now that it's shipped — the record of *how* lives in `git log`/PR history,
`docs/ai-tasks/README.md`, and `KRAYT_SPEC.md`'s own `[x]` phase checklists, not here. This file
only tracks what's still open.

---

## [hardware] `move-egress-proxy-to-host.md` — image rebuild, `PinnedRef` bump, and Phase-3 egress suite re-verification — ✅ DONE

Verified on **both** backends against image
`sha256:4fe2b0b78581d5194ded643fbe5b73c5d69372e70955a37ab716d680974f5014` — an Apple-Silicon Mac
(vfkit) and a Linux/KVM box (firecracker), `hack/run-integration-tests.sh` green end to end on
each: `TestBootHello`, `TestEndToEndRealVM`, `TestEgressEnforcement`, `TestContainerHardening`,
`TestRootImageFailsClosed`, `TestGuestGitConfigInjectionInert`, `TestSecretConfinementInArtifacts`,
`TestConcurrentRealVMs`.

Both checks this task added are confirmed for real:

- **No `skuid` rule in the live guest ruleset.** The guest reads back its own installed ruleset
  after applying it (`verifyInstalledRuleset`), fails the run closed if the lock is wrong, and
  publishes the dump to `/dev/console`; `assertGuestRuleset` reads it out of the run's
  `console.log`. Confirmed identical on both backends: `table inet krayt_egress` with
  `policy drop` + `oif "lo" accept`, and no `skuid` anywhere in the ruleset — NixOS's own
  `nixos-fw` table (input/prerouting only, no bearing on egress) included in the scan.
- **A private-range target through the proxy is refused.** `hack/netprobe` check 4 (exit 24) →
  403 on both backends.

Two things this took three hardware runs to get right, worth knowing before writing the next
guest-side check:

1. **A stale image passes the whole netprobe.** The first run was against a pre-Phase-8 image,
   and every netprobe check passed anyway — allowlisted reachable, non-allowlisted blocked, raw
   socket blocked, private target 403 — because that image's deleted in-guest L7 proxy enforces
   exactly the same observable behavior, and the SSRF guard predates Phase 8 (#40). The netprobe
   cannot distinguish the two architectures; the ruleset shape is the only thing that can. That
   is the whole reason this check exists, and it caught the stale image on its first run.
2. **The guest agent's log does not reach the host.** `krayt-agent` is a systemd unit with no
   `StandardOutput` override (`images/flake.nix`), and journald has no `ForwardToConsole`, so its
   stdout/stderr go to the journal and die with the VM. The first implementation used
   `log.Printf` and failed on hardware for that reason alone. The evidence is now written to
   `/dev/console` explicitly. Note the wider gap this exposes: a guest-agent error during a
   hardware run is currently invisible from the host — `StandardOutput=journal+console` on the
   unit would fix that, and is not done here.

`KRAYT_SPEC.md` §14's Phase 8 "Done when (hardware)" is ticked, with one clause recorded there as
holding **by construction rather than by assertion**: `TestConcurrentRealVMs` sets no
`NetworkPolicy`, so it proves each concurrent VM gets its own egress socket + child process
(`spawnEgressProxy` runs per run; a colliding socket would fail them) but never asserts cross-VM
egress unreachability. That is structural — each VM dials `provider.EgressPort` on its own CID
against a per-VM host socket, so a guest cannot name another run's proxy. Asserting it would need
per-run allowlists in the netprobe.


## [hardware] `add-tls-mitm-credential-injection.md` — real MITM run, `NODE_EXTRA_CA_CERTS` check, and Phase-3 re-verification

- **Needed:** (1) ~~the SAME guest image rebuild Phase 8 above is already waiting on~~ — **done**:
  this task's guest-side pieces (`/run/krayt/ca.crt`, `KRAYT_CA_CERT` + trust-store env vars) ride
  image `4fe2b0b7…`, already pinned and verified on both backends, so there is no image left to
  build and nothing here is blocked any more; (2) ~~a real `claude-code` image run with
  `mitm: true`~~ — **done, run `run_c654e575` on darwin/vfkit with a live `ANTHROPIC_API_KEY`.
  Every clause held:**
    - `/run/secrets` **does not exist at all** inside the container — stronger than "exists
      without the key". The injected key was the only secret, so withholding it left the bundle
      empty and no tmpfs was mounted.
    - `ANTHROPIC_API_KEY=krayt-injected-at-host-proxy` (the entrypoint's placeholder, not a
      credential) and `KRAYT_INJECTED_CREDENTIAL=ANTHROPIC_API_KEY`. Note the variable IS present
      — asserting its absence would be wrong; the value is what matters.
    - A `curl` sending **no** `x-api-key` and no `Authorization` got `http_code=200` and a real
      model list. A client with no credential receiving an authenticated response is the claim,
      demonstrated directly.
    - `openssl s_client -proxy 127.0.0.1:3128` showed leaf `CN = api.anthropic.com` issued by
      `krayt ephemeral MITM CA (run_c654e575)` — interception proven by the chain itself, with
      the per-run CA identity visible in the issuer.
    - `claude -p` itself completed exit 0 through the MITM path, so the real client authenticated,
      not just curl.
    - Both `default-trust=200` and `explicit-ca=200`, which settles an open question: Debian
      curl **does** honor the entrypoint's `SSL_CERT_FILE` (the concatenated distro+krayt bundle),
      so the §8.2 approach works for OpenSSL clients and not only for explicitly-flagged ones.
  (3) ~~the same run with `mitm: false`, confirming it is unchanged (regression)~~ — **done, run
  `run_117d6f75`, an exact mirror of the injected run**: `/run/secrets/ANTHROPIC_API_KEY` present
  (108 bytes, 0644, readable by the non-root agent as designed), the real key in the env, all
  four `KRAYT_*`/CA vars unset, and the same no-auth `curl` answered **401
  `x-api-key header is required`**. That 401 is what makes the pair conclusive: it rules out any
  ambient auth and pins the injected run's 200 to the injection specifically.
    - **Also surfaced a real §10 residual, now documented as redaction gap (3).** krayt's
      `[REDACTED]` marker never appeared in this run's artifacts. The agent masked the middle of
      the key itself before writing the report, so no verbatim match existed for the `Redactor`
      to find, and a 19-character `sk-ant-` prefix persisted into `report.md` and
      `logs/agent.log`. Exact-match redaction cannot catch a transformed secret — and this was a
      *cooperative* agent. It is also the cleanest argument for this whole task: the companion
      `mitm: true` run had no credential in the VM to leak in any form.
  (4) `npm install` (or an equivalent TLS-heavy fetch)
  through the MITM path in **each** of the three agent images — this exercises
  `NODE_EXTRA_CA_CERTS` and is the most likely thing to break. **Partly done and partly not
  applicable as written**: `krayt-agent-claude-code` has no `npm` or `node` (it is
  `debian:bookworm-slim` + `ca-certificates curl git bash` + the native `claude` binary), so the
  "equivalent TLS-heavy fetch" clause is what it can satisfy, and curl did — but nothing in that
  image demonstrably reads `NODE_EXTRA_CA_CERTS`. The genuine `NODE_EXTRA_CA_CERTS` exercise
  needs the node-based images. **`krayt-agent-gemini-cli`: done** (`run_e19488dd`, after the
  folder-trust bug above was worked around config-side):
    - `npm install` of a real package through the MITM proxy succeeded, `strict-ssl` confirmed
      `true` so the check means something.
    - **Negative control failed as required**: the same install with only `NODE_EXTRA_CA_CERTS`
      removed died with `SELF_SIGNED_CERT_IN_CHAIN` / "self-signed certificate in certificate
      chain". That is what proves the variable is load-bearing rather than the install having
      succeeded for some unrelated reason.
    - `openssl s_client` through the proxy showed `issuer=O = "krayt (ephemeral, per-run)",
      CN = krayt ephemeral MITM CA (run_e19488dd)` — the install really did traverse the
      intercepted path.
    - **Methodology note worth keeping:** the first negative control *passed* because npm served
      the package from cache without any network request. The agent caught it, ran
      `npm cache clean --force`, and re-ran **both** arms against an empty cache — which is why
      the result stands. The persisted `report.md` does not mention the cache at all, so the
      artifact alone would lead a reproducer straight back into the same false pass; the task
      file now mandates a cache clear and requires reporting it.
  **`krayt-agent-opencode`: not run** — still unpublished. This clause needs it before it can
  close, since §14's wording is "each of the three agent images".
  (5) ~~the full Phase 3 security suite re-run on both backends~~ — **done** against image
  `4fe2b0b7…` (the same image carrying this task's guest-side pieces) as part of Phase 8's
  verification: green on darwin/vfkit and linux/firecracker, `TestEgressEnforcement` and
  `TestSecretConfinementInArtifacts` included. Those ran with `mitm` off, which is the point —
  they are the "unchanged when not opted in" regression.
- **Why the agent can't:** same reason as Phase 8 (no real Mac/vfkit or Linux/KVM in this
  sandbox), **plus** this task specifically needs a live Anthropic API key to prove the
  authenticate-without-a-credential-in-the-VM claim for real — a claim that cannot be honestly
  demonstrated any other way (a fake/mocked "credential" proves nothing about whether the actual
  injected header format Anthropic's API expects is correct).
- **Exact steps/commands:**
  1. On a Mac with vfkit and/or a Linux/KVM box (the image this needs is already pinned):
     ```sh
     krayt run --image ghcr.io/418-cloud/krayt-agent-claude-code --agent claude-code \
       --task ./task.md --repo . --config ./krayt-mitm-test.yaml
     ```
     with `krayt-mitm-test.yaml` setting `secrets: ./secrets.env` (one real
     `ANTHROPIC_API_KEY`), `network: {mode: allowlist, allow: [api.anthropic.com], mitm: true,
     inject: [{host: api.anthropic.com, strip: [x-api-key, authorization], set: {x-api-key:
     ANTHROPIC_API_KEY}}]}`.
  2. While the run is in progress (or via a debug shell into the same image), confirm
     `env | grep -i -E 'anthropic|api.key'` and `ls -la /run/secrets/` inside the container show
     nothing — the absence is the proof, not the run's exit code.
  3. Re-run with `mitm: false` (drop the `network.mitm`/`inject` keys) and confirm it completes
     identically to before this task.
  4. Repeat step 1's shape for `krayt-agent-gemini-cli` and `krayt-agent-opencode`, each doing a
     real package-manager or HTTP fetch through the MITM'd host, to exercise
     `NODE_EXTRA_CA_CERTS` on all three.
  5. `hack/run-integration-tests.sh` (or the individual `TestEgressEnforcement`/
     `TestSecretConfinementInArtifacts` runs) on both backends, unchanged from Phase 8's own
     commands.
- **Verify success by:** all of the above passing for real, with concrete evidence (command
  output showing no credential in the container, `npm install` succeeding through the MITM path
  in each image). Update `KRAYT_SPEC.md` §14's Phase 9 "Done when (hardware)" checkbox from `[ ]`
  to `[x]` and this section's status line once confirmed — do not mark it done without a real run.
- **Blocking:** no — `network.mitm` is opt-in and defaults to false; every run that doesn't set
  it is unaffected regardless of whether this verification has happened.

## [BUG] every agent entrypoint exited 78 on a shape-translated run — FIXED 2026-08-18, images need a republish

**What was broken.** `claudeCode.Prepare`'s shape-translation path delivers the credential
placeholder as ordinary container env and (by design) writes no `/run/secrets/<key>` file. But all
three entrypoints (`images/agents/*/entrypoint.sh`) decided "do I have a credential?" by looking
only for the FILE, then for `KRAYT_INJECTED_CREDENTIAL` — never for a credential variable that was
**already set**. So every `mitm: true` run using shape translation would print "no credential in
/run/secrets" and exit 78 (`EX_CONFIG`) before the agent started. No traffic, no proxy.log, nothing
to debug.

**Why no test caught it.** The Go tests assert the HOST side (`spec.Env` carries the placeholder),
which was correct. The entrypoint is a shell script that nothing but a real container ever ran, so
the contract between the two halves was unverified in both directions.

**Fixed:** each entrypoint now accepts an already-set recognized credential var, keeping its value
as-is (§8.2's contract, rewritten to list all three sources in order); `KRAYT_INJECTED_CREDENTIAL`
remains as the compatibility branch. `hack/test-entrypoint-credentials.sh` exercises all three
branches plus the fail-closed case against the real scripts, offline, with stubbed CLIs — it fails
against the pre-fix entrypoints, which is what makes it a regression test rather than a tautology.
Two smaller fixes rode along: `KRAYT_OUTPUT` overrides the hardcoded `/output` (so the script is
runnable outside a container at all), and `${extra[@]+"${extra[@]}"}` in the claude-code entrypoint
avoids bash 3.2's empty-array-under-`set -u` abort (macOS's bash; the image's own bash 5 was never
affected).

- **What a human still needs to do:** republish the three agent images so the fix is in
  `:latest` — `agent-images.yml`. **Non-blocking:** the adapter now also sets
  `KRAYT_INJECTED_CREDENTIAL`, which the *unrebuilt* published images already honor, so
  shape-translated runs work against `:latest` today. The difference the rebuild makes is whose
  placeholder value lands: krayt's self-describing `sk-ant-…-krayt-placeholder-do-not-use` (new
  images) versus the entrypoint's own `krayt-injected-at-host-proxy` (current ones). Both are
  non-secret; the former is far more legible in a log, and is the one the OAuth path may need if
  Claude Code turns out to validate token format.
- **Verify success by:** `./hack/test-entrypoint-credentials.sh` green (11/11 locally; 8 of those
  fail against the pre-fix entrypoints, which is what makes it a regression test). The hardware run
  below then confirmed the compatibility path end to end: `run_df97fffa` started fine against
  today's unrebuilt `:latest`, taking the `KRAYT_INJECTED_CREDENTIAL` branch — visible in the wire
  log as a 28-byte placeholder (the entrypoint's own value) rather than krayt's 41-byte one. After
  the republish, expect that to read 41.

## [hardware] `inject-claude-oauth-token-at-proxy.md` — wire-format probe + end-to-end verification — ✅ DONE 2026-08-18

**Needed:** step 3 of the host-side-proxy arc hides OAuth entirely by configuring the container
API-key-shaped no matter which credential the user really supplied, and having the host proxy
translate shape on the wire. That only works if Claude Code's subscription request path differs
from its API-key request path in **headers only** (same host, same path, same body shape) — and
that can only be confirmed by watching a real subscription token go through step 2's own MITM
proxy. This environment has no live Anthropic credential of either kind and cannot fake one
(`CLAUDE.md`: "never fabricate a result for a human-only step" — a mocked "credential" proves
nothing about what Anthropic's real API expects). The task file names this probe P1–P5;
**P1 is already answered** by existing evidence (below) and needs no new run. **P2, P3, and P4
genuinely need a fresh run** with a real subscription token. P5 is documentation-only and gates
nothing.

**P1 — already done, reusing existing evidence (no new run needed).** The `add-tls-mitm-credential-injection.md`
hardware verification above already ran exactly the API-key baseline this probe asks for
(`run_c654e575`, live `ANTHROPIC_API_KEY`, `mitm: true`), and recorded precisely the fields P1
asks for:
- Host: `api.anthropic.com`.
- Auth header: `x-api-key` (a curl sending neither `x-api-key` nor `Authorization` got a real
  `200`; the `mitm:false` mirror run `run_117d6f75` sending the same unauthenticated request got a
  `401 x-api-key header is required` — which is what pins the shape to exactly `x-api-key`, not
  just "some header").
- No static opt-in headers were needed for the run to succeed.
- This is already encoded in `internal/adapter/anthropic_wire.go`'s `anthropicWireRules["ANTHROPIC_API_KEY"]`
  and pinned by `TestAnthropicWireRulesGolden`.

**P2 — subscription baseline. NOT done — this is the blocking part.**
1. Get a real `CLAUDE_CODE_OAUTH_TOKEN` (`claude setup-token` on a machine with a browser and an
   active Pro/Max/Team/Enterprise subscription — prints the token, saves it nowhere, §6.14).
2. Put it alone in a scratch secrets file: `CLAUDE_CODE_OAUTH_TOKEN=<token>` (no `ANTHROPIC_API_KEY`
   alongside it — the exactly-one rule would otherwise pick the wrong one).
3. Run the `claude-code` image with `mitm: true` and **no `network.inject[]`** (nothing to inject
   yet — the point of this probe is to observe, not to configure translation):
   ```yaml
   # krayt-oauth-probe.yaml
   image: ghcr.io/418-cloud/krayt-agent-claude-code
   secrets: ./oauth-secrets.env
   network:
     mode: allowlist
     allow: [api.anthropic.com]   # widen this list if the run reports a blocked host — see P3/P4
     mitm: true
   agent:
     adapter: none                # deliberately bypass the claude-code adapter for this probe —
                                   # it doesn't yet know how to translate this shape, and forcing
                                   # it to try would fail the run before any traffic is observed
   ```
   ```sh
   KRAYT_PROXY_LOG_REQUESTS=1 krayt run --config krayt-oauth-probe.yaml --task ./task.md --repo .
   ```
   **`KRAYT_PROXY_LOG_REQUESTS=1` is mandatory for this probe, and an earlier version of these
   steps was wrong to omit it.** Without it the proxy logs only failures and policy denials, so a
   run in which everything *worked* leaves `proxy.log` **empty** — which is exactly what a first
   attempt at this probe produced (`run_f47f5066`: exit 0, MITM CA trusted, `claude -p` completed,
   0-byte `proxy.log`). The observation mode (`internal/proxy/observe.go`, §6.6) adds one line per
   request — request line, host, header names, query-parameter names, response status, never a
   value — and is what makes the proxy an instrument rather than just an enforcement point.
   Since `mitm: true` with no `inject:` for `CLAUDE_CODE_OAUTH_TOKEN` still ships it via
   `SecretsBundle` as normal (network.inject is opt-in), this run authenticates exactly like any
   other subscription-token run — the ONLY thing new here is that the traffic passes through the
   MITM proxy, which can now see it.
4. Record from `.krayt/runs/<id>/proxy.log` (host, path, header **names** — never values, per §6
   of `add-tls-mitm-credential-injection.md`) and, if a debug shell into the running container is
   available, `curl -v` against the same endpoint to get the exact request line:
   - Host(s) contacted (is it `api.anthropic.com`, same as the API-key run, or something else —
     e.g. a subscription-specific host?).
   - Path(s) contacted for the actual inference call (same path as an API-key `claude -p` run?).
   - The auth header name (`Authorization: Bearer …`? Still `x-api-key`? Something else?) and its
     VALUE'S SHAPE only (e.g. "Bearer-prefixed", "raw token") — never the value itself.
   - Any other headers present on the API-key run but absent here, or vice versa.

**P2 + P1b — RECORDED 2026-08-17. Two like-for-like runs, same task, same instrument
(`KRAYT_PROXY_LOG_HEADER_VALUES`): `run_b408545b` with a live `CLAUDE_CODE_OAUTH_TOKEN`,
`run_99bd261c` with a live `ANTHROPIC_API_KEY`. Both `mitm: true`, no `inject[]`, `adapter: none`.**

| | subscription token | API key |
|---|---|---|
| Host | `api.anthropic.com` | `api.anthropic.com` — **same** |
| Inference | `POST /v1/messages?beta=…` | `POST /v1/messages?beta=…` — **same path and query** |
| Auth header | `authorization: Bearer …`, `credential_len=108` | `x-api-key` |
| Pre-flight calls | `GET /api/claude_code/settings` (404), `/policy_limits` (200), with `anthropic-beta: oauth-2025-04-20` | the **same two calls**, with `x-api-key` and **no** `anthropic-beta` |
| `anthropic-beta` on inference | …`oauth-2025-04-20`…`extended-cache-ttl-2025-04-11` | …`context-1m-2025-08-07`…`fallback-credit-2026-06-01` |
| Response rate-limit headers | `anthropic-ratelimit-unified-5h-*`, `-7d-*`, `-overage-status` | `anthropic-ratelimit-requests-*`, `-tokens-*`, `-input-tokens-*` |

Everything the probe asked for, now answered:

- **P3 → PRIMARY design.** Same host, same path, same body shape; the difference is the auth header.
  No structural divergence, so the fallback (`RefreshRule` response rewriting) stays unimplemented.
- **The token is forwarded VERBATIM.** `credential_len=108` equals the secrets-file value's own
  length, so Claude Code does **not** exchange the long-lived `sk-ant-oat01-…` token for a
  short-lived one. This is the single fact the whole primary design rests on — the proxy holds
  everything the request needs.
- **The auth scheme is `Bearer ` (trailing space).** Observed, not assumed.
- **`oauth-2025-04-20` is the OAuth-only `anthropic-beta` item**, and it appears alone on the two
  `/api/claude_code/*` calls of the OAuth run while the API-key run sends no `anthropic-beta` there
  at all — which is what makes it look required for OAuth-token acceptance rather than incidental.
- **P4 → no refresh.** No token/refresh endpoint, no `401`, in either run. Consistent with §6.14's
  "long-lived token"; a deliberately long/idling run is still what would let "never refreshes" be
  asserted outright.
- **P5 → metering follows the credential.** The OAuth run draws on the unified 5h/7d subscription
  windows; the API-key run reports per-key request/token limits. §6.14's `(verify current)` marker on
  this point is answered by the header sets themselves.
- **Correction to an earlier reading of the P2-only run:** the `/api/claude_code/settings` +
  `/policy_limits` calls are **not** OAuth-specific. The API-key run makes exactly the same two
  calls. They are unconditional Claude Code startup calls, so shape translation does not have to
  suppress or account for them.

**Implemented from this recording (this branch):**
`anthropicWireRules["CLAUDE_CODE_OAUTH_TOKEN"]` = strip `x-api-key` + `authorization`, set
`authorization` = `"Bearer "` + the secret. `InjectRule.SetPrefix` carries the scheme (folded into
the value host-side, so `internal/proxy` still just sets a string). Under the 2026-08-18 shape-
mirroring decision the container is configured `CLAUDE_CODE_OAUTH_TOKEN=<placeholder>`, so Claude
Code emits `oauth-2025-04-20` and the rest of its beta list itself — krayt synthesizes nothing, and
the `AppendCSV` mechanism that existed only for that synthesis is gone.
`TestAnthropicInjectRuleForUnobservedShape` is gone too — `TestAnthropicWireRulesPassRealValidation`
now runs every table entry through `ValidateNetworkPolicy` instead, so a future probe cannot add an
entry the run pre-flight rejects.

**✅ VERIFIED END TO END — `run_df97fffa`, 2026-08-18** (harness:
`/tmp/claude/krayt-oauth-verify`, two runs). Every substantive claim held:

- **`200` on `POST /v1/messages`** with `sent=[… authorization=<scheme="Bearer" credential_len=108>]`
  — the real token, attached host-side, accepted by Anthropic.
- **The container never held it:** its own request carried
  `authorization=<scheme="Bearer" credential_len=28>` (a placeholder), `/run/secrets` did not exist,
  `secret-scan.json` was clean, and no `x-api-key` appeared anywhere — the container was
  OAuth-shaped, as shape mirroring intends.
- **The CLI composed `oauth-2025-04-20` itself**, on the request krayt received. krayt added nothing
  to `anthropic-beta`, which is the whole point of mirroring the shape.
- **Metering follows the injected credential:** the `200` carried
  `anthropic-ratelimit-unified-5h-*`/`-7d-*`, so a translated run bills the subscription, not API
  credits. That is P5 answered for the injected path, not just the direct one.
- **Control run `run_10fc027d`** (same task, same image, `mitm: false`): `/run/secrets/CLAUDE_CODE_OAUTH_TOKEN`
  present at 108 bytes, no placeholder, no injection. Without this, run 1's "no credential here"
  would have been unfalsifiable.

**Two things observed for the first time, both worth carrying forward:**

1. **Claude Code validates credential format on neither path.** The placeholder it accepted was the
   *entrypoint's* prefix-less `krayt-injected-at-host-proxy` (28 bytes), not krayt's
   `sk-ant-oat01-…` — because `:latest` predates the §8.2 entrypoint fix and substituted its own
   value. So the `sk-ant-` prefixes in `anthropicWireRules` are insurance, not a requirement.
2. **Claude Code scrubs its credential from child-process environments** (`CLAUDE_CODE_CHILD_SESSION=1`
   in the agent's own report). An agent running `env` inside the container cannot see the
   placeholder *however the run is configured* — so "no credential in env", reported by an agent, is
   not evidence in either direction. The entrypoint's startup line and the proxy's observation log
   are. Three of this harness's assertions initially pointed at `logs/console.log` (the VM serial
   console, which carries no agent output at all); one of them consequently passed vacuously. Fixed,
   and re-checked against the real artifacts of both runs.

- **Why the agent couldn't:** no live Anthropic credential of either kind in this sandbox, and no
  guessed header name or endpoint may ship — "an honestly-blocked handoff beats a plausible
  invention." Every fact in `anthropicWireRules` came from one of these runs.
- **Still open, and genuinely minor:** P4 can only be *strengthened*, not completed, by a short run —
  no refresh appeared in any of the four runs so far, but asserting "never refreshes" outright wants
  a deliberately long or idling one.
- **Verified:** `KRAYT_SPEC.md` §14's Phase 10 "Done when (hardware)" is ticked and
  `docs/ai-tasks/README.md`'s status cell updated, both citing these run ids.
- **Blocking:** nothing.

## [tooling/CI] vmimage RC/graduate workflows — ✅ DONE

Added `hack/next-vmimage-tag.sh`, `.github/workflows/vmimage-rc.yml`, and
`.github/workflows/vmimage-graduate.yml` (see `RELEASING.md` for the full flow). The
tag-computation logic was already verified locally (fabricated tag lists for rc→rc+1,
stable→next-patch-rc.1, and no-prior-tag, plus a real push round-trip against a scratch bare
repo); the three things that needed a real GitHub Actions run are now confirmed for real too:

1. **A real PR push triggers `vmimage-rc.yml` and publishes a working RC tag.** Confirmed: a PR
   touching a watched path (`images/**`, `internal/guest/**`, `cmd/krayt-agent/**`,
   `cmd/krayt-proxy/**`, `cmd/krayt-ask/**`) ran the workflow, computed the expected tag, and
   pushed it — and `image.yml`'s existing tag trigger picked it up and published.
2. **A real `vmimage-graduate.yml` dispatch re-tags the right commit and `image.yml` publishes it
   correctly.** Confirmed: run with a real `rc_tag` + `version`, the new clean tag pointed at the
   RC's exact commit (not `main`'s tip), and the published digest matched the already-tested RC's
   digest — the reproducibility expectation from `RELEASING.md` held.
3. **Concurrent PRs touching these paths behave as expected under the `vmimage-rc-tag`
   concurrency group.** Confirmed: two overlapping runs queued rather than raced (global group, no
   `cancel-in-progress`), as designed.

## [tooling] Build + first-run the new `edit-probe` image — ✅ DONE

Published multi-arch to `ghcr.io/418-cloud/krayt-probe:edit-probe` via `probe-images.yml`. The
first real run on hardware caught a genuine bug: the original entrypoint wrote an unrelated new
file (`EDITED_BY_KRAYT.txt`) instead of touching the repo's own content, so `TestConcurrentRealVMs`
could never see its per-run marker survive into `changes.patch` — it would have failed on every
run, regardless of whether VM isolation actually held. Fixed to append to the existing
`greeting.txt` instead, so the untouched marker line rides along as ordinary diff context.
Confirmed on an Apple-Silicon Mac after the fix: `TestEndToEndRealVM` and `TestConcurrentRealVMs`
both `--- PASS`.

## [tooling] Run `hack/run-integration-tests.sh` on an Apple-Silicon Mac (macOS/vfkit path) — ✅ DONE

Run end-to-end on real Apple-Silicon hardware: `TestBootHello`, `TestEndToEndRealVM`,
`TestEgressEnforcement`, `TestContainerHardening`, `TestRootImageFailsClosed`,
`TestGuestGitConfigInjectionInert`, `TestSecretConfinementInArtifacts`, and `TestConcurrentRealVMs`
all `--- PASS`; the script exited 0 with `==> Integration suite passed.` — confirms the script
correctly encodes the darwin/vfkit manual steps it replaces.

## [tooling/CI] First real run of the `integration-linux` CI job — ✅ DONE

Confirmed green on a GitHub-hosted Ubuntu runner: `/dev/kvm` is present (just not permissioned for
the runner user by default — worked around with a udev rule in `ci.yml` rather than group
membership, since a CI job never gets the fresh login session that normally requires), and the
full suite passes, `TestEgressEnforcement` included.

That last one surfaced a real bug along the way, not a CI-only quirk: any Linux host running both
Docker and krayt's firecracker backend silently drops all guest egress. `dockerd` sets the
netfilter `FORWARD` hook's policy to `DROP` at startup — a separate base chain from krayt's own
`krayt_fwd`, hooked at the same priority; nftables evaluates every base chain at a given hook
independently, and a `DROP` in any one of them is terminal regardless of what the others decide.
Fixed in `hack/linux-net-setup.sh` (an explicit accept in Docker's own `DOCKER-USER` chain, the
customization point Docker documents for exactly this) and surfaced in `krayt doctor`'s NAT check
so a host in this state doesn't look falsely green. Documented in the README's Linux prerequisites.

## [tooling] Build the `krayt-dev` image with the new `gh` CLI layer — ✅ DONE

The `gh` CLI install layer was added to `hack/krayt-dev/Dockerfile` (`ARG GH_CLI_VERSION=2.96.0`,
fetched as a `gh_<version>_linux_<TARGETARCH>.tar.gz` release tarball, same exception pattern as
`protoc`). Confirmed for real: CI (`.github/workflows/dev-image.yml`) built both `linux/amd64` and
`linux/arm64` on native runners, and `gh --version` runs correctly in the built image — exercised
directly by the real `fix-pr-review-comments` run below, which depends on `gh` working inside it.

## [GitHub] Confirm a read-only fine-grained PAT authenticates `gh` and reads PR review comments — ✅ DONE

`entrypoint.sh` runs `gh auth login --with-token < /run/secrets/GH_TOKEN` when `GH_TOKEN` is present
(non-fatal when absent). Verified with a real fine-grained PAT scoped to this repo with exactly
**Metadata + Contents + Pull requests: read** (no write):

- `gh auth login --with-token` succeeded with that token.
- `gh api "repos/{owner}/{repo}/pulls/<n>/comments"` returned the PR's real inline **review**
  comments.
- A write attempt (`gh api -X POST` / `gh pr comment`) was genuinely **refused by GitHub** —
  confirms the read-only design holds at the token level, not just by the task's own restraint.

## [GitHub] Real run of `docs/common-tasks/fix-pr-review-comments.md` against a real PR — ✅ DONE

Run via `krayt run` with live credentials against a real PR with real inline review comments.
Confirmed it: fetched the **review** comments (not just issue comments), triaged each against the
actual code, fixed genuine issues, left false positives untouched with a stated reason, wrote the
summary table + suggested commit message to `report.md`, and attempted **no** GitHub write.

## [tooling] `krayt upgrade` real-network download+swap smoke test — ✅ DONE

`krayt upgrade` (`internal/selfupdate` + `internal/cli/upgrade.go`) was fully unit-tested offline
against `httptest` fixtures from day one; what remained was the real end-to-end path this file
originally called out as unreachable from the cloud sandbox (no unallowlisted internet, and
linux/arm64 has no published release asset regardless). Run for real on darwin hardware
(`/Users/tjololo/.local/bin/krayt`, real `api.github.com` + release CDN, real installed binary),
covering everything the earlier sandbox pass couldn't:

- Interactive upgrade (no `--yes`): real TTY prompt `Upgrade? [y/N]`, answered `y` — download,
  checksum verification, extraction, and atomic swap all happened for real (0.7.0 → 0.7.1).
- Post-swap confirmation subprocess: immediately after the backup message, `krayt upgrade` itself
  printed the new binary's `version` output (`krayt 0.7.1` + vm-image digest) — confirms
  `exec.CommandContext(ctx, path, "version")` runs the freshly-swapped binary, not the old one.
- Backup + restore path: `krayt.bak` was created on every swap with the documented restore
  command printed (`cp .../krayt.bak .../krayt`).
- Downgrade path (`--version v0.6.0`-style, run here as `--version v0.7.0` against a 0.7.1
  install): correctly labeled `(downgrade)` in the prompt, and completed the same
  download/verify/swap sequence in reverse.
- `--check`: correctly reported `up to date` when current == target, and `upgrade available` when
  current < target, in both directions.
- `--yes`: skipped the interactive prompt and upgraded non-interactively as documented.
- Round-tripped 0.7.1 → 0.7.0 → 0.7.1 across four separate invocations, with `krayt version`
  after each swap matching the version just installed (including the correct pinned vm-image
  digest changing between 0.7.0 and 0.7.1) — real, observable confirmation, not something
  provable from the network-restricted sandbox this was originally logged from.

No gaps remain: this closes out every item the original entry listed as unverifiable (tarball
download, checksum verification, extraction, atomic swap, the post-swap confirmation subprocess,
and the `--yes`-free interactive prompt).

## [tooling/CI] Real compression ratio, CI time, and post-decompress boot for `rootfs.img` zstd compression — ✅ DONE

Ran for real via `workflow_dispatch` (`publish: true`, no tag) on commit `85f7446`, published as
`ghcr.io/418-cloud/krayt-vmimage:manual-85f74468c467-{arm64,amd64}`. Both arches pushed the new
`rootfs.img.zst` layer under `application/vnd.krayt.rootfs+zstd`, with `vmlinuz`/`initrd`
unchanged (`Exists`, reused from a prior push) — confirms the media-type/layer-shape half of the
change end-to-end against a real registry.

**Measured ratio and step time**, from the "Push OCI artifact + record digest" step's log
(`ls -la result/rootfs.img $stage/rootfs.img.zst` line + the step's own wall-clock duration):

| arch  | uncompressed  | `.zst`        | ratio  | zstd-reported | step wall time |
|-------|---------------|---------------|--------|----------------|----------------|
| arm64 | 2,317,037,568 B (2.16 GiB) | 464,153,046 B (443 MiB) | 4.99:1 | 20.03% | 2m 27s |
| amd64 | 2,178,670,592 B (2.03 GiB) | 483,812,248 B (461 MiB) | 4.50:1 | 22.21% | 4m 29s |

Ratio is well within the range that makes the `-19 -T0` choice (decision 3/5 in
`docs/ai-tasks/compress-vmimage-rootfs.md`) look justified — no need to revisit the `--long`/level
tradeoff based on this. Step time (compression + the `.zst` blob's registry upload, since the
other layers were already `Exists`) is a few minutes per arch, run in parallel across the matrix —
acceptable for a `publish` job that only runs on a tag push/dispatch, not on every PR.

**Boot confirmation**, on an Apple-Silicon Mac (vfkit), against the real published multi-arch
index (`internal/vmimage/pinned.go` updated to
`ghcr.io/418-cloud/krayt-vmimage@sha256:f831c8f1dff2f8c06a52e688fd62303048351fbb121694b16fadbcfd7ccb2501`,
which gathers the two per-arch manual pushes above):

- `krayt doctor` correctly reported the pinned digest **not cached** beforehand (proves it wasn't
  silently reusing an old plain-`rootfs.img` cache entry).
- `krayt image pull` verified the digest and produced a plain, decompressed
  `rootfs.img`/`vmlinuz`/`initrd` under `~/Library/Caches/krayt/vmimage/<digest>/` — confirms
  `vmimage.Pull`'s zstd-decompress-then-verify path works against a real registry artifact, not
  just the offline fixture.
- `krayt run` (with `--skip-resource-check`, unrelated to this change — just local free-memory
  headroom) booted the pulled image under vfkit for a real task: `echo "Write Hello to a
  greetings.txt file" | krayt run --task -` completed exit 0, `greetings.txt` containing `Hello`
  came back in `changes.patch`, and `report.md`'s provenance section shows the run's own commit
  (`85f74468c467`) matching the compression change itself. A corrupted/truncated decompression
  would have failed to boot or produced garbage here, not a silent success — this is the real
  round-trip the offline tests couldn't reach.

This closes out the item: offline unit tests, real CI ratio/timing, and a real pull+boot are all
now confirmed. No gaps remain.

## [tooling] Publish `krayt-agent-claude-code` — real workflow run + live onboarding run
- Needed:
  1. A real push to `main` (or `workflow_dispatch`) triggering `.github/workflows/agent-images.yml`,
     confirming both the `linux/amd64` and `linux/arm64` builds succeed, the merge job assembles a
     multi-arch manifest, and `ghcr.io/418-cloud/krayt-agent-claude-code` is pushed with `:latest`,
     `:sha-<short>`, and `:2.1.226` (the pinned CLI version) — plus a check that the GHCR package is
     publicly visible (matching the other published images, e.g. `krayt-dev`/`krayt-probe`).
  2. A live onboarding run exactly as the main README's quickstart shows, with a real
     `ANTHROPIC_API_KEY`, confirming a `changes.patch` and `/output/report.md` come back:
     ```sh
     krayt run --image ghcr.io/418-cloud/krayt-agent-claude-code --agent claude-code \
       --task ./task.md --repo . --secrets ./secrets.env --allow api.anthropic.com
     ```
- Why the agent can't: no `docker build`/push access and no live Anthropic credential in this
  environment; also can't confirm GHCR package visibility without a real push.
- Exact steps/commands: push this change (or `gh workflow run agent-images.yml`) and watch the
  run; then, on a Mac with `krayt` built and the base VM image pulled, create a scratch repo +
  `task.md` + `secrets.env` (one `ANTHROPIC_API_KEY`) and run the quickstart command above.
- Verify success by: `agent-images.yml` green with both arches in the manifest
  (`docker buildx imagetools inspect ghcr.io/418-cloud/krayt-agent-claude-code:latest` shows
  `linux/amd64,linux/arm64`); the GHCR package page loads without auth; `krayt ls` shows the run
  reaching `done` with `EXIT 0`; `.krayt/runs/<id>/changes.patch` applies cleanly and
  `report.md` contains Claude's summary.
- Blocking: no — the gemini-cli/opencode agent-image tasks depend on this task's *code* (the
  `agent-images.yml` scaffolding + README table) having landed, not on this verification.

## [tooling] Pin the `node:24-bookworm-slim` digest in `images/agents/gemini-cli/Dockerfile` — ✅ DONE

Pinned in `3e1fc3f` ("chore: pin node.js to 3638d9a", #104) — `images/agents/gemini-cli/Dockerfile`
now reads `FROM node:24-bookworm-slim@sha256:3638d9a6fe4030bd716be989438248074489337ba3275657f93595428be4fc03`,
via Renovate's `pinDigests` as option (a) below predicted. **Loose end:** the comment a few lines
above that `FROM` still says "NOT digest-pinned: resolving the current node:24-bookworm-slim
digest needs registry access" and now contradicts the line it describes — worth deleting next
time that file is touched.

<details><summary>Original entry (kept for the reasoning)</summary>

- Needed: `images/agents/gemini-cli/Dockerfile` currently reads `FROM node:24-bookworm-slim`
  (tag only, no `@sha256:...`) — every other base image in this repo is digest-pinned, and
  `renovate.json`'s `pinDigests: true` (for the `dockerfile` manager) expects one to maintain.
- Why the agent can't: this environment's egress proxy blocks `registry.npmjs.org`,
  `hub.docker.com`, `registry-1.docker.io`, `ghcr.io`, and GitHub release-asset downloads (all
  return `403`/connection-refused) — only `api.github.com` (via the authenticated `gh` CLI) is
  reachable, which has no route to a Docker registry's manifest digest. Inventing a
  plausible-looking `sha256:...` would silently break the build, which is worse than leaving it
  unpinned — see `CLAUDE.md`'s "never fabricate ... invented image digests."
- Exact steps/commands: either (a) merge Renovate's bootstrap PR — `pinDigests: true` makes
  Renovate open a PR adding the digest to this exact unpinned `FROM` line on its next run, no
  human digest-lookup needed, or (b) resolve it manually first:
  `docker buildx imagetools inspect node:24-bookworm-slim` (or `crane digest
  node:24-bookworm-slim`) and hand-edit the `FROM` line.
- Verify success by: `FROM node:24-bookworm-slim@sha256:<digest>` in the Dockerfile, and
  `agent-images.yml` still builds both arches successfully off it.
- Blocking: no — the tag alone still resolves to a real, current image; this only affects
  reproducibility/supply-chain pinning, not whether the image builds or runs.

</details>

## [BUG] `krayt-agent-gemini-cli` as published cannot complete any task — folder-trust gate

Found while running Phase 9's `NODE_EXTRA_CA_CERTS` check against the published image
(`run_fae09765`, **exit 55**, no task work performed):

```
Approval mode overridden to "default" because the current folder is not trusted.
Gemini CLI is not running in a trusted directory. To proceed, either use `--skip-trust`, set the
`GEMINI_CLI_TRUST_WORKSPACE=true` environment variable, or trust this directory in interactive mode.
```

Gemini CLI gates tool use on a "trusted folder" heuristic. In a headless run an untrusted folder
silently downgrades `--approval-mode yolo` back to `default` and then aborts — so the image runs,
authenticates, sets up the MITM CA correctly, and then does nothing. `entrypoint.sh` had no trust
handling at all.

- **Fixed in the repo:** `images/agents/gemini-cli/entrypoint.sh` now exports
  `GEMINI_CLI_TRUST_WORKSPACE=true` before invoking `gemini`. Chosen over the equivalent
  `--skip-trust` flag so an upstream flag rename cannot turn this into an argument-parsing
  failure. Trusting the folder is correct here, not just expedient: that prompt protects a
  developer's own machine from a freshly cloned repo, whereas krayt already assumes the repo is
  untrusted and puts the isolation boundary at the VM (§10).
- **Needs a rebuild to take effect** — `agent-images.yml` must republish the image before any run
  against `:latest` picks this up. Until then a config-side `env: {GEMINI_CLI_TRUST_WORKSPACE:
  "true"}` does the same job.
- **The fix itself is unverified.** Phase 9's Gemini verification (`run_e19488dd`) used the
  config-side override, not this entrypoint line — so the committed fix has never executed. After
  the republish, do one run **without** the config-side `env:` override: that, and only that,
  confirms the image works unaided. A run that still carries the override would pass either way
  and prove nothing about the fix.
- **Blocks two entries:** Phase 9's `NODE_EXTRA_CA_CERTS` clause (nothing Node-based can be
  exercised while the CLI refuses to run) and this image's own live-onboarding verification
  below, which would have failed identically.
- **Worth checking on the opencode image too** before its onboarding run — same class of
  headless-refusal gate, different CLI.

## [tooling] Publish `krayt-agent-gemini-cli` — real workflow run + live onboarding run
- Needed:
  1. A real push to `main` (or `workflow_dispatch`) triggering `.github/workflows/agent-images.yml`,
     confirming both the `linux/amd64` and `linux/arm64` builds succeed, the merge job assembles a
     multi-arch manifest, and `ghcr.io/418-cloud/krayt-agent-gemini-cli` is pushed with `:latest`,
     `:sha-<short>`, and `:0.55.1` (the pinned CLI version) — plus a check that the GHCR package is
     publicly visible (matching the other published images).
  2. A live onboarding run exactly as this image's README quickstart shows, with a real
     `GEMINI_API_KEY`, confirming a `changes.patch` and `/output/report.md` come back:
     ```sh
     krayt run --image ghcr.io/418-cloud/krayt-agent-gemini-cli --agent gemini-cli \
       --task ./task.md --repo . --secrets ./secrets.env --allow generativelanguage.googleapis.com
     ```
  3. One `--on-question=wait` run against the same image, prompting the task to ask a genuine
     question, confirming the `ask_human` MCP server (registered via the entrypoint's runtime
     `~/.gemini/settings.json` rewrite) actually surfaces the question to `krayt questions`/
     `krayt answer` and the run resumes — this is the one piece of MCP wiring that can't be
     verified without a real `gemini` process talking to a real socket.
- Why the agent can't: no `docker build`/push access, no live Gemini API key, and no way to
  drive a real `--on-question=wait` round-trip in this environment.
- Exact steps/commands: push this change (or `gh workflow run agent-images.yml`) and watch the
  run; then, on a Mac with `krayt` built and the base VM image pulled, create a scratch repo +
  `task.md` + `secrets.env` (one `GEMINI_API_KEY`) and run the quickstart command above; repeat
  with `--on-question=wait` and a task prompt that instructs the agent to ask a clarifying
  question before proceeding.
- Verify success by: `agent-images.yml` green with both arches in the manifest
  (`docker buildx imagetools inspect ghcr.io/418-cloud/krayt-agent-gemini-cli:latest` shows
  `linux/amd64,linux/arm64`); the GHCR package page loads without auth; `krayt ls` shows the run
  reaching `done` with `EXIT 0`; `.krayt/runs/<id>/changes.patch` applies cleanly and
  `report.md` contains Gemini's response; the `--on-question=wait` run shows `waiting` in
  `krayt ls`, `krayt questions <run-id>` lists the real question, and `krayt answer` resumes it
  to completion.
- Blocking: no — nothing downstream depends on this verification; the opencode agent-image task
  builds on the `agent-images.yml` scaffolding landing, not on this run happening.

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

## [tooling] Publish `krayt-agent-opencode` — real workflow run + live onboarding run
- Needed:
  1. A real push to `main` (or `workflow_dispatch`) triggering `.github/workflows/agent-images.yml`,
     confirming both the `linux/amd64` and `linux/arm64` builds succeed (including the models.dev
     catalog snapshot fetched at build time — confirm it actually lands at
     `/home/agent/.cache/opencode/models.json` in the built image and isn't empty/stale), the
     merge job assembles a multi-arch manifest, and `ghcr.io/418-cloud/krayt-agent-opencode` is
     pushed with `:latest`, `:sha-<short>`, and `:1.18.16` (the pinned CLI version) — plus a check
     that the GHCR package is publicly visible (matching the other published images).
  2. A live onboarding run exactly as this image's README quickstart shows, with a real
     `ANTHROPIC_API_KEY` (one provider is enough per the task), confirming a `changes.patch` and
     `/output/report.md` come back:
     ```sh
     krayt run --image ghcr.io/418-cloud/krayt-agent-opencode --agent opencode \
       --task ./task.md --repo . --secrets ./secrets.env --allow api.anthropic.com
     ```
     Also confirms the egress mitigation actually holds: with only `api.anthropic.com`
     allowlisted, opencode must NOT hit `models.opencode.ai` (the baked-in catalog snapshot +
     `OPENCODE_DISABLE_MODELS_FETCH=true` should make that unnecessary) — a run that stalls or
     fails on a blocked catalog fetch means that mitigation didn't actually work and needs
     revisiting.
  3. One `--on-question=wait` run against the same image, prompting the task to ask a genuine
     question, confirming the `ask_human` MCP server (registered via the entrypoint's runtime
     `OPENCODE_CONFIG` file pointing at a generated `mcp` block) actually surfaces the question to
     `krayt questions`/`krayt answer` and the run resumes — this is the one piece of MCP wiring
     that can't be verified without a real `opencode` process talking to a real socket, and the
     first time this image's specific MCP config shape (`type: "local"`, array `command`) has been
     exercised against a live opencode binary rather than just checked against docs.
  4. **The `NODE_EXTRA_CA_CERTS` check for this image (§14 Phase 9's last clause).** Phase 9 was
     closed with this one item deliberately re-homed here rather than left holding that phase
     open, since it is gated on this image existing, not on any MITM code. It is not optional:
     opencode is node-based, and Node does not read the system trust store at all, so if this
     image's entrypoint CA plumbing is wrong then **every** TLS call from it fails whenever
     `network.mitm` is on. Run it exactly as `krayt-agent-gemini-cli` was verified in
     `run_e19488dd` — `mitm: true`, `registry.npmjs.org` allowlisted, then:
     - `npm cache clean --force`, then a real `npm install` through the proxy — it must succeed,
       with `npm config get strict-ssl` confirmed `true` or the check proves nothing;
     - **the negative control**: cache clean again, then the same install with only
       `NODE_EXTRA_CA_CERTS` removed, which must **fail** `SELF_SIGNED_CERT_IN_CHAIN`. Without
       this arm a passing install shows only that Node trusted *something*. Clear the cache
       before **both** installs — a cached package installs with no network request at all, and
       skipping this is how the gemini run initially produced a false pass;
     - `openssl s_client -proxy 127.0.0.1:3128` against the registry, confirming the issuer is
       `krayt ephemeral MITM CA (run_…)`.
     Check first whether opencode has a headless-refusal gate like the one that blocked
     gemini-cli (see the `[BUG]` entry) — same class of problem, different CLI.
- Why the agent can't: no `docker build`/push access, no live provider API key, and no way to
  drive a real `--on-question=wait` round-trip in this environment.
- Exact steps/commands: push this change (or `gh workflow run agent-images.yml`) and watch the
  run; then, on a Mac with `krayt` built and the base VM image pulled, create a scratch repo +
  `task.md` + `secrets.env` (one `ANTHROPIC_API_KEY`) and run the quickstart command above; repeat
  with `--on-question=wait` and a task prompt that instructs the agent to ask a clarifying
  question before proceeding.
- Verify success by: `agent-images.yml` green with both arches in the manifest
  (`docker buildx imagetools inspect ghcr.io/418-cloud/krayt-agent-opencode:latest` shows
  `linux/amd64,linux/arm64`); the GHCR package page loads without auth; `krayt ls` shows the run
  reaching `done` with `EXIT 0`; `.krayt/runs/<id>/changes.patch` applies cleanly and
  `report.md` contains opencode's response; the `--on-question=wait` run shows `waiting` in
  `krayt ls`, `krayt questions <run-id>` lists the real question, and `krayt answer` resumes it
  to completion.
- Blocking: no — nothing downstream depends on this verification; the codex agent-image task
  builds on the `agent-images.yml` scaffolding landing, not on this run happening.

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
