# Task: move the agent images to Debian trixie, then add `rtk` (Rust Token Killer) to all of them

**Read `CLAUDE.md`, `images/agents/*/Dockerfile` + `README.md` + `entrypoint.sh`,
`hack/krayt-dev/Dockerfile` + `README.md`, and `KRAYT_SPEC.md` §6.6 (egress) + §8.2 (container
contract) first.** Medium-large: a base-OS bump across every Debian image, then one new tool wired
into three agent images. Give a short plan and proceed; stop and ask if something here conflicts
with what you find in those files.

The two halves are sequenced deliberately — **do step 1 completely before starting step 2** — and
they are separate commits in the same change set, so a reviewer can bisect a base-OS regression
apart from an rtk regression.

## Background

[`rtk`](https://github.com/rtk-ai/rtk) ("Rust Token Killer", Apache-2.0) is a single static-ish Rust
binary that sits in front of common dev commands and compresses their output before an agent reads
it — `rtk git status`, `rtk cargo test`, `rtk grep …` — claiming 60–90% fewer bash-output tokens on
100+ commands. It integrates with agents through a **pre-tool hook**: the agent is about to run
`git status`, the hook rewrites it to `rtk git status`, and the model sees the compressed output.
Every agent integration is a thin delegate that shells out to `rtk rewrite`; all the logic lives in
the binary.

krayt's agent images (`images/agents/{claude-code,gemini-cli,opencode}`, and `hack/krayt-dev` built
on top of the first) run agents headlessly inside the sandbox, where every byte of command output
becomes context. That is exactly the workload rtk targets, so this task installs it in all three
published agent images (krayt-dev inherits it — see deliverable 8) and wires each one's native
integration so rewriting is automatic.

**Why a base-OS bump comes first.** rtk publishes no `aarch64-musl` build. Its arm64 Linux asset,
`rtk-aarch64-unknown-linux-gnu.tar.gz`, declares `GLIBC_2.39` in its `libc.so.6` version
requirements (verified with `objdump -p` on the real v0.45.0 asset, not assumed). Every image here
is Debian **bookworm**, glibc **2.36** — the binary would install fine and then fail to start with
`version 'GLIBC_2.39' not found`, on the exact arch (arm64) that Apple-Silicon dogfooding runs on.
Debian **trixie** (current stable, Debian 13) ships libc6 **2.41-12+deb13u3**, comfortably over that
floor. Bumping the base is therefore a prerequisite, and it's a change worth making on its own
merits — it also means rtk stays a 4 MB tarball download in the same shape as `gh`/`protoc`/
`hadolint`, with no Rust toolchain or from-source compile anywhere in CI.

## Decisions already made

Every item below is settled. Where a decision says "verify", verify it — but don't relitigate the
choice itself.

### 1. Debian trixie, everywhere, first

Bump **all** Debian bases in the repo, so nothing is left straddling two Debian generations:

| File | From | To |
|---|---|---|
| `images/agents/claude-code/Dockerfile` | `debian:bookworm-slim@sha256:7b140f…` | `debian:trixie-slim` |
| `images/agents/opencode/Dockerfile` | the same pinned digest (deliberately reused) | `debian:trixie-slim` |
| `hack/claude-code/Dockerfile` | the same pinned digest | `debian:trixie-slim` |
| `images/agents/gemini-cli/Dockerfile` | `node:24-bookworm-slim@sha256:3638d9…` | `node:24-trixie-slim` |

