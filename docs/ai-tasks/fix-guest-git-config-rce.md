# Task: stop the container from injecting git config the root guest-agent executes

**Read `CLAUDE.md` and `KRAYT_SPEC.md` (§6.7 code transfer & patch generation, §10 security model)
first. Proceed autonomously. Demonstrating the exploit needs a real Mac; write the regression test
and hand it off via `HUMAN_TODO.md` (§14) rather than faking a result.**

## Reason (the finding)

**Finding #2 (Critical / container→guest-root escape).** `makeContainerWritable`
(`internal/guest/service.go:413-434`) walks the whole workspace and ORs `0o066` (group+other
read/write) onto **every** regular file — **including everything under `.git`**. The comment claims
*".git stays root-owned, so the guest's own git is unaffected"*, but root-owned and world-*writable*
are different: the untrusted container (mounted at `/workspace`, rw — `containerd_linux.go:187`) can
now write `/workspace/.git/config`, `.git/hooks/*`, `.gitattributes`, etc.

After the agent runs, the guest-agent — **running as root** — executes `git add -A` and
`git diff --cached --binary <baseline>` with `cmd.Dir = workspace`
(`internal/patch/patch.go:228-237`, called from `internal/guest/service.go:470-482`). git always
reads the **repo-local** `.git/config` (`GIT_CONFIG_NOSYSTEM=1` disables only *system* config), and
because the workspace `.git` is root-owned and git runs as root, the `safe.directory` ownership check
passes and the config is fully trusted. An attacker who wrote:

```ini
[core]
    fsmonitor = /workspace/pwn.sh
```

gets `pwn.sh` executed **as root in the guest** when the guest runs `git add -A` (fsmonitor is invoked
on index refresh). Diff drivers via `.gitattributes` + `[diff "x"] command = …` are an additional
vector on `git diff`. Root in the guest holds `CAP_NET_ADMIN` → it can `nft flush ruleset` (defeating
the egress lock) and read every secret from the tmpfs — a full escape from the container into the
guest VM.

## Goal

Generate the patch **without the guest ever running git in a repo whose config the container
controls**, while still letting the agent make its own commits inside the container (so
`commits.bundle` for committed work keeps working — decision already made). Concretely: run the
guest's `add`/`diff`/`bundle` git operations against a **separate, root-only git dir** the container
never sees, using `GIT_WORK_TREE` pointed at the workspace to capture the working-tree changes.

## Current behavior (grounding)

- `internal/guest/service.go:247` — `patch.Ingest` clones the bundle into `workspace` (creating
  `workspace/.git`, root-owned).
- `internal/guest/service.go:253` — `makeContainerWritable(workspace)` relaxes the whole tree,
  including `.git`.
- `internal/guest/service.go:470-482` — `buildArtifacts` runs `patch.Diff(workspace, BaselineTag)`
  and `patch.BundleCommits(workspace, BaselineTag, …)`, both shelling out to git *in* `workspace`.
- `internal/patch/patch.go:196-221` — `Ingest` records the baseline commit + tags it `krayt-baseline`
  inside `workspace/.git`.
- `internal/patch/patch.go:228-260` — `Diff` does `git add -A` then `git diff --cached --binary`;
  `BundleCommits` does `git bundle create … baseline..HEAD`. All in `workspace`.
- `internal/patch/patch.go:378-397` — `runGitRaw` sets `GIT_TERMINAL_PROMPT=0`,
  `GIT_CONFIG_NOSYSTEM=1` (system only — **repo-local config is still honored**).

## Implement

Pick the isolation design below (matches the decision "keep agent commits, isolate patch-gen"):

### A. Keep a root-only, container-invisible git dir for patch generation

1. **At ingest**, after cloning into `workspace` and tagging the baseline, **copy the pristine
   `.git` to a location outside the workspace** that is never made container-writable and never
   bind-mounted into the container — e.g. `filepath.Join(root, "patchgit")` (sibling of `workspace`,
   under the Service root). This `patchgit` dir holds the baseline objects/refs and is root-owned
   `0700`. The workspace keeps its own `.git` so the agent can still `git commit` inside the
   container.
2. **Generate the patch against `patchgit`, not the workspace `.git`.** Add a variant of
   `patch.Diff` that runs git with an explicit, isolated environment:
   ```
   GIT_DIR=<root>/patchgit
   GIT_WORK_TREE=<workspace>
   GIT_CONFIG_NOSYSTEM=1
   GIT_CONFIG_GLOBAL=/dev/null
   git -c core.fsmonitor= -c core.hooksPath=/dev/null -c core.symlinks=false \
       add -A
   git -c core.fsmonitor= -c core.hooksPath=/dev/null \
       diff --cached --binary --no-textconv <baseline>
   ```
   Because `GIT_DIR` is the root-only `patchgit` (whose `config` the container never touched) and the
   dangerous knobs are force-cleared on the command line (they win over any repo config that might
   still be read), no attacker-controlled config or attribute driver can execute. `--no-textconv`
   neutralizes diff-driver `textconv`; `core.hooksPath=/dev/null` and `core.fsmonitor=` neutralize
   hook/fsmonitor execution. `.gitattributes` in the work tree can still influence *how* a diff is
   rendered but cannot execute a command once `--no-textconv` and an empty external-diff config are
   in force — still, prefer running with `GIT_ATTR_NOSYSTEM=1` and not honoring in-tree attributes
   for the patch (acceptable: the patch is a faithful content diff, not a filtered one).
