# krayt — working agreement

This repo implements `krayt`, specified in full in KRAYT_SPEC.md. Read it before working.

- Follow the implementation protocol in §14. Maintain HUMAN_TODO.md at the repo root.
  For [HUMAN]-tagged or otherwise human-only steps (Apple code-signing, Nix image
  build/boot, registry creds, real-hardware tests, live API keys): do everything around
  them, append a structured HUMAN_TODO.md entry, and pause-and-ask if it blocks. Never
  fake a result (no fake signatures, digests, or "boot succeeded").
- Use the pinned dependencies in §9.1. Do not guess libraries or versions.
- Work ONE phase at a time. A phase is done only when its "Done when" criterion passes —
  prefer an automated test. Stop at phase boundaries for review.
- Keep the OS-agnostic core build-tag-clean: provider/vz is darwin-only, guest/* is
  linux-only, everything else compiles on both.