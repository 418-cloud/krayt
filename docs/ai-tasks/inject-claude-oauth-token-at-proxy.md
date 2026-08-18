# Task: hide OAuth entirely — the proxy presents an API key to the container and speaks OAuth upstream

**Read `CLAUDE.md` and `KRAYT_SPEC.md` (§6.14 agent authentication, §6.8 secrets, §6.6 egress, §8.2
container contract, §10) first.**

**Depends on [`add-tls-mitm-credential-injection.md`](./add-tls-mitm-credential-injection.md) (step
2), which depends on [`move-egress-proxy-to-host.md`](./move-egress-proxy-to-host.md) (step 1). Do not
start until step 2 has landed with its hardware verification green.**

## Working mode: decide, don't ask

**Complete this task end-to-end without asking questions.** The design is settled, including the
branch: the P1–P5 probe *observations* decide between the primary and fallback designs, and both are
fully specified below. The agent chooses nothing and asks nothing — it reads the probe results and
implements the matching branch. This file **is** the approved plan, so `CLAUDE.md`'s "give a short
plan and wait for my OK before writing code" step is waived here; start implementing.

The extra maintenance cost — krayt tracking Anthropic's wire format — is already accepted; "Making the
treadmill cheap" is the agreed response to it. Do not re-raise it as a question. If something is
underspecified, pick the option most consistent with the stated design, record the choice and its
rationale in the commit/PR description, and keep going.

The only legitimate reasons to stop and involve a human:

- The P1–P5 probes, which need a real credential of each shape and so are a genuine `[HUMAN]` step.
  Write the probe procedure and recording template, append the `HUMAN_TODO.md` entry per §14, do every
  piece of work that does not depend on the outcome, and stop only when the remaining work genuinely
  hinges on the observations. That is a handoff with a filled-in template, **not** a question.
- This file is factually wrong about the codebase — a cited file, symbol, or line no longer exists, or
  step 2 landed differently than assumed. Say so, state the correction you made, proceed.

Do **not** ask for plan approval, for confirmation of a settled decision, or which branch to take.
Never fabricate a probe result, header name, or hardware result to avoid a handoff (`CLAUDE.md`) — an
honestly-blocked step is the correct outcome; a question is not.

## The rule this task implements

> **When `mitm` is on, the container never receives a real credential, and never learns what kind of
> credential is really in use.** It is configured with an API key that is not one; the proxy strips it
> and speaks whatever the provider actually wants.

This is a deliberate, accepted trade: krayt takes on a maintenance dependency on Anthropic's wire
format, and will need an update when that format changes. That cost is accepted — see
"Making the treadmill cheap" below, which is the engineering response to it.

## DEVIATION — shape mirroring supersedes the design below (owner decision, 2026-08-18)

**Read this before the design section.** The rule below — "the container is **always** configured
API-key-shaped, whichever credential the user supplied" — was implemented, then changed by the
owner after the P1–P3 probes came back. What ships instead:

> **The container is configured with a placeholder under the credential's OWN variable.** An
> `ANTHROPIC_API_KEY` secret gives the container a fake `ANTHROPIC_API_KEY`; a
> `CLAUDE_CODE_OAUTH_TOKEN` secret gives it a fake `CLAUDE_CODE_OAUTH_TOKEN`. The agent then runs
> its own code path for that shape, and the proxy substitutes exactly one header value.

Why the change — the probe data is what motivated it:

- **The two code paths differ by more than the auth header.** The subscription path sends
  `anthropic-beta: …,oauth-2025-04-20,…`; the API-key path sends `context-1m-2025-08-07` and
  `fallback-credit-2026-06-01` instead. Under the original design krayt would have to *synthesize*
  the OAuth request shape from the API-key one — appending the OAuth flag and hoping the API-key
  path's own flags are accepted alongside an OAuth credential — and re-do that analysis every time
  either path changes. Mirroring makes the CLI compose its own list, correctly, for free.
- **The hiding property the original design bought was never real.** "The container never learns
  what kind of credential is in use" fails on the response: a subscription's replies carry
  `anthropic-ratelimit-unified-5h-*`/`-7d-*` headers, an API key's carry
  `anthropic-ratelimit-requests-*`/`-tokens-*`. Keeping the secret would mean fabricating response
  headers, which is a worse trade than admitting the kind.
- **"It removes the refresh problem" is moot for env-var delivery.** A CLI configured via
  `CLAUDE_CODE_OAUTH_TOKEN` holds an access token and no refresh token — which is exactly why P4
  found no token endpoint contacted in either run. The refresh problem belongs to the interactive
  `/login` credential store, which krayt never uses.

