# Security Policy

krayt runs untrusted, agent-generated code inside disposable micro-VMs — security isolation is
the project's core promise, not an afterthought. See `KRAYT_SPEC.md` §10 ("Security Model") for
the full threat model, trust boundaries, and documented residual risks, and
`docs/ai-tasks/README.md` for the history of security-review findings and their fixes.

## Supported versions

krayt is pre-1.0. Only the **latest released version** receives security fixes — please upgrade
before reporting an issue, and confirm it still reproduces on the current release.

## Reporting a vulnerability

**Please do not open a public GitHub issue for a security vulnerability.**

Use GitHub's private vulnerability reporting instead: go to the
[Security tab](../../security) of this repository and click **"Report a vulnerability"**. This
opens a private draft security advisory visible only to you and the maintainers, so we can discuss
and fix the issue before any public disclosure.

Please include:
- A description of the vulnerability and its impact (which trust boundary it crosses — see §10).
- Steps to reproduce, or a minimal proof-of-concept image/task if the issue is specific to a run.
- The krayt version and platform (`krayt doctor` output is helpful).

## What to expect

This is a small, currently solo-maintained project — we can't commit to a fixed SLA, but reports
are taken seriously and handled as promptly as possible on a best-effort basis. We'll acknowledge
your report, work with you to understand and confirm it, and credit you in the fix (unless you'd
prefer otherwise) once it's public.

## Scope

In scope:
- Anything that breaks the isolation boundaries described in `KRAYT_SPEC.md` §10 — e.g. a
  container escaping to the guest VM, a guest-VM escape to the host, egress-allowlist bypass,
  secret leakage into a host artifact, or privilege escalation via a krayt-controlled path
  (the vfkit socket root, the control protocol, patch generation, etc.).
- Supply-chain concerns in this repo's own build/release pipeline (CI workflows, Nix image build).

Out of scope (already tracked, not new reports):
- Findings already listed as open in `docs/ai-tasks/README.md`'s security-review table.
- Behavior explicitly documented as an accepted residual risk in `KRAYT_SPEC.md` §10 (e.g. the
  single-layer, in-guest-only egress enforcement — there is no host/hypervisor network filter by
  design for v1).
- Vulnerabilities in a user-supplied agent image's own code, or in the agent/task the user chose
  to run — krayt's job is to contain untrusted code, not to vet it.
