# Task: seed each agent's first-run config from its adapter, so an interactive agent authenticates in any image

**Read `CLAUDE.md`, then `KRAYT_SPEC.md` §6.14, §7, §8.2, §13 (the "2026-09-16 amendment") and
§14 Phase 12 first**, plus `add-interactive-shell-session.md` decisions 2 and 14. State a short
plan and proceed.

## Background

`krayt shell` gives a human a terminal in the sandbox; they start the agent CLI by hand. Two agent
CLIs do not use the credential krayt hands them until a first-run step has been completed
interactively, and nothing in krayt completes it:

- **Claude Code** with `CLAUDE_CODE_OAUTH_TOKEN` shows first-run onboarding (theme picker, login
  flow, connectivity preflight) instead of using the token, unless `hasCompletedOnboarding` is
  `true` in its global config. Observed by the repo owner in a real `krayt shell` session
  (2026-09-16), where onboarding's preflight also failed on `platform.claude.com` because that host
  wasn't in the allowlist.
- **Gemini CLI** shows its auth dialog even when `GEMINI_API_KEY` is set, unless
  `security.auth.selectedType` is set in its settings (evidence below).

`krayt run` never hit either one: `claude -p` and `gemini -p` skip onboarding and choose auth from
the environment. The published images also paper over part of it in their headless entrypoints.
A `krayt shell` user, though, may bring **any image with the CLI installed**. `krayt shell` needs
no `krayt-agent-entrypoint` (§8.2's contract binds `krayt run` only), so an image-side fix
(a baked `~/.claude.json`, or more `krayt-agent-shellenv`) cannot reach those images. The user
should also not need to know any of this. The adapter already knows which agent this is and which
credential was chosen (§6.14), so the fix belongs there: host-side, declarative, image-agnostic.

### Evidence (verified while writing this task; cite it, don't re-derive it)

**Claude Code.** The image pins 2.1.226. These facts were read from the minified JS inside the
2.1.273 native binary, whose error text matched what the 2.1.226 session printed:

- The global config is `join(CLAUDE_CONFIG_DIR || homedir(), ".claude.json")`. No environment
  variable skips onboarding. The startup flow returns early only when
  `config.hasCompletedOnboarding` is true.
- In **interactive** mode, an `ANTHROPIC_API_KEY` from the environment is used **only** if
  `config.customApiKeyResponses.approved` contains `key.trim().slice(-20)`. Print mode uses it
  unconditionally. The approval dialog ("Detected a custom API key in your environment") has its
  default focus on **"No (recommended)"**. A human pressing Enter rejects the key, and with
  onboarding skipped the key is simply ignored. So for `ANTHROPIC_API_KEY`, onboarding alone is
  not enough: the approval must be seeded too.
- The key Claude Code sees is msb's placeholder, never the real value (§6.14). msb's default
  placeholder is `$MSB_<NAME>`, verified on hardware by P5 (`$MSB_ANTHROPIC_API_KEY`, §14 Phase 11)
  and in msb 0.6.16's `crates/network/lib/secrets/config.rs`. The seeded approval is therefore the
  last 20 characters of a non-secret string.

**Gemini CLI** (image pins 0.55.1; read from the `v0.55.1` tag):

- `packages/cli/src/ui/auth/useAuth.ts`: with no `security.auth.selectedType`, the interactive UI
  raises `Existing API key detected (GEMINI_API_KEY). Select "Gemini API Key" option to use it.`,
  i.e. the auth dialog. `packages/cli/src/gemini.tsx` auto-selects a type only for Cloud Shell or
  `GEMINI_CLI_USE_COMPUTE_ADC`; there is no environment variable for the API-key types.
- `AuthType`: `'gemini-api-key'` (USE_GEMINI) and `'vertex-ai'` (USE_VERTEX_AI).
- User settings path: `Storage.getGlobalSettingsPath()` = `join(homedir(), ".gemini",
  "settings.json")`, where `homedir()` (`packages/core/src/utils/paths.ts`) returns
  `GEMINI_CLI_HOME` if set, else `os.homedir()`.
- Folder trust is on by default (`security.folderTrust.enabled ?? true`, `trustedFolders.ts`).
  `GEMINI_CLI_TRUST_WORKSPACE=true` bypasses it (`--skip-trust` just sets that variable,
  `config.ts`). Only the published image's entrypoint sets it; a headless run without it exits 55
  (`images/agents/gemini-cli/entrypoint.sh`).
- A `GOOGLE_API_KEY` credential also needs `GOOGLE_GENAI_USE_VERTEXAI=true`. Again, only the
  published entrypoint sets it.

**opencode:** nothing to seed. Its docs (`packages/web/src/content/docs/cli.mdx`, "auth login")
say providers are loaded at startup from any keys set in the environment. No login or first-run
step gates an env-var credential.

## Decisions already made (do not re-litigate)

1. **The fix is host-side and adapter-declared. No image changes.** The published entrypoints
   keep their own `GEMINI_CLI_TRUST_WORKSPACE`/`GOOGLE_GENAI_USE_VERTEXAI` exports. They are
   harmless duplicates, and they still serve `agent.adapter: none`.

2. **A new declarative field, `adapter.Plan.ConfigSeeds []ConfigSeed`:**
   ```go
   type ConfigSeed struct {
       DirEnv   string         // guest env var that, when set and non-empty, replaces $HOME as the base dir
       Path     string         // file path relative to that base; clean, relative, no ".." element
       Defaults map[string]any // JSON object merged INTO the file — see decision 3
   }
   ```
   Adapters stay msb-agnostic and do no I/O. They only describe the state they need.

3. **Merge semantics: fill in, never override.** Write this as a pure function with table tests.
   - Objects merge recursively.
   - A key already present keeps its existing value, whatever its type, including an explicit
     `false` or `null`. The image author's or user's explicit choice wins.
   - Arrays take the union: append each default element not already present (deep equality),
     preserving existing order.
   - A type conflict (existing non-object where the default is an object) keeps the existing
     value and skips that branch.
   - If the merge changes nothing, report "unchanged" so the caller skips the write.

4. **The orchestrator applies seeds from the host, running every guest step as
   `sandboxAgentUser`, never root.** File ownership then comes out right (the agent rewrites these
   files constantly), and no root process writes through a path the image controls.
   `krayt-helper` is **not** changed: it stays root-only with two subcommands
   (`add-krayt-guest-helper.md` decision 1). Per seed:
   1. **Resolve the base dir.** Exec `sh` printing `${<DirEnv>:-$HOME}`. Validate `DirEnv` against
      `^[A-Za-z_][A-Za-z0-9_]*$` before it goes anywhere near a script. Everything else travels as
      positional arguments (`sh -c '…' sh "$arg"`), never interpolated. The result must be an
      absolute path. Generalize the existing `guestHome` (same "always print something" rule for
      `Exec`'s `ErrMsbFailed` heuristic) rather than duplicating it.
   2. **Read** the existing file with an exec as the agent user. A missing file means `{}`. Cap the
      read at 1 MiB; over the cap means skip this seed.
   3. **Merge** on the host (decision 3). If the existing content is not a JSON object, skip the
      seed and warn. **Never overwrite a file krayt could not parse.** This is the same policy as
      the gemini-cli entrypoint's settings merge.
   4. **Write** only if changed, with an exec as the agent user and the merged bytes on
      `ExecSpec.Stdin`: `mkdir -p` the parent (computed on the host with `path.Dir`), write a temp
      file in the same directory, then `mv -f` it over the target. The script must print something
      on failure so the error isn't reported as `ErrMsbFailed`. The only guest tools assumed are
      `sh`, `cat`, `mkdir` and `mv`, the same floor `defaultShellCommand`/`guestHome` already
      assume.

5. **When: once per sandbox, in the shared prologue.** Apply seeds after `helperSetup` and before
   the agent exec (`Run`) or `ExecTTY` (`Shell`), through one shared function, the same way
   `copyInputs`/`helperSetup` are shared. `AttachShell` and `PatchLiveShell` never seed: the
   sandbox is already set up, and the human may have changed those files since.

6. **Best-effort.** A seed failure never fails a run or a session. Print one warning line naming
   the guest path and the reason on the host output krayt already uses for pre-boot and progress
   messages, then continue. No `meta.json` schema change.

7. **`krayt shell` now also applies `Plan.Env` and `Plan.ConfigSeeds`,** in addition to
   `Plan.Secrets`. Replace `applyAdapterSecrets` with a shell-specific helper that does exactly
   those three.
   - Shell still passes a zero `QuestionsWait`/`AskSocket`, so `askEnv` stays nil.
   - Shell still ignores `TranscriptDir` and never launches an agent.
   - Decision 2 of `add-interactive-shell-session.md` holds for everything it was about. This
     extends the §13 "2026-09-16 amendment" and is not a reversal. Update that amendment and the
     doc comments in `internal/cli/shell.go` and `internal/orchestrator/shell.go`.
   - `Plan.Env` goes through the existing `mergeEnv`, so the user's `krayt.yaml` `env:` still wins.

8. **The placeholder reaches adapters through `adapter.Input`, not an msb import.**
   - Add `Input.Placeholder func(key string) string`.
   - Add a pure `sandbox.SecretPlaceholder(key string) string` returning msb's default
     `"$MSB_" + key`. Its doc comment cites P5 / §14 Phase 11, and a unit test pins it.
   - The CLI fills `Input.Placeholder` with that function for both `run` and `shell`.
   - A nil `Placeholder` means the adapter omits placeholder-derived seeds.

9. **claude-code seeds exactly this:** one `ConfigSeed{DirEnv: "CLAUDE_CONFIG_DIR", Path:
   ".claude.json"}` with `Defaults`:
   - always `{"hasCompletedOnboarding": true}`;
   - when the selected credential is `ANTHROPIC_API_KEY`, also
     `{"customApiKeyResponses": {"approved": [<last 20 chars of strings.TrimSpace(Placeholder(cred))>]}}`
     (JS `trim()` ≈ `strings.TrimSpace` for this ASCII value).

   Nothing else: not the folder-trust dialog (`projects[...]`), not the theme, not
   `bypassPermissionsModeAccepted`. `ANTHROPIC_AUTH_TOKEN` and `CLAUDE_CODE_OAUTH_TOKEN` get the
   onboarding key only.

10. **gemini-cli:**
    - One `ConfigSeed{DirEnv: "GEMINI_CLI_HOME", Path: ".gemini/settings.json"}` with `Defaults`
      `{"security": {"auth": {"selectedType": X}}}`, where X is `"gemini-api-key"` for
      `GEMINI_API_KEY` and `"vertex-ai"` for `GOOGLE_API_KEY`.
    - `Plan.Env` gains `GEMINI_CLI_TRUST_WORKSPACE=true` always, and
      `GOOGLE_GENAI_USE_VERTEXAI=true` when the credential is `GOOGLE_API_KEY`. `askEnv`'s result
      is merged alongside these, not replaced.
    - The published image's baked `settings.json` (general/privacy keys plus rtk's hook) must come
      out of the merge with every existing key intact. Test it against that exact content.

11. **opencode and `none`: no seeds, no env changes.** Add a one-line comment in `opencode.go`
    saying why (env-var providers load at startup), citing the doc above.

## What to build

- `internal/adapter`: the `ConfigSeed` type, `Plan.ConfigSeeds`, `Input.Placeholder`, the pure
  merge function (or put it in a small package of its own if that reads better; your call), and
  the claude-code and gemini-cli changes above.
- `internal/sandbox`: `SecretPlaceholder`.
- `internal/task.RunSpec`: carry the seeds (e.g. `ConfigSeeds []adapter.ConfigSeed`, or a
  task-local mirror type if importing `adapter` into `task` would create a cycle; check that).
- `internal/orchestrator`: the shared apply-seeds step (decisions 4–6), wired into `Run` and
  `Shell` only.
- `internal/cli`: `applyAdapter` sets `spec.ConfigSeeds` and `Input.Placeholder`. The new shell
  helper does the same (decision 7).

## Tests (the "Done when" for this task)

- **Merge table tests:**
  - missing file;
  - `{}`;
  - an existing key of every JSON type that must not be overwritten (including `false` and `null`);
  - a nested object merge;
  - array union with and without the element already present;
  - a type conflict;
  - the "unchanged" signal;
  - non-object JSON (`[]`, `"x"`) and invalid JSON, both refused.
- **Adapter tests:**
  - claude-code, each of its three credentials;
  - gemini-cli, both credentials;
  - gemini-cli's `Plan.Env` with and without `QuestionsWait`;
  - a nil `Placeholder` (no approval seed);
  - opencode and `none` return no seeds.
- **`sandbox.SecretPlaceholder("ANTHROPIC_API_KEY") == "$MSB_ANTHROPIC_API_KEY"`**, and the
  claude-code approval element is `"SB_ANTHROPIC_API_KEY"`.
- **Orchestrator tests against the fake msb** (`internal/orchestrator/fakemsb_test.go`; extend it
  so `exec` honors `--user`, piped stdin and the seed scripts inside the fake sandbox root):
  - seeds are written as the agent user before the agent exec in `Run`, and before `ExecTTY` in
    `Shell`;
  - an existing file keeps its other keys;
  - an unchanged merge performs no write;
  - an invalid-JSON file is left byte-identical and a warning is printed;
  - a failing seed exec does not fail the run or session;
  - `DirEnv` set in the guest environment redirects the path;
  - an invalid `DirEnv` name is rejected before any exec;
  - `AttachShell` performs no seed exec.
- **CLI tests:** `krayt shell` with `agent.adapter: gemini-cli` gets the adapter env (and the
  user's `env:` still wins); with `agent.adapter: claude-code` the spec carries the seed. Extend
  `internal/cli/shell_test.go`'s existing adapter test rather than adding a parallel one.
- `GOOS=darwin go build ./...`, `GOOS=linux go build ./...`, `go vet ./...`, `go test -race ./...`
  and `golangci-lint run` are all green.

## Docs and spec

- **§6.14:** a short **"First-run state"** paragraph: what each adapter seeds and why, the
  fill-in-never-override rule, and that it is image-agnostic.
- **§7:** the new step between helper setup and the agent/tty exec.
- **§8.2:** one sentence saying an image need not pre-seed agent first-run config when an adapter
  is configured.
- **§13:** extend the 2026-09-16 amendment per decision 7.
- **§14 Phase 12:** a bullet for this task, with the hardware items as unchecked boxes.
- **`docs/ai-tasks/README.md`:** update this task's row with the outcome.

## Handoff (`HUMAN_TODO.md`, non-blocking)

Add one `[HUMAN]` entry with these hardware checks (real Mac, msb 0.6.16, a live credential each):

1. **`CLAUDE_CODE_OAUTH_TOKEN`:** run `krayt shell --image ghcr.io/418-cloud/krayt-agent-claude-code
   --config krayt.yaml`, then `claude`. Expect no onboarding and an authenticated prompt; a
   one-line request succeeds.
2. **`ANTHROPIC_API_KEY`:** the same flow. Expect no "Detected a custom API key" dialog, and
   authentication succeeds. Inside, confirm `printenv ANTHROPIC_API_KEY` prints
   `$MSB_ANTHROPIC_API_KEY`. If it prints anything else, `SecretPlaceholder` is wrong: read the
   value from the guest instead.
3. **Whether `platform.claude.com` is still needed.** With onboarding seeded, the preflight that
   contacted it no longer runs. Test with it removed from `krayt.yaml`'s `allow`/`passthrough`,
   and record the answer in `images/agents/claude-code/README.md`'s "Required `--allow` hosts"
   section.
4. **gemini-cli with `GEMINI_API_KEY`:** start `gemini` interactively. Expect no auth dialog and
   no folder-trust dialog.
5. **A minimal image with no krayt entrypoint** (e.g. `debian:trixie-slim` plus the official
   Claude Code installer, run as a non-root user named `agent`), used with `krayt shell`. Check 1
   passes against it too.

## Out of scope (log, don't fix)

- **Gemini's `GOOGLE_API_KEY` host scope.** The adapter scopes it to
  `generativelanguage.googleapis.com`, but the gemini-cli entrypoint's own comment says that
  credential goes through Vertex AI Express (`aiplatform.googleapis.com`). If so, msb never
  substitutes it. Add this to `HUMAN_TODO.md` as a separate finding to verify with a live key;
  don't change the scope here.
- **opencode's `models.opencode.ai` catalog fetch in a user-built image** (the published image
  bakes a snapshot). This is an egress question, not first-run state.
- **The folder-trust dialogs** (Claude's per-project trust; Gemini's is handled by decision 10's
  env var), theme and other UI preferences, and any change to the published images.

## Report

In `/output/report.md`, state:

- what landed;
- the exact seed contents per adapter and credential;
- the test list;
- what went to `HUMAN_TODO.md`;
- anything in "Decisions already made" you found hard evidence against.
