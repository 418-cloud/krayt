# Task: add `sandbox.extra_conf` — a bounded, contained escape hatch to msb's own schema

**Read `CLAUDE.md`, `docs/adr-microsandbox-sandbox-layer.md` ("Recommendation: translate, don't
forward"), and `KRAYT_SPEC.md` §8.1, §8.3, §10 first.** Give a short plan and proceed. Depends on
`run-tasks-on-microsandbox.md`. Small task; the care is all in the containment.

## Background

krayt translates its own vocabulary into msb flags rather than forwarding msb's schema, because
`krayt.yaml` is a *task* config and splicing a vendor's beta schema into it makes the file
bi-vocabulary — and because §8.3's containment rule only works over a closed set of keys krayt
models. The cost is that anything msb can do and krayt does not model is unreachable: DNS policy,
published ports, rlimits, CPU placement, bandwidth limits, idle timeouts, mount tuning.

A bounded escape hatch beats growing krayt's schema to chase a beta project's. `sandbox.extra_conf:
<path>` names one msb configuration file passed as an additional **root `--conf`**, before krayt's
own flags.

## Decisions already made (do not re-litigate)

1. **A root `--conf`, not a scoped `--net-conf`.** This is not a style choice: `--net-rule`, `--net`,
   `--net-default-egress` and `--net-default-ingress` all carry clap's `conflicts_with = "net_conf"`
   (`crates/cli/lib/commands/common.rs:401-436`), so a scoped network file beside krayt's own network
   flags is a hard runtime error. A root `--conf` has no such conflict.
2. **krayt's own configuration wins, and that is a property of msb's precedence — verify it, don't
   assume it.** msb resolves lower to higher: built-in defaults, then every `--conf`/scoped file
   left to right, then **explicit non-config CLI flags** (`docs/cli/configuration.mdx:93-97`). krayt
   passes its security-relevant policy as flags, so it sits above any `extra_conf`. Two specifics
   worth a test each:
   - **Network policy is atomic and flag-set.** When krayt passes `--net-default*`, msb's
     `build_network_policy` returns a policy whose rules are only krayt's, and the CLI takes the
     `replaces_configured_policy` branch that sets the whole policy (`common.rs:2088-2096`). An
     `extra_conf` carrying `network.policy`/`allow`/`deny` is therefore fully replaced, not merged.
   - **Secrets merge by environment-variable name**, so an `extra_conf` naming a secret krayt also
     declares is overridden by krayt's `--secret` flag — but one naming a secret krayt does *not*
     declare is added. It will resolve to an unset host environment variable, because krayt's child
     env carries only its own declared keys, so it cannot exfiltrate anything; assert that.
3. **§8.3 containment applies in full.** `sandbox.extra_conf` joins `network.mitm`, `network.inject`
   and `network.passthrough` on the list of keys refused from an **auto-loaded** repo-local
   `krayt.yaml`, for exactly the same reason: that file ships inside the repo the agent is about to
   edit, so it may not write the run's security policy. It is accepted only from an explicit
   `--config`.
4. **Explicitly unvalidated, and loudly so.** krayt does not parse the file, does not know msb's
   schema, and makes no promise about what it can express. Say that in §8.1, in the config comment,
   and in a stderr line at run start naming the file. **`mounts` is the key that matters**: an
   `extra_conf` can mount host paths into the guest and dissolve the filesystem boundary §10 depends
   on. That is the user's choice to make with an explicit `--config`; it is not a choice they should
   make without being told.
5. **Record it in the run's artifacts.** `meta.json` carries the resolved path and a digest of the
   file's bytes (`go-digest`, the same `sha256:<hex>` convention §6.11/§6.8/`record-run-provenance.md`
   already use), and `report.md` shows a line in the Run section when one was used. A reviewer
   reading a patch produced by a run with an unvalidated vendor config in the path must be able to
   see that from the artifacts alone.

## Influencing the secret-violation policy

krayt emits `--on-secret-violation passthrough` on every sandbox (`KRAYT_SPEC.md` §6.6, decided by
`hack/msb-probes/p7-passthrough-semantics.sh` after two runs died unrecoverably under
`block-and-log`). An operator who wants a *stricter* posture for one particular secret has no key
in `krayt.yaml` for it, and this hatch is where that lands. Three things are settled about how it
behaves — all of them consequences of msb's own merge rules rather than choices krayt makes:

