# claude-code — real-world agent image for the `claude-code` adapter

A real agent image that runs **Claude Code** headlessly inside a krayt micro-VM against a task,
edits `/workspace`, and returns a reviewable `changes.patch`. It's the image behind the
`HUMAN_TODO.md` entry **"[Phase 5] Agent adapter end-to-end with live credentials"** and exercises
the `--agent claude-code` path end to end: the host adapter's exactly-one auth check (§6.14), the
secrets → tmpfs → env credential hand-off (§8.2), and a real task completing with patch + report +
meta (§8.4).

Unlike `ask-probe` (which drives the question channel), this image is a genuine coding agent, so
it needs a **live Anthropic credential** and **egress to the model API**.

## What the image does
The entrypoint (`entrypoint.sh`, baked in) exports the credential from `/run/secrets` into the
environment, then runs:
```sh
claude -p "$(cat /task/prompt.md)" --dangerously-skip-permissions | tee /output/report.md
```
- `-p` = non-interactive/print mode; `--dangerously-skip-permissions` lets it edit autonomously
  (safe: the whole run is already isolated in the VM).
- Claude's final summary lands in `/output/report.md`, which krayt folds into the run report's
  **Notes** (§8.4).
- It runs as a **non-root** user (Claude Code refuses uid 0, §8.2).

## Prerequisites
- Apple-Silicon Mac with `krayt` built (`go build -o bin/krayt ./cmd/krayt`).
- The base micro-VM image built + pinned (same one Phase 2/3 used).
- A container registry the Mac can pull from.
- A **live** `ANTHROPIC_API_KEY` (a scoped Console key is the safe default, §6.14) **or**
  `CLAUDE_CODE_OAUTH_TOKEN` from `claude setup-token` — **exactly one**.

## 1. Build + push the image (linux/arm64)
```sh
cd hack/claude-code
docker buildx build --platform linux/arm64 -t <your-registry>/krayt-claude-code:latest --push .
```
`--platform linux/arm64` matters: the VM is arm64.

## 2. Secrets file (never committed)
Put **exactly one** credential in `secrets.env` (gitignored here):
```sh
echo "ANTHROPIC_API_KEY=sk-ant-…" > /tmp/claude-demo/secrets.env
```
krayt streams it in the `SecretsBundle`, lands it on tmpfs at `/run/secrets/ANTHROPIC_API_KEY`,
and the entrypoint exports it. It never touches the VM disk and is redacted from logs (§6.8).

## 3. A repo + task for the run
```sh
mkdir -p /tmp/claude-demo && cd /tmp/claude-demo
git init -q && printf 'print("hi")\n' > main.py && git add -A && git commit -qm init
cp <krayt>/hack/claude-code/task.example.md task.md    # or write your own
```

## 4. Run it
```sh
krayt run \
  --image <your-registry>/krayt-claude-code:latest \
  --agent claude-code \
  --secrets /tmp/claude-demo/secrets.env \
  --task task.md --repo . \
  --allow api.anthropic.com
```
- `--agent claude-code` runs the host adapter's pre-flight: it validates **exactly one** auth
  credential is in the secrets file, **before any VM boots** (§6.14).
- `--allow api.anthropic.com` opens the egress allowlist to the model API (§6.6). An
  **OAuth token** may also need `console.anthropic.com` / `claude.ai`; a scoped **API key**
  needs only `api.anthropic.com`.
- Add `--detach` to background it ("park and walk away", §6.2).
- To pick a cheaper model, add `env:\n  ANTHROPIC_MODEL: claude-haiku-4-5` to a `krayt.yaml`
  (container env comes from the config file, §8.1).

## Success looks like
- `krayt ls` → the run reaches `done` with `EXIT 0`.
- `krayt patch <id>` → a `changes.patch` with Claude's edits that applies cleanly onto the repo.
- `.krayt/runs/<id>/report.md` → the fixed sections **plus Claude's summary under `## Notes`**.
- `.krayt/runs/<id>/meta.json` → full §8.4 schema (image, task summary, resources, patch stats).
- The API key never appears in `agent.log`, `report.md`, or `meta.json` (§6.8 redaction).

## Prove the exactly-one auth guard (fails fast, no VM boot)
Put **both** credentials in the secrets file and run again — krayt must refuse before booting:
```sh
printf 'ANTHROPIC_API_KEY=a\nCLAUDE_CODE_OAUTH_TOKEN=b\n' > /tmp/claude-demo/secrets.env
krayt run --image <ref> --agent claude-code --secrets /tmp/claude-demo/secrets.env --task task.md --repo .
# → error: claude-code: 2 auth credentials set (…); set exactly one … (§6.14)
```

## Entrypoint exit codes (if it isn't 0)
| exit | meaning |
|------|---------|
| 0  | success |
| 66 | task file `/task/prompt.md` missing (EX_NOINPUT) |
| 78 | no credential in `/run/secrets` (EX_CONFIG). If the secrets file is correct, this is the **non-root secrets-perms bug**: the base VM image needs the guest fix that writes `/run/secrets` world-readable so a non-root container can read it — see `HUMAN_TODO.md` "[Phase 5] Rebuild VM image for the non-root secrets-perms fix". Stopgap without a base rebuild: run this image as root (drop `USER agent`, add `ENV IS_SANDBOX=1`) and rebuild just this image. |
| other | Claude Code's own exit code (auth failure, API error, task failure) — see `krayt logs <id>` |

If Claude Code refuses `--dangerously-skip-permissions`, add `ENV IS_SANDBOX=1` to the Dockerfile
(the run genuinely is sandboxed) and rebuild.

## Cleanup
```sh
krayt rm <run-id>
docker rmi <your-registry>/krayt-claude-code:latest   # optional
```
