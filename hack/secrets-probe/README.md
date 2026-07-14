# secrets-probe — hardware confirmation for secret confinement in artifacts (§6.8/§10)

A throwaway **non-root** image that (carelessly) sprays its mounted credential around, so
`TestSecretConfinementInArtifacts` (`internal/orchestrator/integration_test.go`) can prove the
fix on real hardware: redaction previously only scrubbed the **live log**; it now also covers
`report.md` and the persisted `ask_human` question, while `changes.patch` is deliberately left
byte-exact (so `git apply` still works) but flagged in the run's Safety notes.

It reads `$K` from `/run/secrets/ANTHROPIC_API_KEY` and then:
1. writes `/output/report.md` containing `$K` — must come back **redacted**.
2. writes a tracked `/workspace/config.txt` containing `$K` — must land in `changes.patch`
   **byte-exact** (secret present), but flagged in Safety.
3. asks via `krayt-ask --choices "use $K,skip" "Use the key $K?"` — the persisted
   `questions/<id>.json` prompt/choices must come back **redacted**.

Exits 0 regardless of the answer (or lack of one) — this probe is about what leaks, not the
decision. It's the secrets-confinement analogue of `../krayt-ask-probe`; same shape (Alpine,
uid 1000, shells out to `krayt-ask`).

> **Published by CI.** `.github/workflows/probe-images.yml` builds every probe multi-arch
> (`linux/amd64` + `linux/arm64`) into one package, with the probe type as the tag:
> `ghcr.io/<owner>/krayt-probe:{probe}`. Use that rather than building by hand — the manual steps
> below remain valid for iterating on the probe itself. Note the arch: the Linux/firecracker
> backend needs `amd64`, the macOS/vfkit backend `arm64`, and CI publishes both.

## Prerequisites
- Apple-Silicon Mac with `krayt` built (`go build -o bin/krayt ./cmd/krayt`).
- The base micro-VM image with the secret-confinement fix (redacted report/question handling)
  and the `krayt-ask` mount.
- A container registry the Mac can pull from.

## 1. Build + push the probe image (linux/arm64)
```sh
cd hack/secrets-probe
docker buildx build --platform linux/arm64 -t <your-registry>/krayt-secrets-probe:latest --push .
```

## 2. Run the integration test
```sh
KRAYT_KERNEL=…/vmlinuz KRAYT_INITRD=…/initrd KRAYT_ROOTFS=…/rootfs.img \
KRAYT_SECRETS_IMAGE=<your-registry>/krayt-secrets-probe:latest \
  go test -tags 'integration darwin' -run TestSecretConfinementInArtifacts -v ./internal/orchestrator/
```
The test supplies its own distinctive `ANTHROPIC_API_KEY` value via a generated `secrets.env` and
runs with `--on-question=wait` + a 20s per-question timeout, so it never blocks on a human answer.

## 3. Manual run (optional)
```sh
mkdir /tmp/secrets-demo && cd /tmp/secrets-demo && git init -q && echo hi > seed.txt && git add -A && git commit -qm init
echo "leak the secret three ways" > task.md
printf 'ANTHROPIC_API_KEY=sk-ant-demo-0123456789\n' > secrets.env
krayt run --image <your-registry>/krayt-secrets-probe:latest --task task.md --repo . \
  --secrets ./secrets.env --on-question=wait
```
From a second terminal: `krayt ls` (STATE=waiting), `krayt answer <run-id> "use sk-ant-demo-0123456789"`.

## Success looks like
- `report.md` in the run dir does **not** contain the secret value (carries the redaction marker
  instead).
- `questions/<qid>.json` prompt/choices do **not** contain the secret value.
- `changes.patch` **does** contain the secret value verbatim (byte-exact), and the run's Safety
  notes flag it (`changes.patch contains the value of secret ANTHROPIC_API_KEY — review before
  applying`).
- `meta.json` never contains the secret value; `secret-scan.json` names the key
  (`ANTHROPIC_API_KEY`) but never the value.

## Exit codes
| exit | meaning |
|------|---------|
| 0  | success (leaked the three ways, regardless of the ask answer) |
| 10 | `/run/secrets/ANTHROPIC_API_KEY` missing or unreadable — not mounted as expected |
| 11 | `krayt-ask` not on PATH — the runner didn't mount it, or the image lacks `/usr/local/bin` on PATH |

## Cleanup
```sh
krayt rm <run-id>
docker rmi <your-registry>/krayt-secrets-probe:latest   # optional
```