1. **The global default is out of reach, by construction.** `--on-secret-violation` is a
   krayt-emitted *flag*, and flags outrank every `--conf` (decision 2's precedence chain). An
   `extra_conf` setting the top-level `on_violation` is silently overridden. Do not document it as
   a knob, and do not "fix" it by dropping krayt's flag — the flag being unconditional is what
   makes the run's posture readable from krayt's own code.

2. ~~**Per-secret `on_violation` IS reachable, because secret entries append rather than
   replace.**~~ **WRONG — disproved on hardware 2026-09-05, msb 0.6.16**
   (`hack/msb-probes/p8-extra-conf-precedence.sh`, measurement 2). This item claimed that because
   `SandboxBuilder::secret_entry` pushes without dedupe (`sdk/rust/lib/sandbox/builder.rs`), an
   `extra_conf` entry carrying `on_violation: Block` would tighten that one secret via
   `effective_violation_action`. The internal `SecretEntry` struct does carry the field and the
   handler would honour it — but **nothing on the `msb` CLI can populate it**. The config-file
   schema behind both a root `--conf`'s `secrets:` map and `--secret-conf` is `SecretInput`
   (`crates/cli/lib/sandbox_config.rs`), `#[serde(default, deny_unknown_fields)]` over exactly
   `value`, `allow`, `inject`, `require_tls_identity`; `materialize_secrets` hard-codes
   `on_violation: None` on every entry built from a file; `--secret ENV@HOST[,HOST...]` has no
   violation-action component; and `--on-secret-violation` sets the *global* action only. Because
   of `deny_unknown_fields` this does not degrade quietly — msb refuses the sandbox:

   ```
   error: invalid sandbox configuration …: unknown field `on_violation`,
     expected one of `value`, `allow`, `inject`, `require_tls_identity`
   ```

   The mistake was reading the internal domain struct as if it were the CLI's config schema. **The
   only per-secret lever this hatch reaches is scope** (item 3). `KRAYT_SPEC.md` §8.1 and §10 are
   corrected to match; a per-host violation signal would need a change in msb, not in krayt.

   The *other* half of this item still stands and still corrects decision 2 above, which says an
   `extra_conf` secret krayt also declares is "overridden by krayt's `--secret` flag": it is not
   overridden, it is added alongside. For *scope* the composition is union-like rather than
   replacement — an entry that makes a placeholder eligible for a host un-blocks it for that host,
   because the handler drops blocking reports for any placeholder another entry allows there
   (`handler.rs`'s `ineligible_for_substitution.retain`). That is what item 3 measures.

3. **The escalation this opens, and it belongs beside `mounts`.** Because entries append, an
   `extra_conf` can declare an entry for a secret krayt already declares and give it *wider*
   `allowed_hosts`. That secret's real value is in the msb child's environment — krayt put it
   there for its own `--secret` — so a `SecretSource::Env` reference resolves it and msb will
   substitute the **real credential** at the newly named host. Decision 2's exfiltration argument
   ("it will resolve to an unset host environment variable") holds only for secrets krayt does
   *not* declare; for the ones it does, the value is present and reachable.

   This is the secrets analogue of `mounts` dissolving the filesystem boundary, and it takes the
   same posture — the user's choice to make behind an explicit `--config` — but like `mounts` it
   must be **named**, not discovered. §10 says so, and the stderr line decision 4 requires should
   make clear the file may alter secret scoping as well as mounts.

## What to build

- `krayt.yaml`'s `sandbox:` block with its single `extra_conf` key, resolved relative to the config
  file that declares it (matching how §8.3 anchors other paths), plus the §8.3 refusal.
- The `--conf <path>` argument threaded into `CreateSpec`, emitted **before** every krayt-owned flag.
- The `meta.json`/`report.md` fields of decision 5.
- Spec: §8.1 (the key and its unvalidated contract), §8.3 (the containment list), §10 (the `mounts`
  caveat **and** the secret-scoping escalation of the section above — both are ways an `extra_conf`
  dissolves a boundary krayt otherwise guarantees, and they belong in one place).

## Done when

- `go build ./...`, `go test -race ./...`, `golangci-lint run` green.
- A test asserts `--conf <extra>` precedes every krayt-emitted flag in the rendered argv.
- A test asserts an auto-loaded repo-local `krayt.yaml` carrying `sandbox.extra_conf` is refused
  with an error naming the key, and that the same file passed via explicit `--config` is accepted.
- A test asserts `meta.json` carries the path and digest, and `report.md` renders the line.
- A test asserts krayt's child env is unchanged by the presence of an `extra_conf` — in particular
  that a secret declared only in that file adds no environment variable.
- **Hardware — DONE, 2026-09-05, msb 0.6.16.** `hack/msb-probes/p8-extra-conf-precedence.sh`
  PASSed on all four measurements. Precedence is observed, not merely source-derived: an
  `extra_conf` whose `network.allow` named a host krayt's policy did not was still refused, while a
  control sandbox running the same file with none of krayt's network flags reached it freely (so
  the file was live and merely outranked). The secret-scope-widening escalation is real — the
  **real value** was substituted at the widened host, against a control without the `extra_conf`
  that saw only the placeholder there. The per-secret `on_violation` measurement came back
  **REJECTED**, overturning item 2 of the section above (now corrected there and in §8.1/§10).
  Crucially the probe has a **krayt-driven arm**: measurements 1 and 3 were reproduced through a
  real `krayt run` carrying a real `sandbox.extra_conf:`, with `meta.json`'s recorded digest
  checked against the file, because the synthetic sandboxes only establish what msb does given
  argv transcribed by hand — which is not the claim §8.1 makes.

## Out of scope

- Modelling any msb feature properly in `krayt.yaml`. If a feature turns out to be needed by every
  run, that is a schema change with its own task, not a reason to widen this hatch.
- Validating, linting or schema-checking the file.
- A first-class `krayt.yaml` key for per-secret violation policy. If tightening one secret turns
  out to be something runs routinely need, that is a schema change with its own task — the hatch
  exists so the need can be discovered before the vocabulary is committed to, not so it stays
  unmodelled forever.
- Changing `--on-secret-violation` itself, or making it configurable. Its value is settled by
  `p7-passthrough-semantics.sh` and reasoned in `internal/task/netpolicy_msb.go`; re-run that probe
  before reopening it.