What this file still governs, unchanged: the probe protocol (P1–P5) and its findings, the
"never guess a header shape" rule, the treadmill-cheap constraint (one dated, golden-tested vendor
file), and the fallback design should a future probe show structural divergence. What it no longer
governs: which env var the container's placeholder is delivered as, and the `--bare` / exactly-one
consequences that followed from the always-API-key rule.

## The design (settled — do not redesign)

```
  container                    host proxy                         upstream
  ─────────                    ──────────                         ────────
  ANTHROPIC_API_KEY=           strip: x-api-key, authorization
    sk-ant-krayt-              set:   <whatever the provider           real
    placeholder-do-not-use  ─►        actually wants, carrying   ─►  subscription
                                      the real OAuth token>           auth
```

The container is **always** configured API-key-shaped, whichever credential the user supplied:

| User's secrets file has | Container gets | Proxy sends upstream |
|---|---|---|
| `ANTHROPIC_API_KEY` | `ANTHROPIC_API_KEY=<placeholder>` | the real API key, API-key shape |
| `CLAUDE_CODE_OAUTH_TOKEN` | `ANTHROPIC_API_KEY=<placeholder>` | the real OAuth token, OAuth shape |

**This is what removes the refresh problem, and it is the main reason to do it this way.** API keys do
not refresh. A Claude Code configured with `ANTHROPIC_API_KEY` holds no refresh token, carries no OAuth
state, and never calls a token endpoint — so there is no refresh response to intercept and no vendor
response body to rewrite. Any refresh that OAuth requires happens host-side, in the proxy, invisibly.

Three further consequences worth stating in the docs:

- **The §6.14 exactly-one problem disappears inside the container**, because the container only ever
  sees one variable. The host-side exactly-one check over the user's secrets file stays, and its job
  narrows to "don't build an ambiguous injection rule".
- **`--bare` mode starts working with a subscription token.** §6.14 records that Bare mode "does not
  read `CLAUDE_CODE_OAUTH_TOKEN` at all". Under shape translation it never has to. Note the caveat is
  obsolete *when `mitm` is on*, and leave it standing for `mitm: false`.
- **The container's allowlist stays minimal.** Any refresh/token endpoint is dialed by the *proxy*,
  which is upstream of the allowlist entirely — so it never needs to appear in `network.allow`.

## The one thing that must be observed before coding

Everything above holds **if** Claude Code's API-key request path and its subscription request path
differ only in headers. If the subscription path uses a different host, path, or body shape, a
subscription token cannot authenticate an API-key-shaped request and header translation is not enough.

`KRAYT_SPEC.md` §6.14 already marks the relevant claims `(verify current)`, and this file is not a
source of truth for Anthropic's wire format. **Do not implement against assumptions, including any
written here.** Use step 2's own MITM proxy as the instrument, logging **request line, host, and header
names only — never bodies, never values** (step 2 §6):

**Every probe run below needs `KRAYT_PROXY_LOG_REQUESTS=1`** (§6.6,
`internal/proxy/observe.go`). Step 2 shipped the proxy as an *enforcement point*, whose log records
only failures and denials — so a successful probe run leaves `proxy.log` empty and proves nothing.
That variable turns on the per-request observation log the recordings below assume.

- **P1 — API-key baseline.** Run the `claude-code` image with a real `ANTHROPIC_API_KEY`, delivered the
  old way (`SecretsBundle`), under `mitm: true`. Record: hosts, paths, auth header name, any static
  opt-in headers.
- **P2 — subscription baseline.** Same run, same task, with a real `CLAUDE_CODE_OAUTH_TOKEN` instead.
  Record the same.
- **P3 — diff.** Compare.
- **P4 — refresh.** In the P2 run, note any request to a token/refresh endpoint: host, path, trigger,
  and whether the CLI persists the result inside the container.
- **P5 — billing.** Record whether headless `claude -p` on a subscription token draws API credits
  (§6.14 flags this `(verify current)`). Documentation only; it gates nothing.

**Both probes need a real credential of each kind, so this is a `[HUMAN]` step.** Write the probe
procedure and the recording template, append the `HUMAN_TODO.md` entry per §14, and stop there if you
cannot run it. An honestly-blocked task beats an invented header name.

### The branch — pre-decided, do not deliberate

- **If P3 shows headers-only differences → Primary design.** Implement exactly the table above. No
  response rewriting anywhere. This is the expected outcome.
- **If P3 shows the paths structurally diverge → Fallback design**, specified in full below. The user
  has pre-authorized the extra maintenance cost; implement it without asking.

## Fallback design (only if P3 forces it)

Container is configured OAuth-shaped with a placeholder, and the proxy additionally intercepts the
refresh exchange using step 2's declarative `refresh` block:

