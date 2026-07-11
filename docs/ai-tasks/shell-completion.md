# Task: shell completion for run IDs, question IDs, and flag values

**Read `CLAUDE.md` and `KRAYT_SPEC.md` (§13 CLI surface) first. Proceed autonomously — this is a
self-contained task run inside a krayt sandbox; there is no interactive human to approve a plan
(use the `ask_human` tool only if genuinely blocked).**

## Background

krayt's commands take positional IDs and flag values that a developer has to look up and
copy/paste by hand today — a run id (`krayt ls` first, then paste into `apply`/`logs`/`stop`/…), a
question id (`krayt questions <id>` first, then paste into `answer`), or one of a fixed set of
valid strings for an enum flag (`--net allowlist|full|none`, etc.). Everything needed to complete
all of these is already on the host: on-disk run records under `.krayt/runs/`, and small fixed
value sets already defined as Go constants in this codebase. No network call, no guest/VM
involvement, for any of it.

cobra (already a dependency, `github.com/spf13/cobra v1.10.2`, `go.mod:14`) auto-registers a
hidden `completion` subcommand on the root command that generates bash/zsh/fish/powershell scripts
(`Command.CompletionOptions.DisableDefaultCmd` defaults to `false` — already available, `krayt
completion zsh` etc. already works for **static** completion of command/flag names). This task adds
**dynamic** completion on top of that: `ValidArgsFunction` for positional args, and
`RegisterFlagCompletionFunc` for flag values. Neither is used anywhere in this codebase today.

cobra's completion API (verify against the vendored version — `go doc github.com/spf13/cobra
Command`/`CompletionFunc`/`ShellCompDirective`/`FixedCompletions` — if anything below looks off):

```go
type CompletionFunc = func(cmd *Command, args []string, toComplete string) ([]Completion, ShellCompDirective)
// Completion is a type alias for string ("type Completion = string"), so a plain []string
// return value works directly — no conversion needed.
func CompletionWithDesc(choice string, description string) Completion // "choice\tdescription"
func FixedCompletions(choices []Completion, directive ShellCompDirective) CompletionFunc // static list
const ShellCompDirectiveNoFileComp ShellCompDirective // suppress fallback filename completion
func (c *Command) RegisterFlagCompletionFunc(flagName string, f CompletionFunc) error
```

This task covers, in order: (1) `<run-id>` positional completion, (2) `<question-id>` positional
completion for `answer`, (3) fixed-value flag completion for the enum flags, (4) host-history-based
flag completion for `--image` and `--allow`, (5) an optional addition if `krayt image rm` already
exists in this checkout, and (6) what's explicitly **not** covered and why.

---

## 1. `<run-id>` completion

Commands and their run-id argument, all `Args: cobra.ExactArgs(1)` unless noted:

- `internal/cli/apply.go` `newApplyCmd` — `Use` `:17`, `Args` `:21`, `--repo` flag `:39`.
- `internal/cli/manage.go`:
  - `newLogsCmd` — `Use` `:74`, `Args` `:76`, `--repo` `:90`.
  - `newAttachCmd` — `Use` `:97`, `Args` `:99`, `--repo` `:116`.
  - `newStopCmd` — `Use` `:123`, `Args` `:125`, `--repo` `:150`.
  - `newRmCmd` — `Use` `:158`, `Args` `:160`, `--repo` `:181`, `--force` `:182`.
  - `newPatchCmd` — `Use` `:189`, `Args` `:191`, `--repo` `:205`.
- `internal/cli/questions.go` `newQuestionsCmd` — `Use` `:22`, `Args` `:25`, `--repo` `:92`.
- `internal/cli/answer.go` `newAnswerCmd` — `Use` `:17`, `Args: cobra.RangeArgs(1, 3)` (`:22`),
  `--repo` `:66`. The run-id is always `args[0]`; `args[1]`/`args[2]` are handled in §2 below.

Every command above registers its `--repo` flag with the identical name `"repo"` (default `.`),
resolved via `stateDir(repo)` (`internal/cli/manage.go:21-27`). Runs are read via
`orchestrator.List(sd)` (`internal/orchestrator/state.go:132`, returns `[]RunRecord` newest-first).
`RunRecord` (`state.go:30-50`) has `ID`, `State`, `ImageRef`, `Network NetworkMeta{Mode, Allow
[]string}`; `Terminal()` (`state.go:85-87`) reports `done`/`failed`/`timed_out`; `StateWaiting`
(`state.go:19`) is the state `answer` acts on.

**Design (already decided):**

1. Filtering per command:
   - `stop`, `attach` → only non-terminal runs (`!rec.Terminal()`).
   - `answer` → only `rec.State == orchestrator.StateWaiting`.
   - `rm` → only terminal runs (`rec.Terminal()`) **unless `--force` is already present on the
     command line** (`cmd.Flags().GetBool("force")` — cobra has already parsed flags typed so far
     when it invokes `ValidArgsFunction`), in which case all runs.
   - `logs`, `patch`, `apply`, `questions` → all runs, no filtering.
2. Each suggestion: `cobra.CompletionWithDesc(rec.ID, rec.State+", "+truncate(rec.ImageRef, 40))`.
3. Sort newest-first — `orchestrator.List` already returns that order; preserve it.
4. Only the first positional argument gets id completion; `len(args) >= 1` → `(nil,
   ShellCompDirectiveNoFileComp)`.
5. Any error reading `.krayt` state → `(nil, ShellCompDirectiveNoFileComp)` — fail silently to "no
   suggestions", never print an error or disrupt the shell.

**Implement** — new file `internal/cli/complete.go`:

```go
package cli

