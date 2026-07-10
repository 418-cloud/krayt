# Task: document that egress containment is single-layer (in-guest only)

**Read `CLAUDE.md` and `KRAYT_SPEC.md` (§6.6 networking, §10 security model) first. Proceed
autonomously. This is a documentation-only task — no code.**

## Reason (the finding)

**Finding (Medium) — no host-level egress backstop.** The vfkit VM is given a NAT NIC with full
host-network reachability (`internal/provider/vfkit/vfkit.go:175-178`,
`config.VirtioNetNew("")`), and **all** egress restriction lives *inside* the guest (the nftables
lock + the allowlist proxy). There is no hypervisor/host firewall. The two concrete bypasses that
originally motivated this finding — a container assuming the proxyd uid, and a guest-root escape via
container-writable `.git` config — are now **closed** (dropped `CAP_SETUID`/`CAP_SETGID` + enforced
non-root, see `harden-container-oci-spec.md` and `fix-egress-allowlist-bypass.md`; isolated root-only
`patchgit`, see `fix-guest-git-config-rce.md`). That does not remove the architectural gap: the fix
for both lives entirely *inside* the guest, so any **future** in-guest regression — a misconfigured
capability opt-in, an unknown containerd/kernel escape, a new code path that reintroduces either bug
— still has **nothing backstopping it** at the host/hypervisor layer. The VM boundary protects the
*host*; it provides **no defense-in-depth for the egress allowlist itself**.

**Decision (already made):** for v1 this is documented as an accepted single-layer design rather than
implemented as a host-side control (host-side NAT filtering on macOS/vfkit is impractical for v1).
This task records the residual risk accurately so the in-guest hardening controls are understood as
load-bearing and irreplaceable — not merely one layer among several. A future host backstop can be a
separate task.

## Goal

Update `KRAYT_SPEC.md` so the egress model's single-layer nature and its dependency on the container-
hardening controls are explicit.

## Implement (docs only)

- `KRAYT_SPEC.md` §6.6: add a short "Enforcement layers" note — egress is enforced by (1) the L7
  allowlist proxy and (2) the L3 nftables `output` lock, **both inside the guest**; the host/hypervisor
  applies no egress filtering (the VM uses NAT). State that these two in-guest layers are the *only*
  layers.
- `KRAYT_SPEC.md` §10 "Residual considerations": add a bullet:
  > **Egress control is single-layer (in-guest).** The host applies no network filtering; the
  > allowlist is enforced entirely by the in-VM proxy + nftables. A container that can assume the
  > proxyd uid, or a guest-root escape that can flush nftables, defeats it with no backstop. The
  > container-hardening controls (dropped `CAP_SETUID`/`CAP_SETGID`, enforced non-root, isolated
  > patch generation) are therefore the primary mitigations — see §6.10 and §6.7.
- Cross-reference `fix-egress-allowlist-bypass.md` and `fix-guest-git-config-rce.md` conceptually
  (by spec section, not by file name) so the dependency is clear.
- `docs/ai-tasks/README.md`: add this task.

## Verify

Docs-only; confirm the spec renders and the cross-references point at real sections. No build/test
impact, but still run `go build ./...` to confirm nothing else was touched.

## Done when

- §6.6 and §10 state the single-layer egress model and its reliance on the container-hardening
  controls.

## Constraints

- No code changes. If, while writing, you conclude a cheap host-side control *is* feasible, do **not**
  implement it here — note it as a proposed follow-up task instead.
