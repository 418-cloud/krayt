# Task: translate krayt's network policy into msb flags — fail-closed, never implicit

**Read `CLAUDE.md`, `docs/adr-microsandbox-sandbox-layer.md` ("Default posture: what a bare sandbox
gets", "Config surface under B1"), and `KRAYT_SPEC.md` §6.6, §8.1, §8.3, §10 first.** Give a short
plan (where the translator lives, the mode table, the test shape) and proceed. Depends on
`add-msb-sandbox-driver.md` landing first; nothing here needs the hardware probes.

This task is **pure translation and pure functions**. It produces the argv a run will use; it does
not run anything. That keeps every rule below testable offline, which matters more here than
anywhere else in the arc — this is the file that decides whether a sandbox has network access it
should not.

## Background — msb's defaults fail open where krayt's fail closed

krayt's design principle 4 is *"Default-deny. Network egress, secrets, and host access are all
opt-in per task"* and its `network.mode` defaults to `allowlist`. msb's baseline is the opposite,
and the ADR's description of it is directionally right but **understates the trap**. Verified
against msb 0.6.16 source:

- `NetworkPolicy::default()` is `from_profiles([Public])` — `default_egress: Deny`,
  `default_ingress: Allow`, with rules `[allow_dns, allow@public]`
  (`crates/network/lib/policy/types.rs:724-728`, `:295-330`). That is the policy a sandbox gets with
  no network flags at all: **the whole public internet.**
- The ADR says the implicit `allow@public` applies "only when no other rules are present". It is
  worse than that. The CLI branches on `replaces_configured_policy`, which is true only when one of
  `--net`, `--no-net`, `--net-default`, `--net-default-egress` or `--net-default-ingress` is
  present (`crates/cli/lib/commands/common.rs:2088-2104`). **`--net-rule` alone does not qualify** —
  it takes the `prepend_network_policy_rules` branch, which prepends krayt's rules *on top of the
  default allow-public policy*. So `msb create --net-rule "allow@api.anthropic.com" …` grants
  `api.anthropic.com` **plus the entire public internet**, silently.
- Conversely, once a `--net-default*` flag *is* present, `build_network_policy` returns a policy
  whose `rules` are **only** the ones krayt passed (`common.rs:2494-2512`). The profile path is the
  only thing that injects `Rule::allow_dns()` (`policy/types.rs:313-315`). So
  `--net-default deny --net-rule "allow@api.anthropic.com"` gives a guest that **cannot resolve any
  hostname**, because DNS is denied along with everything else.

Both failures are silent in opposite directions. Hence the single design rule this task exists to
enforce: **krayt always emits a complete, explicit network policy — a default action, every allow
rule, an explicit DNS decision, and explicit denies for the private ranges — and never relies on any
msb default.** "No network policy computed" is a pre-flight error, not a valid state.

## Decisions already made (do not re-litigate)

1. **Translate, don't forward.** `krayt.yaml` keeps its own vocabulary; msb's schema does not leak
   into it. §8.3's containment rule (refusing security-relevant keys from an auto-loaded repo-local
   config) only works over a closed set of keys krayt models. The one bounded escape hatch is
   `sandbox.extra_conf`, which is `add-msb-extra-conf-escape-hatch.md`, not this task.
2. **`network.mitm` is a removed key and must hard-error**, naming itself and its replacement. It
   cannot be silently ignored: today it gates TLS termination, and a config that sets it is a config
   whose author is reasoning about interception. Under B1 krayt decides interception itself (see
   rule 5 below), so the key has no meaning — and quietly dropping a security key is how a policy
   regression ships. Same treatment for `inject[].set`, `inject[].set_prefix` and `inject[].strip`,
   handled in `hand-secrets-to-msb.md`.
3. **krayt's allow list stays exact-host-only.** `internal/proxy`'s matcher is an exact folded-ASCII
   map lookup (`internal/proxy/proxy.go:515-521`) and `validateHostEntry` already rejects anything
   else. msb supports `*.example.com` suffix rules; do **not** expose that. Adding a wildcard
   vocabulary is a schema change with its own blast radius, and it is not what this task is for.
