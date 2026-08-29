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

## What to build

- `krayt.yaml`'s `sandbox:` block with its single `extra_conf` key, resolved relative to the config
  file that declares it (matching how §8.3 anchors other paths), plus the §8.3 refusal.
- The `--conf <path>` argument threaded into `CreateSpec`, emitted **before** every krayt-owned flag.
- The `meta.json`/`report.md` fields of decision 5.
- Spec: §8.1 (the key and its unvalidated contract), §8.3 (the containment list), §10 (the `mounts`
  caveat).

## Done when

- `go build ./...`, `go test -race ./...`, `golangci-lint run` green.
- A test asserts `--conf <extra>` precedes every krayt-emitted flag in the rendered argv.
- A test asserts an auto-loaded repo-local `krayt.yaml` carrying `sandbox.extra_conf` is refused
  with an error naming the key, and that the same file passed via explicit `--config` is accepted.
- A test asserts `meta.json` carries the path and digest, and `report.md` renders the line.
- A test asserts krayt's child env is unchanged by the presence of an `extra_conf` — in particular
  that a secret declared only in that file adds no environment variable.
- **Hardware (`HUMAN_TODO.md`, non-blocking)**: one real run proving precedence empirically — an
  `extra_conf` whose `network.allow` names a host krayt's policy does not, confirming the host is
  still refused. Decision 2 is source-derived; this makes it observed.

## Out of scope

- Modelling any msb feature properly in `krayt.yaml`. If a feature turns out to be needed by every
  run, that is a schema change with its own task, not a reason to widen this hatch.
- Validating, linting or schema-checking the file.
