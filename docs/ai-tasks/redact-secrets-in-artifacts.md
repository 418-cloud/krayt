# Task: redact secrets from report.md and question text; scan the patch and warn

**Read `CLAUDE.md` and `KRAYT_SPEC.md` (§6.8 secrets, §6.13 ask_human, §8.4 run artifacts, §10) first.
Proceed autonomously. Where a full end-to-end check needs a real Mac, write the test and hand it off
via `HUMAN_TODO.md` (§14).**

## Reason (the finding)

**Finding (Medium) — secret confinement gap.** §10 claims *"no secret can reach the returned patch,
report.md, meta.json, question text, or a filename."* In the code, the redactor
(`internal/secrets/secrets.go:99-113`) is applied **only to streamed container log lines**
(`internal/guest/service.go:311-317`). Everything else the agent controls is un-redacted:

- The agent-written `report.md` is collected verbatim by `CollectArtifacts`/`tarDir`
  (`internal/guest/service.go:484-496`, `:566-615`) and folded into the final report on the host
  (`internal/orchestrator/report.go:20-77`).
- `ask_human` **question prompts/choices** flow through the bridge un-redacted
  (`internal/guest/ask/ask.go:61-83`, pushed at `internal/guest/service.go:280-284`).
- Secret values the agent writes into a source file appear in `changes.patch`.

So a secret the agent copies into its report, an `ask_human` prompt, or a tracked file reaches the
host artifacts in clear text.

## Goal

Extend redaction to the artifacts that *can* be safely redacted, and warn (not corrupt) on the one
that can't:

1. **Redact `report.md`** (agent-written notes) with the existing redactor.
2. **Redact `ask_human` question prompt + choices** with the existing redactor before they leave the
   VM.
3. **Scan `changes.patch`** for secret values; if any appear, surface a **warning in the report's
   Safety section** — do **not** redact the patch (mutating hunks breaks `git apply`).

Keep all secret-value handling **in the guest** (secrets are confined there per §6.8; the host does
not retain secret values — `internal/orchestrator/orchestrator.go:283-296` loads and pushes them
transiently). The guest already has the values (`s.secrets`) and builds the redactor in `Start`.

## Current behavior (grounding)

- Redactor built in `Start`: `internal/guest/service.go:244` (`secrets.NewRedactor(...)`), used only
  for logs at `:311-317`.
- Artifacts built in the guest: `buildArtifacts` writes `changes.patch` (+ optional `commits.bundle`)
  at `internal/guest/service.go:470-482`; the agent's `report.md` is whatever it wrote to `/output`.
- Artifacts streamed by `CollectArtifacts` → `tarDir` (`:484-496`, `:566-615`).
- Host folds `report.md` and builds Safety from `patch.Lint`:
  `internal/orchestrator/orchestrator.go:214-222`, `internal/orchestrator/report.go:47-53,68-74`.
- Redactor known limitation (acknowledged, keep it): a value split across two chunks is not caught
  (`internal/secrets/secrets.go:99-101`). Since we now redact whole files (not streamed chunks), the
  report/patch scan see the complete bytes — the split-chunk gap only remains for live logs.

## Implement (guest-side)

1. **Store the redactor on the Service** so post-run steps can use it. Add a field
   `redactor *secrets.Redactor` set in `Start` right after it's built (`service.go:244`). Also keep
   the raw secret values available to the artifact steps (already on `s.secrets`, captured into a
   local in `Start` at `:215`).

2. **Redact `report.md` before it is collected.** After the run, before/inside `CollectArtifacts`
   (or at the end of `Start`, before returning), if `<outputDir>/report.md` exists, read it, apply
   `s.redactor.Redact`, and write it back. Doing it in the guest keeps the value inside the VM. (If
   you prefer, redact within `tarDir` for the `report.md` entry specifically — but rewriting the file
   once, after the run, is simpler and also fixes the copy that stays in `/output`.)

3. **Redact question prompt + choices** at the bridge boundary. In `Start`, the bridge push closure
   is at `service.go:280-284`:
   ```go
   bridge := ask.NewBridge(func(id, prompt string, choices []string) error {
       return es.send(&pb.RunEvent{Kind: &pb.RunEvent_Question{Question: &pb.Question{
           Id: id, Prompt: prompt, Choices: choices,
       }}})
   })
   ```
   Redact `prompt` and each `choices[i]` with `s.redactor.Redact` (byte→string) before constructing
   the `pb.Question`. This covers both live display and the persisted `questions/<id>.json` (the host
   persists what it receives — `internal/orchestrator/questions.go:30-38`). Answers come from the
   human (host side), not the agent, so they are not a secret-leak path and need no redaction.