`node:24-trixie-slim` exists upstream (verified against Docker Hub's `library/node` tags). Node 24
stays as-is — this is an OS change, not a Node change.

**Digest pinning.** Renovate keeps these digest-pinned (`pinDigests: true` for the `dockerfile`
manager, `renovate.json`). Resolve the real trixie digests from the registry if this sandbox can
reach it; if it can't, leave the `FROM` **tag-only** with a short bootstrap comment and let
Renovate open the pin PR — `images/agents/gemini-cli/Dockerfile` already carries exactly that
comment from when it was in the same position, so copy its wording. **Never invent or copy-forward
a digest**: a wrong digest either fails the build or, worse, silently pins the wrong image.

Update the comments that name the old base, not just the `FROM` lines — each of these Dockerfiles
explains its base choice in prose (`opencode`'s says the digest is "reused rather than re-resolved,
since it's the identical upstream reference"; `gemini-cli`'s explains the Node base). Keep those
explanations true. Add, in `images/agents/claude-code/Dockerfile` (the base the others follow), one
sentence recording *why* trixie: rtk's arm64 build needs glibc ≥ 2.39, bookworm has 2.36, trixie has
2.41. That's the fact a future reader will need when they wonder why the base moved.

Do **not** touch the Alpine probe images (`hack/{edit,root,secrets,gitconfig,krayt-ask}-probe`), the
`golang:*-alpine` builder stages, or `Dockerfile.test` — none of them run rtk and none are affected.

### 2. rtk install: the pinned prebuilt release tarball

Same exception pattern `hack/krayt-dev/Dockerfile` already uses for `protoc`/`gh`/`hadolint` — a
prebuilt release archive, pinned by an `ARG`, with a `TARGETARCH` `case` that hard-fails an
unrecognized arch. Add this identically to all three `images/agents/*/Dockerfile`s.

The asset names are **not** a `TARGETARCH` substitution (unlike `gh`) and are **not** symmetric —
amd64 is a musl build, arm64 is a gnu build:

| `TARGETARCH` | Asset |
|---|---|
| `amd64` | `rtk-x86_64-unknown-linux-musl.tar.gz` |
| `arm64` | `rtk-aarch64-unknown-linux-gnu.tar.gz` |

Each tarball contains a single bare `rtk` binary at the archive root (no wrapping directory, unlike
`gh`'s). Verified against the real v0.45.0 release. A `checksums.txt` ships alongside; the existing
tool installs in this repo don't verify checksums, so match that rather than introducing a
one-off — but do keep `--fail` (`-f`) on every `curl` so a 404 on a renamed asset fails the build
loudly instead of writing an HTML error page to disk.

```dockerfile
ARG RTK_VERSION=0.45.0
ARG TARGETARCH
RUN set -eu; \
    case "${TARGETARCH}" in \
      amd64) rtk_target=x86_64-unknown-linux-musl ;; \
      arm64) rtk_target=aarch64-unknown-linux-gnu ;; \
      *) echo "unsupported TARGETARCH: ${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    curl -fsSL --retry 5 --retry-all-errors --retry-delay 2 -o /tmp/rtk.tar.gz \
      "https://github.com/rtk-ai/rtk/releases/download/v${RTK_VERSION}/rtk-${rtk_target}.tar.gz" \
 && tar -xzf /tmp/rtk.tar.gz -C /tmp \
 && install -d -m 0755 /usr/local/lib/rtk \
 && install -m 0755 /tmp/rtk /usr/local/lib/rtk/rtk \
 && rm -f /tmp/rtk.tar.gz /tmp/rtk
```

`0.45.0` was current when this task was written — **verify against
`https://api.github.com/repos/rtk-ai/rtk/releases/latest` and use whatever is current**, and
re-confirm the asset names there too rather than trusting this table.

**Do not `cargo install rtk`.** The `rtk` crate on crates.io is an unrelated project ("Rust Type
Kit — query Rust types and produce FFI types", `reachingforthejack/rtk`). A source install would
have to be `cargo install --git https://github.com/rtk-ai/rtk --tag vX.Y.Z`, which this task
deliberately avoids: it needs Rust ≥ 1.91 and an LTO release build of bundled SQLite on both arches.

Add a `renovate.json` custom manager alongside the existing ones, in the same shape as the
`GH_CLI_VERSION` entry — one per Dockerfile, or a single entry whose `managerFilePatterns` lists all
three, matching whatever reads more cleanly next to the existing entries:

```jsonc
{
  "customType": "regex",
  "description": "images/agents/*/Dockerfile: rtk (Rust Token Killer), fetched as a release tarball (Rust binary, no Dockerfile-manager datasource; the crates.io `rtk` crate is an unrelated project)",
  "managerFilePatterns": ["^images/agents/[^/]+/Dockerfile$"],
  "matchStrings": ["ARG RTK_VERSION=(?<currentValue>.*?)\\n"],
  "datasourceTemplate": "github-releases",
  "packageNameTemplate": "rtk-ai/rtk",
  "extractVersionTemplate": "^v(?<version>.*)$"
}
```

The `extractVersionTemplate` is load-bearing beyond stripping the `v`: `rtk-ai/rtk`'s releases feed
is dominated by `dev-<version>-rc.<N>` prerelease tags, which that anchor excludes. Confirm the file
still parses (`python3 -c "import json; json.load(open('renovate.json'))"`).

### 3. Rewriting is on by default, with a `KRAYT_RTK=off` per-run opt-out

rtk ships **no** disable switch of its own. But every agent integration — Claude's shell hook,
Gemini's hook, OpenCode's TS plugin — is a thin delegate that shells out to `rtk rewrite` and obeys
its exit-code contract, documented in `hooks/claude/rtk-rewrite.sh` upstream:

| Exit | Meaning |
|---|---|
| `0` + stdout | rewrite found, auto-allow |
| `1` | **no rtk equivalent — pass the original command through unchanged** |
| `2` | deny rule matched — pass through, let the agent's native deny handle it |
| `3` + stdout | ask rule matched — rewrite, but prompt |

So one wrapper on `rtk rewrite` disables automatic rewriting for **every** agent at once. Install
the real binary at `/usr/local/lib/rtk/rtk` (deliverable 2 above already does) and put this at
`/usr/local/bin/rtk` in each image:

```sh
#!/bin/sh
# krayt: per-run opt-out for rtk's automatic command rewriting (KRAYT_RTK=off, settable per run
# via krayt.yaml's `env:` block, §8.1). Exit 1 is rtk's own "no rtk equivalent" code, so every
# agent hook falls through to the ORIGINAL command — the same path a command with no rtk
# equivalent already takes. Direct `rtk <cmd>` invocations are unaffected: only `rewrite` is
# gated, so a task can still opt individual commands in by hand.
[ "${KRAYT_RTK:-on}" = "off" ] && [ "$1" = "rewrite" ] && exit 1
exec /usr/local/lib/rtk/rtk "$@"
```

Ship it with `COPY --chmod=0755` from a file in the image's directory (so `hadolint` and review see
it as a real file), not a heredoc `RUN`. It is identical in all three images; three copies of a
6-line file is the right call here rather than inventing a shared build context — these images
build from their own directories (`context: images/agents/<name>`, see `agent-images.yml`).

**Verify the wrapper is actually on the hook's path.** After wiring (deliverable 4), read what each
`rtk init` variant registered: if it recorded an **absolute path** to the binary rather than a bare
`rtk` resolved through `PATH`, the wrapper is bypassed and the opt-out silently does nothing. If
that's what you find, point the registered command at `/usr/local/bin/rtk` explicitly and say so in
the image README.

`KRAYT_RTK` means exactly "automatic rewriting on/off". It must never mean "rtk is absent" — the
binary stays on `PATH` and directly-invoked `rtk` commands keep working in both states.

Also set `ENV RTK_TELEMETRY_DISABLED=1` in each image. rtk's telemetry is opt-in and off by
default, and the run's egress allowlist denies unknown destinations anyway (§6.6), so this is
belt-and-braces — but it's one line and it makes the image's posture explicit, matching how
`gemini-cli` bakes `usageStatisticsEnabled: false` and `opencode` sets
`OPENCODE_DISABLE_AUTOUPDATE`. **No new egress-allowlist entries are needed for rtk**: it makes no
network calls at runtime.

### 4. Wire each agent's native integration, at build time, as that image's own user

rtk has a first-class integration for all three agents. Run the matching `rtk init` **after** the
`USER` line that switches to the image's non-root user, because `rtk init --global` writes into
`$HOME`:

| Image | User / `$HOME` | Command | What it writes |
|---|---|---|---|
| `claude-code` | `agent`, `/home/agent` | `rtk init --global --auto-patch` | `~/.claude/hooks/rtk-rewrite.sh` + a `PreToolUse` entry in `~/.claude/settings.json` |
| `gemini-cli` | `node`, `/home/node` | `rtk init --global --gemini` | `~/.gemini/rtk-hook-gemini.sh` + a `BeforeTool` entry in `~/.gemini/settings.json` |
| `opencode` | `agent`, `/home/agent` | `rtk init --global --opencode` | `~/.config/opencode/plugins/rtk.ts` |

Paths are from rtk's own `src/hooks/constants.rs` at v0.45.0. `--auto-patch` is the documented
non-interactive form for the settings.json patch; **run `rtk init --help` during the build and
confirm the flag spelling and whether `--gemini`/`--opencode` also need it** rather than trusting
this table — a wrong flag either prompts (and hangs the build) or no-ops.

After each `rtk init`, run `rtk init --show` in the same layer and let its output land in the build
log, so a CI log is evidence the hook registered rather than something to take on faith.

**`jq`: only if it's actually needed.** rtk's Claude hook is a shell script that requires `jq` and
warns-and-no-ops without it — and none of these images ship `jq`. But rtk 0.45 also has a
binary-command variant (`CLAUDE_HOOK_COMMAND = "rtk hook claude"` in `src/hooks/constants.rs`),
which needs no jq at all. So: after `rtk init`, read `~/.claude/settings.json` and see which command
it registered. If it's `rtk hook claude`, add nothing. If it's the `.sh`, add `jq` to that image's
`apt-get install` line (and only that image — the Gemini and OpenCode integrations are a Rust hook
and a TS plugin respectively, neither of which needs jq). Record which variant you found, in the
image README, because it explains the presence or absence of the dependency.

### 5. The gemini-cli entrypoint will delete rtk's hook — fix it

`images/agents/gemini-cli/entrypoint.sh` rewrites `$HOME/.gemini/settings.json` **wholesale** with a
heredoc when `KRAYT_ASK_SOCKET` is set, repeating the two static keys the Dockerfile baked in
(`general.enableAutoUpdate`, `privacy.usageStatisticsEnabled`) so they survive. rtk's `BeforeTool`
registration lives in that same file — so as written, **the hook is destroyed in exactly the runs
that enable questions** (`--on-question=wait`), and survives only in `fail` mode. That failure is
silent: no error, just no rewriting, in half of all runs.

Fix it properly — **merge, don't overwrite.** Preferred shape: build the `mcpServers` object and
merge it into the existing file (`jq` if you added it for deliverable 4, otherwise repeat rtk's hook
block in the heredoc the same way the two static keys already are). Whichever you choose:

- The comment above that block currently explains why it rewrites the file and repeats the static
  keys. Update it — it is the thing that made this bug easy to miss.
- Prove the result: run the entrypoint's settings-writing path offline with a fake
  `KRAYT_ASK_SOCKET` and a settings.json containing an rtk `BeforeTool` entry, and assert the output
  parses **and** still contains both the rtk key and `mcpServers`.

### 6. Documentation

- **`images/agents/*/README.md`** — each one has a "What's in the image" (or equivalent) section
  naming the base and its contents. Update the base (trixie / `node:24-trixie-slim`), add rtk (what
  it is, that rewriting is automatic, the `KRAYT_RTK=off` opt-out and how to set it per run via
  `krayt.yaml`'s `env:` block, §8.1), and note that rtk needs no egress and no secret.
- **`hack/krayt-dev/README.md`** — krayt-dev gets rtk **by inheritance**, not by a change of its
  own: it has no OS layer and no entrypoint, so rtk arrives only once `krayt-agent-claude-code` is
  republished and Renovate bumps krayt-dev's `FROM` digest (that README's "How a base-image change
  reaches this image" section already describes this chain — extend it rather than restating it).
  Add the **output-fidelity caveat**, which matters more here than anywhere else: rtk compresses and
  truncates output, and some krayt-dev workflows depend on verbatim text — notably reading the real
  `got: sha256-…` out of a Nix `vendorHash` mismatch (that README's own "Regenerating vendorHash"
  section, where fabricating a hash is explicitly forbidden) and reading a full `go test -race`
  failure list. Tell the reader to run those with `KRAYT_RTK=off`.
- **`hack/krayt-dev/Dockerfile`** — its header comment describes the base image's contents
  ("debian:bookworm-slim + `ca-certificates curl git bash`"). Make it true again.
- **Repo `README.md`** — the "Agent images" section states each image is `debian:bookworm-slim` (or
  `node:24-bookworm-slim` for gemini-cli) plus a non-root user (around line 342). Update it, and
  mention rtk in the same breath as the other things every agent image ships.
- **`docs/ai-tasks/add-codex-agent-image.md`** — still unstarted, and specifies a
  `debian:bookworm-slim` base. Re-point it at trixie so the next image built from that doc doesn't
  land the repo back on two Debian generations. Docs-only, one line.
- **Do not rewrite** the historical `docs/ai-tasks/*.md` entries that mention bookworm as a record
  of what was done at the time (`add-claude-code-agent-image.md`, `add-gemini-cli-agent-image.md`,
  `add-opencode-agent-image.md`, `build-krayt-dev-image.md`). They are a log, not current config.
- **`docs/ai-tasks/README.md`** — update this task's status row when you're done, per that file's
  own convention (what landed, and what's still handed off).

### 7. No workflow changes

`agent-images.yml` already builds all three images on both arches on any `images/agents/**` change,
and `dev-image.yml` already rebuilds krayt-dev on `hack/krayt-dev/**`. Nothing in this task adds an
image, an arch, or a trigger. Don't touch either workflow.

### 8. Scope boundary

`krayt-dev` gets **no rtk install of its own** (deliverable 6 explains why), the probe images and
`Dockerfile.test` are untouched, and nothing in `internal/`, the vmimage, or the Nix side changes.
This task adds no Go code and no tests to the Go suite.

## Deliverables

1. `images/agents/claude-code/Dockerfile` — trixie base; rtk tarball install; `rtk` wrapper;
   `RTK_TELEMETRY_DISABLED=1`; `rtk init --global --auto-patch` as `agent`; `jq` only if the
   registered hook command needs it.
2. `images/agents/opencode/Dockerfile` — trixie base; the same rtk block + wrapper + ENV;
   `rtk init --global --opencode` as `agent`.
3. `images/agents/gemini-cli/Dockerfile` — `node:24-trixie-slim`; the same rtk block + wrapper +
   ENV; `rtk init --global --gemini` as `node`.
4. `images/agents/*/rtk` (three copies of the wrapper script from decision 3), `COPY --chmod=0755`'d
   to `/usr/local/bin/rtk`.
5. `images/agents/gemini-cli/entrypoint.sh` — the settings.json merge fix from decision 5.
6. `hack/claude-code/Dockerfile` — trixie base only (no rtk; it's an integration/demo fixture).
7. `renovate.json` — the `RTK_VERSION` custom manager; file still valid JSON.
8. `hack/krayt-dev/{Dockerfile,README.md}` — comment/base corrections and the inherited-rtk +
   output-fidelity documentation from decision 6.
9. `images/agents/*/README.md`, repo `README.md`, `docs/ai-tasks/add-codex-agent-image.md`,
   `docs/ai-tasks/README.md` — per decision 6.
10. `HUMAN_TODO.md` — honest entries for everything under "Hand off" below.

## Verify

What you can do yourself, and must:

```sh
hadolint images/agents/claude-code/Dockerfile \
         images/agents/gemini-cli/Dockerfile \
         images/agents/opencode/Dockerfile \
         hack/claude-code/Dockerfile \
         hack/krayt-dev/Dockerfile
python3 -c "import json; json.load(open('renovate.json'))"
bash -n images/agents/*/entrypoint.sh
sh -n images/agents/*/rtk
hack/test-entrypoint-credentials.sh          # the shared entrypoint's credential contract, offline
go build ./... && go test ./...              # should be untouched by this task; confirm it
```

Re-verify the upstream facts rather than trusting this document — asset names and versions move:

```sh
curl -s https://api.github.com/repos/rtk-ai/rtk/releases/latest \
  | grep -E '"(tag_name|browser_download_url)"'
# and, for the arm64 asset, the thing this whole task is shaped around:
objdump -p rtk | sed -n '/Version References/,/^$/p'   # expect a GLIBC_2.39 line under libc.so.6
```

Also prove the gemini fix offline: run the entrypoint's settings-writing path with a fake
`KRAYT_ASK_SOCKET` against a settings.json that already has rtk's `BeforeTool` entry, and assert the
output parses and retains **both** keys (decision 5).

Attempt a local single-arch build if this sandbox has Docker/buildx — `docker buildx build
--platform linux/arm64 -t krayt-agent-claude-code:local images/agents/claude-code` — and if you have
it, run `rtk --version` inside the result: on arm64 that single command is what proves the entire
glibc premise of this task. If Docker isn't available, say so plainly and hand off; do not assume
the Dockerfile is correct because it reads correctly.

## Hand off (`HUMAN_TODO.md`, template in `KRAYT_SPEC.md` §14)

Never fabricate any of these. An honestly-blocked step is correct; a faked one is a defect.

- **The four images build on both arches** (if Docker isn't available here).
- **`rtk --version` runs inside the arm64 image.** Call this out as the decisive check: it is the
  one that fails loudly if the trixie bump didn't take, and it fails *only* on arm64.
- **Claude Code still installs and runs on trixie** — the native installer is the reason that base
  is glibc Debian at all (`images/agents/claude-code/Dockerfile`'s own header says so). Needs a real
  build plus a real run.
- **Rewriting actually happens in a real run** for each agent: a `report.md` or run log showing an
  `rtk`-prefixed command, plus the negative control — the same task with `KRAYT_RTK=off` in
  `krayt.yaml`'s `env:` showing the un-rewritten command. Gemini and OpenCode need live
  Gemini/OpenCode credentials; note that their integrations have never been exercised here.
- **The OpenCode plugin loads without network access.** `~/.config/opencode/plugins/rtk.ts` imports
  `@opencode-ai/plugin` as a type-only import (erased at runtime) and uses the `$` the plugin host
  injects, so it *should* need no npm install — but if opencode resolves plugin dependencies at
  load, the run's allowlist blocks the npm registry and the dependency must be pre-baked into the
  image. Unproven either way; state it as such.
- **Trixie digest pins** — if you left the `FROM`s tag-only, the Renovate bootstrap PR is the
  handoff (same as gemini-cli's original pin).
- **krayt-dev rebuild + repin** — `HUMAN_TODO.md` already carries an outstanding entry for exactly
  this (item 6 in its Status list, the rebase onto the published base). **Extend that entry**
  instead of opening a competing one; this task adds trixie + rtk to what that rebuild needs to
  pick up.

## Done when

- Every Debian base in the repo is trixie (`images/agents/{claude-code,gemini-cli,opencode}`,
  `hack/claude-code`), each with its explanatory comment updated and either a resolved digest or an
  honest tag-only bootstrap note.
- All three published agent images install rtk from the pinned prebuilt tarball with the
  arch mapping of decision 2, expose it via the `/usr/local/bin/rtk` opt-out wrapper, set
  `RTK_TELEMETRY_DISABLED=1`, and register their agent's native integration as the image's non-root
  user — with `rtk init --show` output in the build log.
- `KRAYT_RTK=off` demonstrably makes `rtk rewrite` exit 1 (and nothing else changes), and the
  registered hook command resolves to the wrapper rather than around it.
- `images/agents/gemini-cli/entrypoint.sh` merges rather than overwrites `~/.gemini/settings.json`,
  with an offline check proving rtk's key survives a questions-enabled run.
- `renovate.json` parses and tracks `RTK_VERSION` against `rtk-ai/rtk` releases, excluding its
  `dev-*-rc.N` tags.
- `hadolint` is clean on every changed Dockerfile; `hack/test-entrypoint-credentials.sh` passes;
  `go build ./...` and `go test ./...` are unaffected.
- The READMEs (three agent images, krayt-dev, repo root) describe the trixie base, rtk, the
  `KRAYT_RTK=off` opt-out, and krayt-dev's output-fidelity caveat.
- `HUMAN_TODO.md` has honest entries for every item above that needs a real build, a live
  credential, or a real run — and extends the existing krayt-dev rebuild entry rather than
  duplicating it.

## Constraints

- **Never invent a registry digest**, an image build result, a `rtk --version` output, or a run
  that didn't happen.
- **`KRAYT_RTK=off` disables automatic rewriting only** — never removes rtk from `PATH` and never
  breaks a directly-invoked `rtk <cmd>`.
- **No new egress-allowlist entries**, no new secrets, no entrypoint credential logic. rtk needs
  none of them; if you find yourself adding one, something is wrong.
- **Don't install rtk into `hack/krayt-dev`** — it inherits it, and a second install would drift
  from the base exactly the way the duplicated entrypoint once did (see that Dockerfile's header).
- **Don't touch** the probe images, `Dockerfile.test`, `agent-images.yml`, `dev-image.yml`, or
  anything under `internal/`.
- **Don't `cargo install rtk`** — that crate is a different project (decision 2).
- Keep step 1 (trixie) and step 2 (rtk) as separate commits, in that order.

## Output

When this task is done, output a suggested branch name and commit messages (don't create the branch
or commit yourself unless separately asked) — a kebab-case branch name describing the outcome, and
two Conventional Commits messages, one per step, both typed `chore:` (image/dev tooling, not a
CLI-facing `feat:`/`fix:` — these images ship via `agent-images.yml` and `dev-image.yml`, entirely
separate from the `krayt` binary's release-please package and the pinned vmimage).