import (
	"github.com/spf13/cobra"

	"github.com/418-cloud/krayt/internal/orchestrator"
)

// completeRunIDs returns a cobra ValidArgsFunction that completes <run-id> from the command's
// --repo, newest-first, annotated with "<state>, <image-ref>". keep filters which runs are
// suggested; pass nil to suggest every run.
func completeRunIDs(keep func(rec orchestrator.RunRecord, cmd *cobra.Command) bool) func(
	cmd *cobra.Command, args []string, toComplete string,
) ([]string, cobra.ShellCompDirective) {
	return func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		if len(args) >= 1 {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		repo, _ := cmd.Flags().GetString("repo")
		sd, err := stateDir(repo)
		if err != nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		recs, err := orchestrator.List(sd)
		if err != nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		var out []string
		for _, rec := range recs {
			if keep != nil && !keep(rec, cmd) {
				continue
			}
			out = append(out, cobra.CompletionWithDesc(rec.ID, rec.State+", "+truncate(rec.ImageRef, 40)))
		}
		return out, cobra.ShellCompDirectiveNoFileComp
	}
}

// truncate shortens s for a completion description so one long value can't break the shell's
// completion-list formatting.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}
```

Wire into each command's `ValidArgsFunction` field, next to `Args`/`RunE`:

- `internal/cli/manage.go`:
  - `newLogsCmd`, `newPatchCmd` → `completeRunIDs(nil)`.
  - `newAttachCmd`, `newStopCmd` → `completeRunIDs(func(rec orchestrator.RunRecord, _ *cobra.Command) bool { return !rec.Terminal() })`.
  - `newRmCmd` → `completeRunIDs(func(rec orchestrator.RunRecord, cmd *cobra.Command) bool { if rec.Terminal() { return true }; force, _ := cmd.Flags().GetBool("force"); return force })`.
- `internal/cli/apply.go` `newApplyCmd` → `completeRunIDs(nil)`.
- `internal/cli/questions.go` `newQuestionsCmd` → `completeRunIDs(nil)`.
- `internal/cli/answer.go` `newAnswerCmd` → see §2 (a two-stage function that covers both the
  run-id and question-id positions).

---

## 2. `<question-id>` completion for `answer`

`answer <run-id> [<question-id>] <response>` (`internal/cli/answer.go:17-22`) is positionally
polymorphic: `resolveAnswerArgs` (`:77-95`) treats a single arg after the run-id as the
**response** (using the newest pending question) unless `--no-answer` is set, and only treats it as
a **question id** when a second arg follows. Completion cannot know in advance which the user
intends — that's fine and normal for shell completion (it offers useful candidates; the user can
always type something else). Offer pending question IDs as completions for `args[1]`; the user
either accepts one (2-arg form) or ignores the suggestion and types the response directly (1-arg
form).

`orchestrator.ReadQuestions(runDir)` (`internal/orchestrator/questions.go:83`) returns
`[]QuestionRecord{ID, Prompt, Choices, AskedAt, Response, NoAnswer, AnswerAt}` (`:17-25`), oldest
first. A question is pending when `AnswerAt == ""` (mirrors `isPending`,
`internal/cli/questions.go:99`, already in package `cli` — reuse it directly).

**Prompts are agent-originated, untrusted text — sanitize before it ever reaches a completion
description**, exactly as `newQuestionsCmd` already does for display (`orchestrator.Sanitize`,
`internal/orchestrator/report.go:179`, used at `questions.go:67,71,81`). An unsanitized prompt
echoed into a completion description is attacker-controlled text rendered in the developer's
terminal — sanitize it the same way existing output does.

**Implement** — add to `internal/cli/complete.go`:

```go
// completeQuestionIDs completes args[1] of `answer <run-id> [<question-id>] <response>` with
// the run's pending question IDs, annotated with a sanitized, truncated prompt snippet.
func completeQuestionIDs(cmd *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
	if len(args) != 1 { // only right after <run-id>
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	repo, _ := cmd.Flags().GetString("repo")
	sd, err := stateDir(repo)
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	qs, err := orchestrator.ReadQuestions(orchestrator.RunDir(sd, args[0]))
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	var out []string
	for _, q := range qs {
		if !isPending(q) {
			continue
		}
		out = append(out, cobra.CompletionWithDesc(q.ID, truncate(orchestrator.Sanitize(q.Prompt), 40)))
	}
	return out, cobra.ShellCompDirectiveNoFileComp
}
```

Wire `newAnswerCmd`'s `ValidArgsFunction` (`internal/cli/answer.go`) as a two-stage dispatcher
(cannot just use `completeRunIDs` alone, since position 1 needs different logic):

```go
cmd.ValidArgsFunction = func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) == 0 {
		return completeRunIDs(func(rec orchestrator.RunRecord, _ *cobra.Command) bool {
			return rec.State == orchestrator.StateWaiting
		})(cmd, args, toComplete)
	}
	if len(args) == 1 {
		return completeQuestionIDs(cmd, args, toComplete)
	}
	return nil, cobra.ShellCompDirectiveNoFileComp // args[2] (the response) is free text
}
```

---

## 3. Fixed-value flag completion (enums already defined as Go constants)

Every value below is already validated by an existing `Parse*`/switch function — reuse those
constants directly so completion can't drift from what's actually accepted; do not hand-write a
second copy of the literal strings.

| Flag | Command | Source of truth | File:line |
|---|---|---|---|
| `--net` | `run` | `task.NetworkAllowlist/Full/None` | `internal/task/spec.go:121-124`, used by `internal/cli/run.go:100` |
| `--on-question` | `run` | `task.QuestionFail/Wait` | `internal/task/spec.go:75-77`, used by `run.go:109` |
| `--on-question-timeout` | `run` | `task.OnTimeoutSentinel/Abort` | `internal/task/spec.go:95-96`, used by `run.go:111` |
| `--agent` | `run` | `adapter.Get`'s cases (`none`, `claude-code`, `gemini-cli`) | `internal/adapter/adapter.go:44-55`, used by `run.go:112` |
| `--sort` | `questions` | `validateSort`'s cases (`asked`, `pending-first`, `pending-last`) | `internal/cli/questions.go:102-109`, used by `questions.go:94` |

**Implement:**

1. `internal/adapter/adapter.go`: add an exported enumerator next to `Get` so the two can't drift
   (there is no existing one):
   ```go
   // Names lists every valid adapter name (the config/flag values Get accepts), for shell
   // completion. Keep in sync with Get's switch — small enough that duplication here is the
   // simplest way to keep both colocated and reviewable together.
   func Names() []string { return []string{"none", "claude-code", "gemini-cli"} }
   ```
2. `internal/cli/questions.go`: extract `validateSort`'s literals to a package-level var so
   completion and validation share one list instead of two hand-kept copies:
   ```go
   var sortModes = []string{"asked", "pending-first", "pending-last"}
   ```
   and rewrite `validateSort` (`:102-109`) to check membership in `sortModes` instead of a literal
   switch (keep its error message format).
3. In `internal/cli/run.go` `bindRunFlags` (`:88-113`), after the existing flag registrations,
   register fixed completions:
   ```go
   _ = cmd.RegisterFlagCompletionFunc("net", cobra.FixedCompletions(
       []string{string(task.NetworkAllowlist), string(task.NetworkFull), string(task.NetworkNone)},
       cobra.ShellCompDirectiveNoFileComp))
   _ = cmd.RegisterFlagCompletionFunc("on-question", cobra.FixedCompletions(
       []string{string(task.QuestionFail), string(task.QuestionWait)},
       cobra.ShellCompDirectiveNoFileComp))
   _ = cmd.RegisterFlagCompletionFunc("on-question-timeout", cobra.FixedCompletions(
       []string{string(task.OnTimeoutSentinel), string(task.OnTimeoutAbort)},
       cobra.ShellCompDirectiveNoFileComp))
   _ = cmd.RegisterFlagCompletionFunc("agent", cobra.FixedCompletions(
       adapter.Names(), cobra.ShellCompDirectiveNoFileComp))
   ```
   (`RegisterFlagCompletionFunc` only errors on an unknown flag name or a double-registration —
   both are programmer errors caught immediately by any test/manual run, safe to discard here, same
   style as this codebase's other best-effort `_ = ...` writes.)
4. In `internal/cli/questions.go` `newQuestionsCmd` (`:18-96`), after the flag registrations:
   ```go
   _ = cmd.RegisterFlagCompletionFunc("sort", cobra.FixedCompletions(sortModes, cobra.ShellCompDirectiveNoFileComp))
   ```

---

## 4. History-based flag completion: `--image` and `--allow`

Unlike §3, these have no fixed set of valid values — but this repo's own run history
(`.krayt/runs/` under `--repo`) already has real examples the developer is likely to reuse.

**`--image`** (`run.go:90`, `fl.StringVar(&f.image, "image", "", ...)`): complete with the
distinct `ImageRef` values from this repo's run history, most-recently-used first (`ImageRef` is
the raw `--image` string a prior run used — a tag or digest reference, see `RunRecord`,
`internal/orchestrator/state.go:32`).

**`--allow`** (`run.go:101`, `fl.StringArrayVar(&f.allow, "allow", nil, ...)`, repeatable):
complete with the union of (a) domains from this repo's run history (`RunRecord.Network.Allow`,
`state.go:35,54-56`) and (b) a small static seed list of domains already documented elsewhere in
this repo as common egress needs — cite each on addition, don't invent new ones:
`api.anthropic.com` and `generativelanguage.googleapis.com` (`KRAYT_SPEC.md:409`),
`proxy.golang.org` and `sum.golang.org` (`hack/krayt-dev/README.md:70`), `cache.nixos.org`,
`github.com`, `codeload.github.com` (`hack/krayt-dev/README.md:116`).

**Implement** — add to `internal/cli/run.go` (near `bindRunFlags`):

```go
// wellKnownAllowDomains seeds --allow completion with domains already documented in this repo
// as common egress needs (README.md, KRAYT_SPEC.md §6.6, hack/krayt-dev/README.md). Not
// authoritative or exhaustive — a completion convenience layered under the repo's own run
// history, which always takes priority.
var wellKnownAllowDomains = []string{
	"api.anthropic.com",
	"generativelanguage.googleapis.com",
	"proxy.golang.org",
	"sum.golang.org",
	"cache.nixos.org",
	"github.com",
	"codeload.github.com",
}