3. **`BundleCommits`** reads the agent's commits from the **workspace** `.git` (the container-writable
   one) — that repo is untrusted, but `git bundle create` runs no hooks/fsmonitor/textconv. Run it
   with the same force-cleared knobs (`-c core.fsmonitor= -c core.hooksPath=/dev/null`) and
   `GIT_CONFIG_GLOBAL=/dev/null`, and keep `commits.bundle` as a best-effort artifact. If you prefer
   maximum safety, fetch the agent's commits into `patchgit` first (`git fetch <workspace> …` with
   the hardened env) and bundle from there.
4. **Baseline integrity:** the `krayt-baseline` tag/ref used for the diff must come from `patchgit`
   (root-only), **not** from the workspace `.git` (where the container could move the tag). Ensure
   `Diff`/`BundleCommits` resolve `krayt-baseline` in `patchgit`.

### B. Alternative / additional hardening (do both if cheap)

- Also change `makeContainerWritable` to **skip the `.git` subtree entirely** if — and only if — the
  agent does not need to write it. Decision: agents *do* commit, so `.git` stays writable; therefore
  design A (isolated `patchgit`) is the primary fix and `.git` remains container-writable. Do **not**
  simply make `.git` read-only (that breaks commits). Keep design A.

Add a short comment at `makeContainerWritable` explaining that `.git` is intentionally left writable
for agent commits and that patch generation is isolated in `patchgit` so the writable `.git` is never
trusted by root git.

## Tests

Unit (no VM — `internal/patch` runs git on the host):
- **Config-injection is inert.** Build a workspace repo, write a malicious `.git/config`
  (`[core] fsmonitor = <script that touches a sentinel>`) and a malicious `.gitattributes`
  (`* diff=evil` + `[diff "evil"] textconv = <script>`), then run the new isolated `Diff`. Assert
  the sentinel file was **not** created and the diff output is still correct for a normal edit.
- **Baseline tamper is inert.** Move `workspace/.git` refs/tags (e.g. delete/rewrite
  `krayt-baseline` in the workspace repo) and assert the isolated `Diff` still diffs against the
  real baseline resolved from `patchgit`.
- **Working-tree capture still works.** An uncommitted edit in the workspace yields a non-empty
  patch via `GIT_WORK_TREE` (regression against today's behavior in `patch.Diff`).
- **Commits still bundle.** An agent-style commit in the workspace repo still produces a valid
  `commits.bundle`.

Integration (real Mac, `HUMAN_TODO.md` hand-off): full run where the container writes the malicious
`.git/config`; assert no root-side code executed (no sentinel, egress lock still present via
`nft list ruleset`, secrets not exfiltrated).

## Docs (required)

- `KRAYT_SPEC.md` §6.7: document that the guest generates the patch from a **root-only git dir**
  (`patchgit`), never from the container-writable `workspace/.git`, and that all guest git invocations
  force-clear `core.fsmonitor`/`core.hooksPath` and use `--no-textconv`. Note that the workspace
  `.git` is deliberately container-writable (for agent commits) and therefore untrusted.
- `KRAYT_SPEC.md` §10: add/adjust the "malicious patch content" residual — the source repo's hooks
  are already never run; now the *guest's own* git no longer trusts container-written `.git` config
  either.
- `docs/ai-tasks/README.md`: add this task.

## Verify (offline)

```sh
go build ./...
GOOS=linux GOARCH=arm64 go build ./...
go test -race ./...     # includes the new internal/patch injection tests
golangci-lint run
```

## Done when

- Guest patch generation runs against `patchgit` with force-cleared dangerous git knobs; the
  injection unit tests pass (sentinel never created, baseline tamper inert); working-tree diffs and
  `commits.bundle` still work; the integration escape test is written and logged in `HUMAN_TODO.md`.
- KRAYT_SPEC §§6.7/10 updated.

## Constraints

- No new dependency (still shelling out to the `git` binary, per §9.1 — no git library is pinned).
- Keep `internal/patch` OS-agnostic (it runs on host and guest).
- Preserve the existing `changes.patch` content/format byte-for-byte for the non-malicious case
  (the human's review workflow must not change).