4. **Scan `changes.patch` and emit a Safety warning (no redaction of the patch).** In
   `buildArtifacts` (`service.go:470-482`), after writing `changes.patch`, scan its bytes for any
   secret value (reuse the redactor's value set — add a `secrets.Redactor.Contains(b []byte) []int`
   or a small `ScanKeys` helper that reports **which secret KEYS** matched; key *names* are not
   secret, values must never be written out). If any match, write a small marker file into the output
   dir, e.g. `<outputDir>/secret-scan.json`:
   ```json
   { "patch_contains_secret_keys": ["ANTHROPIC_API_KEY"] }
   ```
   Never write the values. The patch itself is left byte-exact.

5. **Host surfaces the warning in Safety.** In the finalizer
   (`internal/orchestrator/orchestrator.go:212-223`), after the `patch.Lint` loop, read
   `secret-scan.json` from the run dir (it is collected with the other artifacts) and append a Safety
   line per key, e.g.:
   `rec.Safety = append(rec.Safety, "changes.patch contains the value of secret ANTHROPIC_API_KEY — review before applying")`.
   These flow through the already-sanitized Safety rendering (`report.go:47-53`) and into
   `meta.json`/`report.md`. Ensure the marker file is either excluded from the returned tree or is
   harmless if left in the run dir (it names keys only).

To keep `internal/secrets` reusable for the scan, add the scan/contains helper there (host and guest
both import it) and unit-test it directly.

## Tests

Unit (no VM):
- `internal/secrets`: new `Contains`/`ScanKeys` — matches a full value, reports the right key(s),
  ignores empty values, and (documented) misses a value only if it never appears whole.
- Guest report redaction: seed the Service with a secret value, drop it into
  `<outputDir>/report.md`, run the collection step, assert the collected bytes contain
  `[REDACTED]` and not the value.
- Question redaction: drive a bridge question whose prompt/choices embed a secret value; assert the
  emitted `pb.Question` and the persisted record are redacted.
- Patch scan → Safety: build a `changes.patch` containing a secret value; assert `secret-scan.json`
  lists the key and the finalizer adds the Safety line; assert the patch bytes are unchanged.

Integration (real Mac, `HUMAN_TODO.md`): a run where the agent writes its credential into
`report.md`, an `ask_human` prompt, and a source file — inspect `report.md`, the question record,
and `changes.patch` in the run dir. Expect: report + question redacted; patch unchanged but flagged
in Safety.

## Docs (required)

- `KRAYT_SPEC.md` §6.8: state precisely what redaction covers — live logs, `report.md`, and
  `ask_human` prompt/choices are redacted in the guest; the **patch is scanned, not redacted**
  (redacting would corrupt it), and a hit is reported in the report's Safety section. Reword the §10
  claim so it no longer implies the patch is redacted.
- `KRAYT_SPEC.md` §8.4: note the Safety warning for secret-in-patch and the `secret-scan.json`
  marker.
- `KRAYT_SPEC.md` §10: update the "Secrets" row / residual notes to match (redaction scope + the
  known split-chunk limitation for live logs only).
- `docs/ai-tasks/README.md`: add this task.

## Verify (offline)

```sh
go build ./...
GOOS=linux GOARCH=arm64 go build ./...
go test -race ./...
golangci-lint run
```

## Done when

- `report.md` and question prompt/choices are redacted in the guest; the patch is scanned and a hit
  surfaces in Safety without altering the patch; unit tests pass; the end-to-end check is written and
  logged in `HUMAN_TODO.md`.
- KRAYT_SPEC §§6.8/8.4/10 updated (and the misleading §10 patch claim corrected).

## Constraints

- **Secret values never leave the guest un-redacted and are never written to any host artifact.** The
  scan reports **key names only**.
- No new dependency. Keep `internal/secrets` OS-agnostic.
- Do not change the redactor's live-log behavior; you are adding coverage, not altering existing
  redaction.
