# Task: hand secrets to msb — env-reference channel, `--secret` scoping, and the adapter rework

**Read `CLAUDE.md`, `docs/adr-microsandbox-sandbox-layer.md` ("The secret-handling contract",
"Handing secrets over", "How a run would flow under B1"), and `KRAYT_SPEC.md` §6.6.1, §6.8, §6.14,
§8.1, §10 first.** Give a short plan (the new types, the child-env mechanism, how adapters opt in)
and proceed. Depends on `add-msb-sandbox-driver.md`; benefits from
`probe-microsandbox-feasibility.md` P5 but is not blocked by it.

## Sequencing — additive only

**Nothing here is wired into `krayt run`.** The Phase 9/10 host-proxy injection path is still the
only live one and must keep working byte-for-byte until `run-tasks-on-microsandbox.md` flips the
switch. Add types, functions, an opt-in adapter branch, and tests. **Delete nothing** — in
particular not `internal/adapter/anthropic_wire.go`, which the live MITM path still calls. If
something will not compile without a deletion, you have wired it in too early.

## Background — the three channels, and only one carries a value

msb never accepts a credential on argv. `--secret` takes `NAME@HOST[,HOST...]` and the inline
`NAME=VALUE@HOST` form is **rejected by msb itself** on both `create` and `modify`, precisely
because shell history and process listings would leak it
(`crates/cli/lib/commands/common.rs:548-562`). So the hand-off is three channels and exactly one
carries secret material:

| krayt input | Channel | Carries a secret? |
|---|---|---|
| `krayt.yaml` `network.inject[]` | `--secret NAME@host1,host2`, one per credential | no — a name and an allow list |
| `krayt.yaml` `network.allow` | `--net-rule` / `--net-default*` | no |
| `secrets.env` values | `cmd.Env` on the spawned `msb` | **yes — this only** |

The value channel is `exec.Cmd.Env`, extending the closed allowlist that `add-msb-sandbox-driver.md`
established (itself modelled on `egressProxyChildEnvKeys`,
`internal/orchestrator/egressproxy.go:32-57`): the process-hygiene keys plus, on top, exactly the
names read out of `secrets.env` that this run declares. Values travel in the `execve` envp array —
no disk, no argv, no shell.

**This deliberately overturns §6.6.1's stdin rule.** Phase 9 hands krayt's own injected credential
to the proxy child on stdin specifically to keep it out of a child's environment, and
`egressProxyChildEnvKeys`' comment says so in as many words. msb offers no stdin path: the complete
set of host-side sources is `SecretSource::{Env, Store}`
(`packages/microsandbox-types/rust/lib/modify.rs:142`), `Store` has zero usages anywhere in msb's
repository, and the CLI only ever builds `Env` (`common.rs:2027-2038`). The remaining alternative is
an inline value, which is SDK-only and lands in msb's durable on-disk config — strictly worse.

Be precise about the size of the step. `/proc/<pid>/environ` is mode 0400: same-uid only, unlike the
world-readable `/proc/<pid>/cmdline` that makes argv unacceptable. The adversary it admits is one
who can already read `secrets.env` (0600), ptrace krayt, and read msb's heap, where the value lives
for the sandbox's lifetime regardless of how it arrived — by msb's own documentation, un-zeroized.
Host compromise is out of scope in both threat models. **The requirement is therefore narrowed to
"never on argv, never persisted", and that narrowing is a decision, not an oversight.**

Two findings land in krayt's favour, both source-verified: `SecretEntry.value` is
`Zeroizing<String>`, "wiped when the entry drops" — better than msb's docs claim
(`domain.rs:2090-2103`) — and the reference model means the durable config never stores secret
material at rest (`common.rs:2016-2018`).

## Decisions already made (do not re-litigate)

1. **CLI + env-reference is the best secrets path msb offers, not a compromise the CLI forces.** The
   SDK is *worse*: its only create-time secret path takes a raw `Value string` that msb persists to
   the sandbox config on disk. Do not revisit the SDK to "avoid" the environment; it does not.
2. **Take msb's default placeholder, `$MSB_<ENV_VAR>`.** Do not supply a custom one. krayt therefore
   needs **no secret config file at all** — the repeatable `--secret NAME@HOST` flag covers the
   whole surface, and `placeholder`, `require_tls_identity` and `inject:` all keep defaults that are
   already what krayt wants (`require_tls_identity` true, `inject` = headers).
   **The contingency, if `probe-microsandbox-feasibility.md` P5 shows Claude Code rejects the default
   placeholder client-side:** msb exposes `placeholder` only through a config file, never argv, so
   krayt would need `--secret-conf PATH`. `--secret` carries no `conflicts_with` against
   `--secret-conf` (unlike `--net-rule` vs `--net-conf`), so the two combine. stdin is not an option
   — `sandbox_config.rs`'s `parse_yaml_value` is a plain `fs::read_to_string(path)` with no `-`
   handling — but `/dev/fd/N` backed by a pipe or memfd via `cmd.ExtraFiles` is, and is safe for
   `--secret-conf` specifically because `load_typed` does not call `absolutize_input` the way a root
   `--conf` does. **Write that down in the code comment; do not build it.** It is dead weight unless
   the probe fails.
