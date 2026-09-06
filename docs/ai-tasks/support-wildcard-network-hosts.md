# Task: accept `*.example.com` wildcard hosts in krayt's network policy

**Read `CLAUDE.md`, `KRAYT_SPEC.md` §6.6, §8.1, §8.3, §10, and
`docs/ai-tasks/translate-network-policy-to-msb.md` (especially its decision 3, which this task
deliberately reverses) first.** Give a short plan — where the pattern matcher lives, which call
sites change, the test shape — and proceed.

This task is **pure validation and pure functions plus documentation**. It changes what krayt's
pre-flight accepts and how it cross-checks the three host lists against each other. It does not
change what krayt sends to msb beyond letting a new spelling through, and it introduces no new
enforcement mechanism. Everything except one hardware probe is testable offline.

## Background — an exact-only allowlist cannot name a per-tenant host

`internal/task/network.go:428`'s `validateHostEntry` permits only `a-z A-Z 0-9 . - :`. A `*` falls
into the generic `default:` branch, so `allow: ["*.example.com"]` fails pre-flight with

```
host "*.example.com" is not a bare hostname: write the host alone — letters, digits, '.', '-',
or an IPv6 literal — with no scheme, path or userinfo
```

— an error that never mentions wildcards, for a decision that was deliberate but is no longer
justified.

The consequence is that any host whose name is per-tenant or per-bucket is unreachable:
`<account>.blob.core.windows.net`, `<bucket>.s3.amazonaws.com`, `<project>.storage.googleapis.com`.
An operator's only option is to enumerate every subdomain in advance, which for object storage is
not possible.

**This repo's own `krayt.yaml:64` is the proof.** It lists a bare `'blob.core.windows.net'`. No
request host ever equals that string, so the entry matches nothing: the config reads as though
Azure storage were permitted while every request to it is denied. That is precisely the
silently-ineffective entry `validateHostEntry`'s own doc comment says the function exists to
refuse — and it slipped through because it is a perfectly well-formed exact hostname that simply
names nothing anyone connects to.

### Why decision 3 no longer holds

`translate-network-policy-to-msb.md` decision 3 says, in full:

> **krayt's allow list stays exact-host-only.** `internal/proxy`'s matcher is an exact folded-ASCII
> map lookup (`internal/proxy/proxy.go:515-521`) and `validateHostEntry` already rejects anything
> else. msb supports `*.example.com` suffix rules; do **not** expose that. Adding a wildcard
> vocabulary is a schema change with its own blast radius, and it is not what this task is for.

Both halves have expired:

1. **`internal/proxy` no longer exists.** `run-tasks-on-microsandbox.md` deleted it at the msb
   cut-over. The exact folded-ASCII map that the decision named as *the matcher* is gone; msb's
   network policy engine is the matcher now, and it supports domain suffixes natively. The
   decision's premise was a property of code that has since been removed.
2. **"not what this task is for"** was a scoping statement about `translate-network-policy-to-msb.md`,
   not a judgement that wildcards are wrong. This is the task it is for.

The blast radius the decision warned about is real and is what most of this task is about — see
"The three asymmetric cases" below. It is bounded, and it is bounded by work that must be done
explicitly rather than by refusing the feature.

## What msb actually supports — verified against v0.6.16 source