func completeImageRef(cmd *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
	repo, _ := cmd.Flags().GetString("repo")
	sd, err := stateDir(repo)
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	recs, err := orchestrator.List(sd)
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	seen := map[string]bool{}
	var out []string
	for _, rec := range recs { // already newest-first
		if rec.ImageRef == "" || seen[rec.ImageRef] {
			continue
		}
		seen[rec.ImageRef] = true
		out = append(out, rec.ImageRef)
	}
	return out, cobra.ShellCompDirectiveNoFileComp
}

func completeAllowDomain(cmd *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
	seen := map[string]bool{}
	var out []string
	add := func(d string) {
		if d == "" || seen[d] {
			return
		}
		seen[d] = true
		out = append(out, d)
	}
	if repo, _ := cmd.Flags().GetString("repo"); repo != "" {
		if sd, err := stateDir(repo); err == nil {
			if recs, err := orchestrator.List(sd); err == nil {
				for _, rec := range recs {
					for _, d := range rec.Network.Allow {
						add(d)
					}
				}
			}
		}
	}
	for _, d := range wellKnownAllowDomains {
		add(d)
	}
	return out, cobra.ShellCompDirectiveNoFileComp
}
```

Register both in `bindRunFlags`, alongside §3's registrations:

```go
_ = cmd.RegisterFlagCompletionFunc("image", completeImageRef)
_ = cmd.RegisterFlagCompletionFunc("allow", completeAllowDomain)
```

---

## 5. Optional: `krayt image rm <digest>` — only if it already exists in this checkout

A sibling task, `docs/ai-tasks/prune-cached-images.md`, specs `krayt image ls/rm/prune` for the
host-side vmimage/container image caches. Digest completion for `image rm <digest>` is exactly as
natural as everything else in this task — **but only add it if that task has already landed**:

- Check whether `internal/cli/image_rm.go` (or equivalent — a `newImageRmCmd` in package `cli`)
  and an `internal/imagecache` package already exist in this checkout.
- **If they don't exist yet, skip this section entirely** — it is not a prerequisite for the rest
  of this task, and there is nothing to wire completion onto.
- If they do exist, add a `ValidArgsFunction` to `newImageRmCmd` that lists cached digests from
  `imagecache.List` across both cache roots (mirror this task's `completeRunIDs` shape: only
  complete `args[0]`, return `ShellCompDirectiveNoFileComp`, annotate each digest with its `KIND`
  and `SIZE` via `cobra.CompletionWithDesc`).

---

## 6. Explicitly out of scope (already covered, or deliberately not done)

- `--repo`, `--task`, `--config`, `--secrets` — all take filesystem paths; cobra falls back to the
  shell's normal filename/directory completion for any flag with no registered completion function
  (`RegisterFlagCompletionFunc` is opt-in per flag; leaving these alone is correct, not an
  oversight). Do not add file-completion registrations for these — they'd only duplicate what the
  shell already does.
- `krayt image pull --ref`/`--digest` — there is essentially only ever one meaningful value (the
  pinned base image), so completion adds negligible value; left alone.
- `answer`'s response text (final positional arg) — free text, not completable; already returns no
  suggestions (§2).

---

## Tests

Add `internal/cli/complete_test.go` (mirror `manage_test.go`'s `seedRun`/`run` helpers — extend
`seedRun` if needed so a test can also set a specific `image_ref` and `network.allow` in the seeded
`meta.json`, and add a helper to seed a run's `questions/<qid>.json` matching the shape
`orchestrator.ReadQuestions` expects):

- **Run-id completion:** seed runs in states `done`, `running`, `waiting`, `failed`; call each
  command's `ValidArgsFunction` directly with `args: nil` (cobra exposes it as a plain field — no
  need to drive real shell completion) and assert the filtering rules from §1 (`stop` excludes
  terminal runs; `rm` excludes non-terminal unless `--force` is set via
  `cmd.Flags().Set("force", "true")`; `answer` includes only `waiting`; `logs`/`patch`/`questions`/
  `apply` include everything). Assert `args: []string{"already-here"}` returns `(nil,
  ShellCompDirectiveNoFileComp)` for every command. Assert a `--repo` with no `.krayt` yet returns
  `(nil, ShellCompDirectiveNoFileComp)`, not an error.
- **Question-id completion:** seed a `waiting` run with two questions, one answered
  (`AnswerAt` set) and one pending; call `newAnswerCmd().ValidArgsFunction(cmd, []string{runID},
  "")` and assert only the pending question's ID is suggested, with its (sanitized) prompt as the
  description. Seed a prompt containing characters `orchestrator.Sanitize` strips/escapes and
  assert the completion description reflects the sanitized form, not the raw prompt.
- **Fixed-value flag completion:** for `run`, fetch the registered completion func for each of
  `net`/`on-question`/`on-question-timeout`/`agent` (cobra exposes registered flag completions via
  `cmd.GetFlagCompletionFunc(name)` — check this accessor exists in the vendored version; if not,
  call `RegisterFlagCompletionFunc` again and capture the func, or invoke completion end-to-end via
  cobra's `__complete` mechanism) and assert it returns exactly the expected fixed set. Same for
  `questions`' `--sort`.
- **History-based flag completion:** seed two runs with different `image_ref` values and assert
  `completeImageRef` returns both, newest first, deduplicated. Seed a run with `network.allow:
  ["example.internal"]` and assert `completeAllowDomain` includes it alongside the well-known seed
  list, deduplicated.

## Docs (required)

- `README.md`: add a short "Shell completion" subsection (near "Quick start" or "Running an
  agent") with the standard cobra setup lines, e.g.:
  ```sh
  # bash (Homebrew bash-completion@2, or add to ~/.bashrc)
  krayt completion bash > "$(brew --prefix)/etc/bash_completion.d/krayt"
  # zsh (macOS default shell)
  krayt completion zsh > "${fpath[1]}/_krayt"
  ```
  plus a short note on what's dynamic (run IDs, question IDs, `--image`/`--allow` history) versus
  static (enum flags, command/flag names) and that repo-scoped completions are scoped to
  `--repo`/`.`.
- `KRAYT_SPEC.md` §13: note that the run-scoped commands and `run`'s enum/history flags support
  dynamic shell completion.
- `docs/ai-tasks/README.md`: add this task to the top table with a status.

## Verify (offline)

```sh
go build ./...
GOOS=linux GOARCH=arm64 go build ./...
go test -race ./...
golangci-lint run
```

No new dependency — cobra's completion support is already vendored (`go.mod:14`). Runs fully
offline.

**Manual smoke test (real terminal, cannot be driven from the sandbox — log it in `HUMAN_TODO.md`,
do not fabricate a result):** after `go install`, `source <(krayt completion zsh)` (or the bash
equivalent) in an interactive shell inside a repo with a running/waiting run and at least one
pending question, confirm: `krayt stop <TAB>` shows the live run with its description; `krayt
answer <run-id> <TAB>` shows the pending question id; `krayt run --net <TAB>` shows `allowlist
full none`.

## Done when

- `<run-id>` completes correctly, filtered per §1, on every command listed there.
- `<question-id>` completes correctly for `answer`'s second position, per §2, with sanitized
  descriptions.
- `--net`, `--on-question`, `--on-question-timeout`, `--agent`, and `questions --sort` complete
  their exact fixed value sets, sourced from existing constants (no duplicated literals).
- `--image` and `--allow` complete from this repo's run history, merged with the `--allow` seed
  list for the latter.
- All of the above is unit-tested offline (no registry, no VM, no real shell); `go build`/`go test
  -race`/`golangci-lint run` pass for both the host and `linux/arm64` guest target.
- `README.md` documents shell setup; the real-terminal smoke test is logged in `HUMAN_TODO.md`, not
  fabricated.

## Constraints

- Host-side CLI only.
- Do not disable or replace cobra's default `completion` command — this task only adds
  `ValidArgsFunction`/`RegisterFlagCompletionFunc` to existing commands/flags; it does not touch
  `CompletionOptions`.
- Any agent-originated text reaching a completion description (question prompts) must go through
  `orchestrator.Sanitize` first — no exceptions.
- Don't invent new "well-known" `--allow` domains beyond what's already documented elsewhere in
  this repo (§4) — cite the source for each, don't guess at others.
- Small, focused diff per section above; §5 is genuinely optional and conditional — do not block
  the rest of the task on it.