3. **msb sets the guest's placeholder env var itself** — `guest_secret_env()` maps each secret's
   `env_var` to its `placeholder` and the runtime extends the guest bootstrap environment with it
   (`crates/network/lib/network.rs:454-464`, `crates/runtime/lib/vm.rs:1871`). So krayt does **not**
   emit a placeholder `--env`, and `adapter.Plan.Placeholders` has no job under msb. Do not
   reimplement what msb already does; a krayt-set `--env` of the same name would fight it.
4. **`krayt.yaml`'s `network.inject` keeps its name and loses most of its schema.** Keeping the key
   name matters: §8.3's containment list already refuses `network.inject` from an auto-loaded
   repo-local config, and that protection should not need re-deriving for a renamed key. The new
   shape:
   ```yaml
   network:
     inject:
       - key: GH_TOKEN              # secrets-file key name; never a value
         hosts: [api.github.com]    # hosts allowed to receive it
       - key: ANTHROPIC_API_KEY
         host: api.anthropic.com    # singular form accepted; `host` xor `hosts`
   ```
   `set`, `set_prefix`, `set_literal` and `strip` are **removed keys and must hard-error**, naming
   themselves and why they have no equivalent (msb matches a placeholder *string*, not a header
   name). Silently ignoring `strip` in particular would weaken the posture without telling anyone —
   see the security note below.
5. **The security regression is real, bounded, and must be written into §10 rather than discovered.**
   msb never strips a pre-existing auth header. krayt today strips `authorization` and `x-api-key`
   before setting one. Under B1, a credential the agent obtained elsewhere and placed in that header,
   addressed to an allowed host, goes out untouched. It is bounded by the allowlist — the agent can
   only send it somewhere already permitted — and it is the one row where B1 loses to krayt today.
   State it plainly in §10's threat table; do not soften it.
6. **The matched-secret-key-names scan moves to the host.** Today the guest scans `changes.patch` for
   secret *values* and reports which keys matched, because values never leave the VM
   (`secretPatchKeys`, `internal/orchestrator/orchestrator.go`). Under B1 values never *enter* the
   VM, so the host scans the patch it copied out — strictly more trustworthy (the agent cannot
   tamper with the scanner) and it removes the requirement from `add-krayt-guest-helper.md`
   entirely. Write the host-side scanner here; the cut-over wires it.
7. **Host-side redaction stays; guest-side redaction becomes unnecessary.** A guest that never holds
   a value cannot leak one into a log — the worst it can print is a placeholder, which is
   meaningless by construction. krayt's own artifact redaction (`redact-secrets-in-artifacts.md`,
   covering `report.md`, question text and the patch warning) still applies, because the *host* does
   hold values. Do not delete the redactor; do note in §6.9 why its guest half is no longer
   load-bearing.

## The gap the ADR does not name: non-network secrets

**Read this carefully — it is a capability loss, not a translation detail, and it is not in the
ADR.** Today every key in `secrets.env` is materialized into the guest at `/run/secrets/<KEY>` on
tmpfs (§6.8), so a secret can be *used inside the guest* — an SSH key, a signing key, a database
password for a local test — not only sent to a host.

Under B1 there is no such channel:

- `--secret NAME@HOST` never puts the value in the guest; the guest gets the placeholder.
- `--env KEY=VALUE` on `create`/`exec` puts the real value on **argv** — disqualified.
- `msb copy` into a `--tmpfs` mount would work, but requires writing the value to a host temp file
  first, which trades §6.8's "never persisted" for a weaker property. **Rejected**; if it is ever
  reconsidered it needs its own decision, not a quiet implementation.

So under B1 **every secret must be network-scoped**. Make that a pre-flight error, in the same
spirit as decision 4: a key present in `secrets.env` with no `network.inject` entry naming it fails
the run, naming the key and saying that B1 delivers secrets only as host-side substitutions to
allowed hosts. A value that genuinely must be readable inside the guest belongs in `env:` with the
user's eyes open, not in `secrets.env` where the name promises something krayt can no longer deliver.

Record this in §6.8 as an explicit narrowing of what "secret" means under B1.

## What to build