msb's `*.` shorthand works on **all three** flags krayt drives, and it is the *same* convention on
each by design (`crates/cli/lib/net_rule.rs:443-445`: *"Mirrors the syntax already used by
`--tls-bypass` and `--secret` so users see one wildcard convention across the CLI"*).

| `krayt.yaml` field | msb flag krayt emits | msb parses it as |
|---|---|---|
| `network.allow[]` | `--net-rule allow@*.example.com` | `Destination::DomainSuffix("example.com")` — `crates/cli/lib/net_rule.rs:443-449` |
| `network.passthrough[]` | `--tls-bypass *.example.com` | wildcard bypass pattern — `crates/network/lib/config/builder.rs:452-456` |
| `network.inject[].host` | `--secret NAME@*.example.com` | `HostPattern::Wildcard` — `crates/cli/lib/commands/common.rs:2619-2630` |

**Matching semantics are identical across all three: the apex domain itself, plus any
label-aligned subdomain.** `*.example.com` matches `example.com` and `api.example.com` and
`a.b.example.com`; it does **not** match `evilexample.com`. Three independent implementations in
msb agree on this, and a Go mirror has to match all three:

- `matches_suffix` — `crates/network/lib/policy/types.rs:997-1013` (net rules)
- `HostPattern::matches` — `packages/microsandbox-types/rust/lib/domain.rs:2325-2341` (secrets)
- `DomainPattern::matches_normalized` — `crates/network/lib/tls/state.rs:236-243` (TLS bypass)

### msb's own guards are uneven — this is the most important finding in this task

- **`--net-rule` is guarded.** Bare `*` is rejected (`net_rule.rs:833-838`: *"`*` alone is not a
  suffix shorthand"*), and a single-label suffix like `*.com` or `suffix=local` is rejected as
  `SuffixTooBroad` (`crates/network/lib/policy/name.rs:65-107`).
- **The secret surface is not guarded at all.** `HostPattern::parse`
  (`packages/microsandbox-types/rust/lib/domain.rs:2308-2318`) is three lines with no validation:

  ```rust
  pub fn parse(host: &str) -> Self {
      if host == "*" { HostPattern::Any }
      else if host.starts_with("*.") { HostPattern::Wildcard(host.to_string()) }
      else { HostPattern::Exact(host.to_string()) }
  }
  ```

  So `--secret TOKEN@*.com` is accepted silently, `--secret TOKEN@*.*.example.com` becomes a
  `Wildcard` that matches nothing (a credential that mysteriously never substitutes), and
  `--secret TOKEN@*` becomes `HostPattern::Any` — msb's own doc comment for that variant reads
  *"Any host (dangerous — secret can be exfiltrated)"*.

**krayt's pre-flight is therefore the only guard on the secret surface.** That fact is what makes
decision 1 below safe, and it is why the same strict validator must run on all three fields.

### The public-suffix gap

msb accepts any **two-label** suffix, so `*.co.uk`, `*.github.io`, `*.pages.dev` and
`*.blob.core.windows.net` all pass. msb has no public-suffix list and says so
(`net_rule.rs:792-795`: *"The PSL shortcoming (`*.co.uk`, `*.github.io` etc) is documented"*).
`*.github.io` therefore allows every GitHub Pages tenant; `*.s3.amazonaws.com` every S3 bucket.
Decision 3 below settles what krayt does about this.

### No new enforcement machinery

An allow-side domain rule requires a DNS-cache binding tying the connected IP to a hostname that
matches, before the rule can allow anything (`deferred_domain_match`,
`crates/network/lib/policy/types.rs:960-975`; the allow side is deliberately stricter than the deny
side so a guest cannot declare an arbitrary SNI on an unresolved IP). That is the *same* mechanism
krayt's existing exact-host allow rules already depend on — `DomainSuffix` and `Domain` go through
one function. Nothing about how a rule is enforced changes here; only which spellings krayt's
pre-flight will pass through to msb.

## Decisions already made (do not re-litigate)

1. **Wildcards are accepted in all three fields** — `network.allow`, `network.passthrough`, and
   `network.inject[].host` (secret scoping).

   Not allow-only: `network.passthrough` must be a subset of `network.allow`, so a wildcard-allowed
   storage host with no wildcard passthrough would be **MITM'd by msb's interception CA** the moment
   any secret is declared anywhere in the run. That breaks every client that pins or that uses its
   own trust store — exactly the reason this repo's `krayt.yaml` already passes `go`, `nix` and
   `git`/libcurl hosts through.

   Not exact-only for `inject` either, despite that being the field where the blast radius is a
   *credential* rather than a *connection*. The reason it is safe is the finding above: msb does
   not validate that surface at all, so krayt running **one identical strict validator across all
   three fields** is strictly better than today's split, where `inject` hosts go through
   `validateHostEntry` and would otherwise reach `HostPattern::parse` unchecked. Do not weaken the
   validator for `inject`, and do not let a wildcard secret host skip any check an allow entry gets.

2. **Mirror msb's strictest acceptance set, on every surface.** Reject bare `*` and single-label
   suffixes everywhere, including where msb itself would accept them. krayt is never more permissive
   than msb's guarded surface, and never relies on a guard msb only applies to one of the three.

3. **No krayt-side public-suffix list, in any form.** A label-count threshold cannot separate
   `*.blob.core.windows.net` (four labels, multi-tenant, the motivating case) from `*.example.com`
   (two labels, single-owner) — any threshold that blocks the dangerous shape blocks the use case
   that prompted this task. A curated denylist (`co.uk`, `github.io`, `pages.dev`, …) is stale by
   construction and buys false confidence; a real PSL would be a vendored, expiring data file inside
   a tool whose containment story is provenance (§8.3). And krayt being *stricter than msb in a way
   msb deliberately declined to be* needs its own justification, which there isn't one for.

   Document the gap instead (§6.6, §10, `configs/krayt.yaml`), and mitigate it where it is cheap: a
   wildcard line in the pre-boot policy print, so the operator's last-chance read shows the entry
   whose printed width most understates its breadth.

4. **§8.3 containment is unchanged — a wildcard `network.allow` entry stays acceptable from an
   auto-loaded repo-local `krayt.yaml`.** A wildcard adds breadth, not a capability class:

   - An auto-loaded config can already name `evil.attacker.example` as an exact allow entry. The
     exfiltration destination is already grantable; the wildcard only makes it shorter to write.
   - It cannot reach the credential boundary. Secret substitution hosts come from `network.inject`,
     which §8.3 **refuses** from an auto-loaded file. Allow is a gate, never a widener:
     `ValidateNetworkPolicyForMsb` requires secret hosts ⊆ allow, never the reverse, so widening
     allow adds zero substitution hosts.
   - It cannot turn off interception either — `network.passthrough` is likewise refused from an
     auto-loaded file.
   - Private/loopback/link-local/meta stay denied in every mode (`msbDenyGroups`,
     `internal/task/netpolicy_msb.go:12`), ordered before any allow, with msb's DNS-rebind
     protection on — so a wildcard suffix resolving into RFC1918 still cannot reach the host LAN.

   Refusing wildcards while honoring exact hosts would be an incoherent boundary: it blocks
   `*.example.com` while permitting the same repo to list the twenty subdomains it wanted. The
   residual — a one-line repo diff widening egress more than it reads — is a *review* problem,
   addressed by the print line in decision 3, not a containment problem. **Add no
   `rejectAutoLoadedPolicy` case**, and record this reasoning in §8.3 so it is a stated decision
   rather than an omission a later reader has to re-derive.

5. **`NetworkArgs` needs no behavioural change.** It already appends `"allow@"+h` and
   `"--tls-bypass", h` as separate argv elements, and krayt exec's msb with an argv slice and no
   shell, so `*` needs no quoting or escaping. Doc comment only.

## What to build

### `internal/task/network.go`

**`validateHostPattern(h string) error`** — new. Strips one leading `*.` and delegates everything
else to the existing `validateHostEntry`, so there is exactly one copy of the byte-class, label,
IPv6 and port rules. It is the entry point for `network.allow`, `network.passthrough`, and secret
hosts.

| input | outcome |
|---|---|
| `*.example.com`, `*.blob.core.windows.net`, `  *.EXAMPLE.com  ` | accept |
| `*.co.uk`, `*.github.io` | accept — deliberate, decision 3 |
| `*` | error: krayt has no "any host" wildcard. Say why it matters: msb reads `--secret NAME@*` as `HostPattern::Any` ("any host, dangerous") and krayt must never emit it |
| `*.` | error: a wildcard must name a suffix |
| `*.com`, `*.local` | error mirroring msb's `SuffixTooBroad` — a single-label suffix matches every domain under that TLD |
| `*.*.example.com` | error: one wildcard, and only as the leftmost label. (msb's secret surface would silently accept this as a `Wildcard` matching nothing) |
| `api*.example.com`, `*api.example.com`, `api.*.example.com` | error naming the leading-`*.`-only rule, not the generic "not a bare hostname" |
| `*.1.2.3.4`, `*.::1` | error: a wildcard names a domain suffix; an IP literal has no subdomains |
| `*.example.com.`, `*.exampİe.com`, `*.example.com:443`, `*.a/b`, `suffix=example.com`, `domain=example.com` | error via the delegated `validateHostEntry` rules (empty label, punycode, port, byte class) |

The last row matters for "translate, don't forward": `=` and `/` are already outside the byte
class, so msb's own `suffix=` and `domain=` spellings stay untranslatable in `krayt.yaml`. Keep it
that way.

**`validateHostEntry`** gains an explicit leading-`*.` branch so any caller that still wants
exact-only — `refresh.host` at line 329 — produces a diagnostic error instead of the generic one.

Its doc comment (lines 400-427) must be retargeted while you are in there. It currently frames the
whole function around the deleted `internal/proxy`'s `normalizeHost` ("It was the pre-flight half
of the pre-msb host proxy's normalizeHost…"), and its closing paragraph cites
`internal/orchestrator/egressproxy.go`, which no longer exists. Rewrite it against msb's matcher.
**Keep the comma-refusal rationale** — it is still load-bearing, just for a different consumer:
`internal/sandbox.SecretArgs` renders `--secret NAME@HOST[,HOST...]`, so a comma inside a host
would silently become two scoped hosts.

**Pattern helpers** — three functions, built on one primitive. Cite all three msb implementations
in the doc comment so drift is visible to the next reader:

```go
// hostCovers reports whether pattern (an exact host or "*.suffix") matches the concrete host.
func hostCovers(pattern, host string) bool

// coversPattern reports whether pattern a subsumes EVERY host pattern b could match.
// Directional — for the ⊆ checks.
func coversPattern(a, b string) bool

// patternsOverlap reports whether any single host could match both.
// Symmetric — for the conflict checks.
func patternsOverlap(a, b string) bool
```

plus slice wrappers (`anyCovers`, `anyOverlaps`). Use the existing ASCII-only `lower`
(`network.go:492`), not `strings.ToLower` — the U+0130 folding reason in its comment still applies.
The label-alignment boundary is what makes `evilexample.com` fail against `*.example.com`: match
`host == suffix` **or** `host` ending in `"." + suffix`, never a bare `strings.HasSuffix`.

**Rewire `ValidateNetworkPolicy`** (line 209):

| site | today | becomes |
|---|---|---|
| 216-225 shape checks | `validateHostEntry` | `validateHostPattern` |
| 231-236 `passthrough ⊆ allow` | `allow[lower(h)]` | `anyCovers(np.Allow, h)`, using **`coversPattern`** |
| 252-255 inject host ∈ passthrough | `passthrough[host]` | **`patternsOverlap`** against each passthrough entry |
| 256-258 inject host ∈ allow | `allow[host]` | `anyCovers(np.Allow, rule.Host)` |

`lowerSet` (503-509) loses all four of its call sites across the two files — delete it.

The `seenHost` duplicate check (247-250) stays **exact-string**. `*.example.com` and
`api.example.com` are two legitimately distinct rules, not a duplicate.

### `internal/task/netpolicy_msb.go`

- Line 196: `validateHostEntry` → `validateHostPattern` for secret hosts (decision 1).
- Lines 199-201 (`allow[lower(h)]`) → `anyCovers`; lines 207-210 (`passthrough[lower(h)]`) →
  `anyOverlaps`. Delete the `lowerSet` calls at 190-191.
- Extend the comment at 202-206 to explain why the passthrough check is *overlap*, not coverage.
- One line on `NetworkArgs`' doc comment (lines 14-34): a `*.`-prefixed entry becomes msb's
  `DomainSuffix` / `HostPattern::Wildcard`, the apex is included, and all three msb flags share one
  wildcard convention.

### The three asymmetric cases — where a naive implementation goes wrong

Swapping the exact map lookups for "does any allow entry cover this?" is not enough. The direction
matters, and it differs per check.

1. **allow `*.example.com` + passthrough `api.example.com` → VALID.**

   This is load-bearing beyond ergonomics. Adapter-supplied secret scopes are always **exact**
   hosts — `claudeCodeAPIHost = "api.anthropic.com"` (`internal/adapter/claudecode.go:9`, used at
   `:34`), `geminicli.go:27`, `opencode.go:35`. Without directional coverage, an operator writing
   `allow: ["*.anthropic.com"]` would break every `agent.adapter: claude-code` run at pre-flight,
   on a config that is obviously correct.

2. **allow `api.example.com` + passthrough `*.example.com` → ERROR.**

   The reverse direction is not symmetric and must not be treated as such. The message has to say
   *why*, not repeat today's "must also be in allow":

   > `network: passthrough host "*.example.com" is not covered by allow: allow names the single
   > host "api.example.com", but this passthrough exempts every subdomain of example.com from TLS
   > interception. The allowlist must be at least as wide as anything exempted from it — write
   > "*.example.com" in allow, or narrow passthrough to "api.example.com".`

   Keep the reasoning in a doc comment: `--tls-bypass` is emitted verbatim
   (`netpolicy_msb.go:73-75`) and msb's bypass matcher is independent of its net-rule matcher, so a
   bypass wider than the allowlist is a real mismatch between the policy krayt printed and the one
   msb enforces.

3. **passthrough `*.example.com` + a secret scoped to `api.example.com` → ERROR.**

   Only `patternsOverlap` catches this, and it is a genuinely silent failure today: a passthrough
   host is tunneled un-MITM'd, so the secret can never be substituted and the guest sends only
   msb's placeholder. The existing check at `netpolicy_msb.go:207-210` is an exact map lookup and
   misses it entirely.

   The message must **name the offending passthrough entry** — the operator wrote a host that
   appears nowhere in the passthrough list literally, so an error that only names the secret host
   is unactionable:

   > `network: inject (GH_TOKEN): host "api.example.com" is covered by passthrough entry
   > "*.example.com" — a passthrough host is tunneled un-MITM'd and can never receive secret
   > substitution; the guest would send only the placeholder.`

### `internal/cli/run.go`

- **`printNetworkPolicy`** (line 925): append a distinct line **only when a wildcard is present**,
  e.g. `  wildcard suffixes (every subdomain): *.blob.core.windows.net`. §8.3 describes this print
  as the operator's last chance to notice a host they did not choose, and a wildcard is exactly the
  entry whose printed width understates its breadth (decision 3's mitigation).
  `TestPrintNetworkPolicyFlagsOnly` (`internal/cli/run_test.go:334-344`) asserts an exact string for
  a wildcard-free policy, so the line must be conditional, not unconditional-and-empty.
- **`--allow` help text** (line 133): document the wildcard form **and the shell-quoting trap**.
  `krayt run --allow '*.example.com'` — unquoted, zsh fails the entire command with
  `no matches found`, which reads like a krayt bug rather than a globbing one.
- **No `rejectAutoLoadedPolicy` change** (decision 4). Leave `wellKnownAllowDomains` (line 172) and
  `completeAllowDomain` alone; teaching completion the wildcard form is out of scope.

### `krayt.yaml` (this repo)

Line 64: `- 'blob.core.windows.net'` → `- '*.blob.core.windows.net'`, with a comment naming the
account-scoped hostname it exists for. Add the matching entry to `passthrough:` so per-account
storage traffic is tunneled rather than intercepted, consistent with the other non-credential hosts
in that list.

This fixes the dead entry, dogfoods the feature, and activates the wildcard path in
`TestNetworkArgsThisRepoConfig` (`internal/task/netpolicy_msb_test.go:304`), which derives its
expected argv from the real file and so needs no code change of its own.

### Documentation

Per `CLAUDE.md`, the spec wins until amended — so amend it here rather than diverging.

- **`docs/ai-tasks/translate-network-policy-to-msb.md`, decision 3 (lines ~52-56)** — add a single
  `> **Superseded by `support-wildcard-network-hosts.md`** — …` line **above** the decision,
  naming both reasons its premise expired. Leave the original text intact. Do not rewrite history.
- **`KRAYT_SPEC.md` §6.6** — the `allowlist` bullet (~line 293), plus a new **"Wildcard suffix
  entries"** paragraph: the `*.` syntax, apex-inclusive label-aligned semantics, msb parity across
  all three flags, the two-label floor, and the **PSL gap** (`*.github.io` allows every GitHub
  Pages tenant, `*.s3.amazonaws.com` every bucket) with decision 3's reasoning for why krayt keeps
  no PSL of its own.
- **`KRAYT_SPEC.md` §8.1** — add the wildcard to the `network:` example (~line 1137). **Scoping
  note:** that block is *already* stale — it still shows the removed pre-msb `mitm: true` and the
  `inject[].strip`/`set`/`set_literal` shape that `SecretSpecsFromConfig` now hard-errors. Fixing
  that is not this task's job; add the wildcard example without expanding the staleness, and do not
  get drawn into rewriting the block.
- **`KRAYT_SPEC.md` §8.3** — the containment table's last row (~line 1450): name `network.allow`
  *(including `*.suffix` wildcard entries)* explicitly and carry one sentence of decision 4's
  reasoning, so this is a stated decision rather than an omission.
- **`KRAYT_SPEC.md` §10** — the Network egress row (~line 1723): one sentence that a wildcard grants
  a whole DNS subtree and that neither krayt nor msb can tell a registry suffix from an
  organization suffix.
- **`configs/krayt.yaml`** — a commented wildcard entry in `allow:` and `passthrough:`, with the PSL
  caveat and the quoting note.
- **`README.md`** — the network sections around lines 171-185 and 217-240.
- **Do not hand-edit `CHANGELOG.md`** — release-please manages it (`release-please-config.json`).

## Done when

- `go build ./...` (both `GOOS`), `go test -race ./...` and `golangci-lint run` are green.
- **`TestHostCovers`** pins apex-inclusive, label-aligned matching as a table: apex
  (`*.example.com` vs `example.com`) true; subdomain true; deep subdomain true; `evilexample.com`
  false; `example.com.evil.com` false; mixed case on both sides; exact-vs-exact equality; a suffix
  longer than the host; an IPv6 literal exact match.
- A test proves **`coversPattern` is directional**: `("*.example.com","api.example.com")` true,
  `("api.example.com","*.example.com")` false, `("*.example.com","*.api.example.com")` true,
  `("*.api.example.com","*.example.com")` false.
- A test proves **`patternsOverlap` is symmetric**: `*.a.com` vs `*.x.a.com` true, `*.a.com` vs
  `*.b.com` false, and both argument orders agree.
- **Each of the three asymmetric cases has its own named test**, and case 3's assertion checks that
  the error text **names the passthrough entry**, not just the secret host.
- A test asserts **bare `*` and `*.com` are rejected as secret hosts** — the case msb itself would
  not catch, and the reason decision 1 is safe. Both must also be rejected in `allow` and
  `passthrough`.
- A test asserts an **exact secret host validates under a wildcard allow** (`allow: ["*.anthropic.com"]`
  with an adapter-contributed `api.anthropic.com` scope) — the adapter-compatibility case from
  asymmetric case 1.
- A test asserts a **wildcard secret host validates under a wildcard allow**, so decision 1 is
  pinned rather than incidental.
- A **golden argv test** pins `--net-rule allow@*.blob.core.windows.net` byte-for-byte and in
  order, and `TestNetworkArgsHostsAreOwnArgvElements`
  (`internal/task/netpolicy_msb_test.go:276`) is extended with a wildcard allow **and** a wildcard
  passthrough — proving `*` is never quoted, escaped, or string-joined on the way to msb.
- **`TestNetworkArgsThisRepoConfig`** (line 304) passes against the updated `krayt.yaml`, with no
  change to the test itself.
- `TestPrintNetworkPolicyFlagsOnly` (`internal/cli/run_test.go:334`) still matches exactly, and a
  new test covers the wildcard line.
- `*.co.uk` is in the accepted set **with an inline comment naming the PSL gap**, so the acceptance
  is a pinned decision rather than an accident a later reader "fixes".
- The `bad` table in `internal/task/network_test.go` (212-252) gains named cases for every rejected
  spelling above — `*`, `*.`, `*.com`, `*.local`, `api*.example.com`, `*api.example.com`,
  `api.*.example.com`, `*.*.example.com`, `**.example.com`, `*.1.2.3.4`, `*.::1`,
  `*.example.com.`, `*.exampİe.com`, `*.example.com:443`, `*.example.com/v1`,
  `*.a.example,evil.example`, `suffix=example.com`, `domain=example.com`. A wildcard must never
  again be rejected only by the generic branch.

## Needs real hardware — `[HUMAN]`, route through `HUMAN_TODO.md`

Everything above is offline. Proving msb behaves the way its source reads is not: it needs an
Apple-Silicon Mac with msb ≥ 0.6.16.

Write **`hack/msb-probes/p9-wildcard-suffix-rules.sh`**, following the p3/p7/p8 convention (same
header, same measurement/RESULT output shape), plus its row in `hack/msb-probes/README.md`.
Measure:

1. `--net-rule allow@*.<suffix>` + a real request to a subdomain **succeeds**. This is the only way
   to show the deferred DNS-cache binding (`types.rs:960-975`) fires for a `DomainSuffix` exactly as
   it does for the exact-host rules krayt already relies on.
2. The **apex itself** is reachable under the same rule (the `hostname == suffix` half — the branch
   that regresses most easily and that a subdomain-only test would miss).
3. A **non-aligned neighbour** (`evil<suffix>`) is still denied.
4. `--tls-bypass *.<suffix>` genuinely skips interception for a subdomain — check the served
   certificate's issuer, mirroring p7's method.
5. `msb create --net-rule allow@*.com` is **rejected**. This pins that krayt's pre-flight is
   *aligned* with msb's guard rather than merely additive, so a future msb relaxation is noticed
   rather than silently inherited.

Then append the `HUMAN_TODO.md` entry (template in `KRAYT_SPEC.md` §14) and **stop there for those
five measurements**. Never write a result you did not measure — record the date and msb version in
the §6.6 paragraph the way §6.6 already records p3/p7/p8, and only once the probe has actually run.

## Out of scope

- Fixing §8.1's unrelated pre-msb staleness (`mitm: true`, the `strip`/`set` inject shape).
- A public-suffix list in any form — decision 3.
- `--allow` shell-completion suggestions for wildcards.
- msb's explicit `suffix=<name>` and `domain=<name>` spellings. "Translate, don't forward"
  (`translate-network-policy-to-msb.md` decision 1) still holds; `=` and `/` stay outside krayt's
  byte class, so those forms remain unwritable in `krayt.yaml` by construction.
- msb's `HostPattern::Any` / `allow_any_host_dangerous`. krayt has no vocabulary for it and must
  never emit one.
- Anything touching `NetworkArgs`' mode handling, the deny groups, `allow@dns` ordering, or
  `--on-secret-violation` — all settled by `translate-network-policy-to-msb.md` and unaffected here.