4. **Private/loopback/link-local/metadata stay blocked in every mode**, including `full`. This is
   an existing krayt property, not a new one: `move-egress-proxy-to-host.md` deleted the `full`
   carve-out deliberately, because once the dialer is on the host an SSRF to `127.0.0.1` means
   something much worse. msb's destination groups (`private`, `loopback`, `link-local`, `meta`,
   `multicast`, `host`) express this directly; emit explicit `deny@` rules for them, ordered before
   the allow rules, in **every** mode.
5. **`--tls-intercept` is emitted whenever any secret is declared, and only then.** msb's own docs
   claim declaring a secret enables interception (`docs/cli/configuration.mdx:320`); **the CLI flag
   path does not do this.** `TlsConfig.enabled` is `#[serde(default)] bool` — false
   (`packages/microsandbox-types/rust/lib/domain.rs:2408-2411`) — and the `has_tls` predicate that is
   the only thing setting it lists every `--tls-*` flag and omits `opts.secret` entirely
   (`crates/cli/lib/commands/common.rs:2198-2208`). Since `require_tls_identity` defaults true and
   the handler skips such secrets on non-intercepted connections
   (`crates/network/lib/secrets/handler.rs:876`), a secret declared without `--tls-intercept` is
   **never substituted, silently** — the API just sees a garbage credential. `probe-microsandbox-feasibility.md`
   P3 confirms this on hardware; the translator must not depend on the probe's outcome to be correct.
6. **Ingress is denied explicitly.** msb's ingress default is `allow`, to preserve unfiltered
   published-port behaviour. krayt publishes no ports, so the setting is inert today and wrong the
   moment anything does. Set it now, while it costs one flag.
7. **The guest gets DNS, and that is a deliberate change.** Since Phase 8 krayt's guest has had no
   usable network at all in `allowlist`/`none` — everything rode vsock to the host proxy. Under msb
   the guest has a real (policed) interface, so `allowlist` mode must emit `--net-rule "allow@dns"`
   or nothing resolves. It is policed by msb's gateway with DNS-rebind protection on by default;
   record it in §6.6 as a capability the guest gains, rather than letting a reader discover it.

## The mapping

| `krayt.yaml` | msb flags |
|---|---|
| `network.mode: allowlist` (default) | `--net-default deny`, then `--net-rule "deny@<each private group>"`, `--net-rule "allow@dns"`, and one `--net-rule "allow@<host>"` per `network.allow` entry |
| `network.mode: full` | `--net-default-egress allow --net-default-ingress deny`, plus the same explicit `deny@<private group>` rules first — `allow` must not mean "and also the host's LAN" |
| `network.mode: none` | `--no-net` and **no `--net-rule` at all**. `--net none` is *not* the right flag: `build_network_policy` still attaches any supplied rules to `NetworkPolicy::none()` (`common.rs:2459-2465`), so a stray rule would punch through the mode that means "no network" |
| `network.allow[]` | `allow@<host>` rule tokens, exact domains |
| `network.passthrough[]` | `--tls-bypass <host>` |
| `network.mitm` | **removed — hard error** |
| any secret declared | `--tls-intercept` (rule 5) |
| — | `--on-secret-violation block-and-log`, set explicitly rather than inherited |

**Rule ordering matters and must be deterministic.** msb evaluates rules first-match-wins within a
direction, and explicit `--net-rule` entries are evaluated before profile-generated ones. Emit
denies before allows, and emit allows in the order `network.allow` lists them, so the same config
always produces byte-identical argv — that is what makes the golden tests below meaningful.

**Quoting.** `--net-rule` tokens contain `@`, `:` and `,`, all shell-significant. krayt never goes
through a shell (`exec.Cmd` with an argv slice), so no quoting is needed — but assert in a test that
each token is one argv element and that nothing ever string-joins them, because that is exactly the
mistake a later refactor makes.

**One conflict rule to design around.** `--net-rule`, `--net`, `--net-default-egress` and
`--net-default-ingress` all carry clap's `conflicts_with = "net_conf"` (`common.rs:401-436`).
Network policy is flags **xor** a scoped `--net-conf` file — mixing them is rejected. `--secret` has
no such conflict with `--secret-conf`, so those two may be combined. `add-msb-extra-conf-escape-hatch.md`
depends on this distinction; note it where the translator emits network flags so nobody later adds a
`--net-conf` beside the flags and gets a hard clap error at runtime.

