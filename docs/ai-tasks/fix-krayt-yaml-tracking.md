# Task: stop the tracked `krayt.yaml` from masquerading as a gitignored local file

**Read `CLAUDE.md` and `KRAYT_SPEC.md` (§8.1 config) first. Proceed autonomously. Small, self-contained
repo-hygiene change.**

## Reason (the finding)

**Finding (Low) — misleading tracked config.** The repo-root `krayt.yaml` is **tracked** in git
(`git ls-files` lists it), yet its own header comment says
*"PERSONAL/local config (gitignored): it pins a specific dev-image build + your secrets path."*
(`krayt.yaml:5`). The claim is false and dangerous: a user who trusts the comment and later inlines a
token into the `env:` block would commit a secret. Today the file carries no secret *value* (only
`secrets: secrets.env`, an image pin, and non-secret env), and the actual secret file `secrets.env`
is correctly gitignored and was never committed (verified) — but the mismatch should be removed before
release. (`configs/krayt.yaml` is the intended checked-in example and is fine.)

## Goal

Make the repo state match intent: either the root `krayt.yaml` is genuinely local (gitignored,
untracked) or it is clearly labeled as a tracked example. Prefer the former, since the comment and
its contents (a personal dev-image pin + secrets path) describe a local file.

## Implement (pick option A unless the maintainer wants a tracked sample)

### Option A (recommended): make it truly local

1. Add `/krayt.yaml` to `.gitignore` (the root one — it already ignores `secrets.env`, `.env`,
   `.krayt/`).
2. `git rm --cached krayt.yaml` (stop tracking; keep the working-tree file).
3. Keep `configs/krayt.yaml` as the canonical committed example (already present and correct).
4. Ensure the README / any docs that tell users how to configure point at `configs/krayt.yaml` as the
   template and describe the root `krayt.yaml` as an auto-loaded **local** override (§8.1/§8.3).

### Option B (only if a tracked root sample is wanted)

1. Replace the root `krayt.yaml` contents with a non-personal, secret-free example (no real image
   digest pin tied to one dev, no `secrets:` pointing at a real path), and change the comment to say
   it is a **tracked example**, removing the false "gitignored" wording.
2. Leave it tracked.

Do **not** leave the current state (tracked file claiming to be gitignored).

## Docs (required)

- `KRAYT_SPEC.md` §8.1/§8.3: make the auto-loaded root `krayt.yaml` vs the `configs/krayt.yaml`
  example explicit, and state that the root file is a **local, gitignored** override (Option A) — so
  the "auto-loaded from `<repo>/krayt.yaml`" behavior is documented without implying users should
  commit it.
- `docs/ai-tasks/README.md`: add this task.

## Verify

```sh
git check-ignore krayt.yaml        # Option A: prints "krayt.yaml"
git ls-files | grep -x krayt.yaml  # Option A: no output (untracked)
go build ./...                     # confirm nothing else changed
```

Also re-confirm no secret material is tracked:
```sh
git ls-files | grep -iE 'secret|\.env'   # expect only source .go files, not secrets.env
git log --all -p -- secrets.env | head   # expect empty (never committed)
```

## Done when

- The root `krayt.yaml` is either untracked+gitignored (Option A) or a clearly-labeled secret-free
  tracked example (Option B); no tracked file claims to be gitignored; §8.1/§8.3 updated.

## Constraints

- Do not touch `secrets.env` handling beyond confirming it stays gitignored/untracked.
- Rotating the live-looking token currently in the working-tree `secrets.env` is a **human** action —
  it is logged in `HUMAN_TODO.md`, not part of this task.