- Container gets `CLAUDE_CODE_OAUTH_TOKEN=<placeholder>`; the proxy strips and replaces it on the
  inference host as usual.
- A request matching `refresh.host` + `refresh.path_prefix` is handled host-side: the proxy performs
  the real exchange with the real credential, keeps the resulting token, and returns a response to the
  container in which every field named in `response_token_fields` has been replaced with the
  placeholder.
- **Parse minimally.** Decode the response body to a generic map, replace only the named fields,
  re-encode. Do **not** model the vendor's schema — that way an unrelated schema change passes through
  untouched instead of breaking the run.
- **Fail closed.** If the body is not JSON, or a named field is absent, return a 502 with a
  krayt-specific message. Never pass an unrewritten body through: that is the exact failure that puts
  a live token back inside the container.
- Everything else (placeholder handling, adapter-supplied rules, reporting) is identical to the
  primary design.

## Making the treadmill cheap

Since krayt is now on the hook for Anthropic's wire format, the cost of each future break must be
"edit one table and one test", not "re-understand the proxy".

- **All vendor-specific facts live in one file**: `internal/adapter/anthropic_wire.go`. §6.14 already
  mandates this boundary — "Everything agent-specific (which env var a credential maps to…) lives in
  the optional per-agent adapter… **not** the core."
- That file holds a small declarative table only: per credential shape, the headers to strip, the
  headers to set, any static opt-in headers, and (fallback only) the `refresh` block. **No logic.**
- Head the file with a comment recording *when* the shape was last observed and by which probe, so the
  next reader knows how stale it is.
- A golden test asserts the exact rule set each credential shape produces. When Anthropic changes
  something, that test fails, and its diff *is* the changelog of what changed.
- `internal/proxy` stays generic: it executes strip/set/refresh rules and knows nothing about
  Anthropic. Verify this by grepping — the string `anthropic` must not appear anywhere under
  `internal/proxy`.

## Implement

### 1. `internal/adapter/anthropic_wire.go` — the vendor table

Per the P1/P2 findings, a table from credential shape → wire rules. Nothing else in the repo may
encode a header name or endpoint for Anthropic.

### 2. Adapter produces the injection rule

`internal/adapter/adapter.go` — extend the types:

```go
type Plan struct {
    Env          map[string]string
    Credential   string
    Inject       []InjectRule      // host-side injection the orchestrator applies
    Placeholders map[string]string // container env standing in for host-only secrets
}

type InjectRule struct {
    Host       string
    Strip      []string          // headers removed from the guest's request
    Set        map[string]string // header name -> secrets-file key (resolved host-side)
    SetLiteral map[string]string // header name -> fixed non-secret value
    Refresh    *RefreshRule      // fallback design only; nil otherwise
}
```

Extend `Input` with whether `network.mitm` is on. With it off, `Prepare` returns an empty `Inject` and
today's behavior, byte for byte.

`claudecode.go`:

- Select the credential with the existing `exactlyOne` check (`claudecode.go:18`) — unchanged in
  substance. Add a test proving it still fires when the credential is host-only, and update the
  comment at `:3-8` to say the check now prevents an ambiguous *injection rule* rather than silent
  mis-billing.
- Emit the rule from the §1 table for the selected shape.
- Emit `Placeholders{"ANTHROPIC_API_KEY": placeholder}` — see §3.
- Drop the `--bare` + `CLAUDE_CODE_OAUTH_TOKEN` refusal **when `mitm` is on** (it no longer applies);
  keep it otherwise.

### 3. The placeholder — nailed down

`ANTHROPIC_API_KEY=sk-ant-krayt-placeholder-do-not-use`

- The `sk-ant-` prefix survives any client-side format check; the rest is self-describing so a human
  who finds it in a log knows immediately it is not a credential.
- Defined as one exported constant in `anthropic_wire.go`. If P1/P2 reveal a stricter format check,
  extend it minimally and **document in §10 what shape was required and why** — a placeholder forced
  to look like a real key is a finding, because someone will eventually mistake it for one.
- Placeholders are **not secrets**: plain env map, never `SecretsBundle`, never added to the redactor
  set. Redacting them would hide the very evidence that the run was credential-free.

### 4. Merge, validate, deliver

- Union the adapter's `Inject` with any user-written `network.inject`. On conflict (same host + header)
  the **user's explicit config wins** and the run logs the override.
- Re-run step 2's pre-flight validation over the merged set, so an adapter rule is held to exactly the
  same standard as a hand-written one.
- The merged set is serialized into the proxy child's stdin config (step 2 §2b). Adapter-supplied
  rules travel the same path as user-supplied ones; there is no second channel.