- `task.SecretSpec{Key string; Hosts []string}` and the parse/validate path for the new
  `network.inject` shape, including the removed-key errors (decision 4) and the
  every-secret-is-scoped rule above. Put the validation in `ValidateNetworkPolicyForMsb`, the
  not-yet-called function `translate-network-policy-to-msb.md` introduced.
- `internal/sandbox`: two pure functions, both trivially testable,
  ```go
  // SecretArgs renders one --secret NAME@host[,host...] flag per spec, deterministically ordered.
  func SecretArgs(specs []task.SecretSpec) []string
  // SecretEnv returns the KEY=VALUE entries to append to the msb child's environment,
  // for exactly the declared keys — never every key in the secrets file.
  func SecretEnv(specs []task.SecretSpec, vals map[string]string) ([]string, error)
  ```
  `SecretEnv` errors on a declared key the secrets file lacks: pre-flight refusal beats a run that
  fails thirty seconds in, unauthenticated — the rule `krayt.yaml`'s own comment already states.
- **Timing.** msb reads the value "at start time", not at config-load time (`common.rs:548-562`), so
  the environment must be set on whichever invocation actually *starts* the sandbox — `msb create`,
  in krayt's lifecycle. A later `msb exec` against an already-running sandbox needs no env, because
  the per-sandbox host runtime holds the value for the sandbox's lifetime. Assert this with a test
  on the driver: the exec argv's env must **not** contain any secret.
  Reassurance, worth a comment: msb's launcher "keeps only operator-readable labels on the real argv
  and serializes the rest — network config, env (including secrets), mounts, and paths — to an
  inherited fd, so they no longer appear in `ps` or `/proc/<pid>/cmdline`"
  (`crates/cli/lib/sandbox_cmd.rs:322`).
- **Adapters.** Add `Input.Sandbox bool` alongside the existing `Input.MITM`, following that field's
  own precedent — "False for every existing caller/test unless set explicitly, so an adapter that
  doesn't check it keeps today's behavior byte for byte"
  (`internal/adapter/adapter.go:29-34`). When set, an adapter returns `Plan.Secrets []task.SecretSpec`
  instead of `Plan.Inject`/`Plan.Placeholders`: for `claude-code` that is one spec naming whichever
  credential key `secrets.env` holds, scoped to `api.anthropic.com`. The exactly-one-credential rule
  of §6.14 is unchanged and still enforced.
  **`anthropic_wire.go` is not deleted here** — the live MITM path still calls it. But note in your
  report that decision 3 plus shape mirroring is what makes its deletion realistic at cut-over: the
  CLI emits `x-api-key: $MSB_…` or `authorization: Bearer $MSB_…` unprompted and msb swaps the
  placeholder without needing to know which header it was in, which is exactly the work the dated
  header table does today.
- Host-side `secretPatchKeys` equivalent (decision 6), operating on a patch file and a
  `map[string]string` of values, returning matched key names. Reuse the existing redaction
  machinery's matching rather than writing a second definition of "this looks like the value".
- Spec amendments, additive: §6.8 (the narrowing, the env-reference channel, the timing), §6.6.1
  (an explicit note that B1 overturns its stdin rule and why — the ADR asks for the
  `egressProxyChildEnvKeys` comment to be *amended rather than left contradicting*, so amend it),
  §6.14 (adapters return secret specs under msb), §10 (the un-stripped-header regression).

## Done when

- `go build ./...` (both `GOOS`), `go test -race ./...` and `golangci-lint run` are green, with the
  existing MITM path's tests — including `internal/orchestrator/mitm_secrets_test.go` and the
  `anthropic_wire` golden tests — **still passing unchanged**. That is the proof this task stayed
  additive.
- A test asserts `SecretEnv` returns entries only for declared keys, and that a `secrets.env`
  carrying an undeclared key produces a pre-flight error rather than a leaked env var.
- A test asserts no secret value ever appears in any rendered argv, for every `CreateSpec`/`ExecSpec`
  the driver can build — property-style over the whole struct, not one hand-written case.
- A test asserts the exec argv carries no secret env at all (the timing rule).
- A `krayt.yaml` carrying `inject[].set`, `set_prefix`, `set_literal` or `strip` fails
  `ValidateNetworkPolicyForMsb` with an error naming the key, while `ValidateNetworkPolicy` still
  accepts it.
- A `secrets.env` key with no `network.inject` entry fails `ValidateNetworkPolicyForMsb`.
- §10's threat table carries the un-stripped-header row.

## Out of scope

- Deleting `internal/proxy`, `internal/adapter/anthropic_wire.go`, `internal/orchestrator/egressproxy.go`
  or `internal/cli/egressproxy.go` — all at cut-over.
- Supplying krayt's own interception CA (deliberately not in this arc).
- Building the `--secret-conf` / `/dev/fd/N` contingency of decision 2.
- Anything that runs a sandbox.