**Config precedence is a security decision.** msb's config sources overlay left to right, and
network policy is **atomic**: a higher-precedence source supplying any of `policy`, `allow` or `deny`
replaces the *complete* lower-layer policy rather than merging (`docs/cli/configuration.mdx:99`).
Secrets, by contrast, merge by environment-variable name, so a later file can silently redefine a
secret's allow list. krayt must therefore pass its own security-relevant configuration **last**, and
refuse overlapping keys outright rather than trusting ordering alone.

## What to build

- `internal/sandbox/netpolicy.go` (or `internal/task`, if you judge the translation belongs beside
  the vocabulary it translates — argue it in your plan, but keep it a pure function either way):
  ```go
  // NetworkArgs renders a fully explicit msb network policy for np. It never returns an
  // empty policy: a policy that computed to nothing is an error, not a permissive default.
  func NetworkArgs(np task.NetworkPolicy, hasSecrets bool) ([]string, error)
  ```
- `task.ValidateNetworkPolicyForMsb(np)` — a **new, not-yet-called** function carrying the
  removed-key errors of decision 2. Do **not** change `ValidateNetworkPolicy`
  (`internal/task/network.go:209`): the vfkit/Firecracker path is still live at this point in the
  arc and *requires* `network.mitm: true` to inject anything, so making `mitm` an error here would
  break every existing run — including this repo's own `krayt.yaml`. The cut-over task swaps the
  call site. Keep the existing shape checks (`validateHostEntry`, the allowlist/passthrough subset
  rule) — they are unchanged by msb and still correct, so the new function should reuse them.
- Amend `KRAYT_SPEC.md` §6.6 and §8.1 **additively**: describe the msb mapping, the DNS change, the
  ingress denial and the "never emit an empty policy" rule as the model krayt is moving to, without
  yet deleting the statements about `internal/proxy` that are still true of the running code. The
  cut-over task rewrites those. Per `CLAUDE.md` the spec wins until amended — so amend it here
  rather than diverging, but do not describe as current a behaviour nothing yet performs.

## Sequencing — additive only

**Nothing in this task is wired into `krayt run`.** The vfkit and Firecracker path is still the only
path that executes, and it must keep working byte-for-byte until `run-tasks-on-microsandbox.md`
flips the switch and deletes it in the same change. Concretely: add functions and tests, change no
call site, delete no file. If you find yourself needing to delete `internal/proxy` to make something
compile, you have wired something in too early.

## Done when

- `go build ./...` (both `GOOS`), `go test -race ./...` and `golangci-lint run` are green.
- Golden tests pin the exact argv for each of the three modes, including this repo's own
  `krayt.yaml` allow list, so a reviewer can read the flags a real run would get.
- A test asserts **`NetworkArgs` never returns a slice without a `--net-default*` or `--no-net`
  flag**, for every input including the zero value. This is the single most important test in the
  task: it is what makes the `--net-rule`-alone trap unreachable.
- A test asserts `mode: none` emits `--no-net` and **zero** `--net-rule` flags.
- A test asserts the private/loopback/link-local/meta deny rules are present in `full` mode and
  precede any allow rule.
- A test asserts `--tls-intercept` appears iff `hasSecrets`, and `--on-secret-violation` is always
  explicit.
- `ValidateNetworkPolicyForMsb` rejects a policy carrying `network.mitm` with an error naming the
  key and its replacement, while the existing `ValidateNetworkPolicy` still accepts it — proven by
  a test asserting both, so the deferred activation is a property the suite holds, not a comment.

## Out of scope

- Actually creating a sandbox — `run-tasks-on-microsandbox.md`.
- Secret values, `--secret` flags, placeholders, adapters — `hand-secrets-to-msb.md` (this task only
  needs to know *whether* any secret exists, for `--tls-intercept`).
- `sandbox.extra_conf`, published ports, rate limits, DNS nameserver overrides, CPU placement.
- Supplying krayt's own interception CA. `--tls-intercept-ca-cert`/`--tls-intercept-ca-key` exist and
  are validated as a pair (`crates/network/lib/config/builder.rs:369-373`), but the ADR classes the
  shared on-disk CA as an accepted residual and it was deliberately left out of this arc.