- `report.md`/`meta.json`: record the credential **key name**, that it was injected host-side, and
  which shape was translated. This is the user-visible proof the container ran credential-free.

### 5. Other agents

The rule at the top of this file is universal — with `mitm` on, no container gets a real credential.
But the *mechanism* is per-adapter and each one needs its own P1/P2 observation.

- `geminicli.go` and `opencode.go`: implement the same shape translation **only** if you have observed
  their wire format with the same confidence. Otherwise return no `Inject`, which degrades to today's
  `SecretsBundle` delivery — correct and safe, just not yet improved.
- **Never infer one vendor's header format from another's.**

## Tests

**Offline:**

- Golden test on the §1 table: each credential shape produces exactly the expected rule set.
- `grep -r anthropic internal/proxy` finds nothing.
- Adapter: with `mitm` on, an `ANTHROPIC_API_KEY` secret and a `CLAUDE_CODE_OAUTH_TOKEN` secret each
  produce a rule whose container-facing placeholder is the **same** `ANTHROPIC_API_KEY` value — the
  container cannot tell them apart. Assert that equivalence directly; it is the whole thesis.
- With `mitm` off, `Inject` is empty and behavior is byte-identical to today.
- `exactlyOne` still fires for a host-only credential; `--bare` + OAuth refused with `mitm` off,
  allowed with it on.
- Merge precedence; merged set re-validated; an adapter rule naming an unallowlisted host fails
  pre-flight in `allowlist` mode.
- Orchestrator: `SecretsBundle` omits the credential when injection is active.
- Proxy: a request bearing the placeholder leaves with the real value in the translated header; the
  placeholder never reaches upstream; the stripped header never reaches upstream.
- Fallback only: a refresh response has its token fields replaced with the placeholder; a non-JSON
  body and a missing field each produce a 502 and never forward the real token.

**On hardware (`[HUMAN]` — needs a real subscription token; write the tests, then hand off):**

- Full `claude -p` run on a real subscription token with `mitm: true`: the task completes, and `env`
  plus `/run/secrets` inside the container contain **no** token and **no** OAuth artifact. Assert the
  absence.
- The same task with a real API key under `mitm: true` still works (step 2 regression).
- Both still work with `mitm: false` via `SecretsBundle` (regression).
- `--bare` with a subscription token under `mitm: true` now works.
- Record the P5 billing observation.

```sh
go build ./...
GOOS=linux GOARCH=arm64 go build ./...
go test -race ./...
golangci-lint run
```

## Docs (required)

- **§6.14** — the substantial one. Add shape translation as the delivery mode when `mitm` is on, with
  the two-row table. Resolve the `(verify current)` markers P1–P5 answer. State that the container is
  always API-key-shaped and cannot observe which credential is really in use. Mark the `--bare`
  caveat obsolete under `mitm`. Update "Recommended default": injection means a subscription token no
  longer outlives the run, so the blast-radius argument for preferring an API key **softens** — but it
  does **not** disappear, because a compromised agent can still spend the seat's quota for the
  duration of the run. Say both halves.
- **§8.1** — adapter-supplied injection and merge precedence.
- **§10** — update the "Auth-credential blast radius" residual; add the placeholder-shape note from §3
  if a realistic-looking placeholder was required; record the accepted maintenance dependency on
  Anthropic's wire format as a known operational risk.
- **`README.md`** — subscription-token quickstart under injection.
- **`docs/ai-tasks/README.md`** — status.

## Done when

- A real subscription-token run completes with no token and no OAuth artifact anywhere in the
  container — verified on hardware, not inferred host-side.
- The container is configured identically for both credential shapes; a test asserts the equivalence.
- Every Anthropic-specific fact lives in `internal/adapter/anthropic_wire.go`, with a dated
  provenance comment and a golden test; `internal/proxy` contains no vendor identifiers.
- The user configures nothing beyond `mitm: true` and the secret itself.
- P1–P5 are recorded, and §6.14's `(verify current)` markers they cover are resolved or explicitly
  re-affirmed as still unverified.
- `mitm: false` runs are unchanged.

## Constraints

- **No guessed vendor wire formats.** Every header name and endpoint in shipped code traces to a
  logged P1/P2 observation. An honestly-blocked handoff beats a plausible invention (`CLAUDE.md`, §14).
- No new dependencies.
- Do not implement the fallback design unless P3 forces it; do not implement the primary design if it
  does.
- Do not extend translation to `gemini-cli` or `opencode` by inference from Claude Code.
- Placeholders never enter `SecretsBundle` or the redactor set.
- `internal/adapter` stays host-side and pre-flight — it must still fail fast before the image is
  pulled.
