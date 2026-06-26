# Building the VM image locally on a Mac (aarch64-linux builder)

The base VM image (`images/flake.nix`) is an **aarch64-linux** artifact, so building it —
or even just computing the guest-agent `vendorHash` — needs an `aarch64-linux` builder.
Apple Silicon Macs can't do this natively; you either let CI build it (the canonical path,
§11.3/§11.5) or set up a local Linux builder for fast iteration. This doc covers the local
option.

You only need this if you want to build the image without round-tripping through CI. If
you'd rather use CI, skip to [Using CI instead](#using-ci-instead).

There are two ways to get a local `aarch64-linux` builder. Pick one.

> **Which Nix do I have?** Run `nix --version`:
> - `nix (Nix) 2.x.x` → **upstream Nix** (e.g. installed from
>   [nix.dev](https://nix.dev/install-nix) / `nixos.org/nix/install`). Use **Option B** —
>   Option A's native builder is a Determinate-only feature.
> - `nix (Determinate Nix 3.x.x) …` → **Determinate Nix** (installed from
>   `install.determinate.systems`). Either option works; **Option A** is the lightest.
>
> "Installer" and "distribution" are separate choices: the Determinate *installer* installs
> the Determinate Nix *distribution* (with `determinate-nixd`) by default, which is what
> Option A needs. Switching distributions means uninstalling upstream Nix first, then
> reinstalling — not running the other script on top. You do **not** need to switch just to
> build this image; Option B works on upstream Nix.

---

## Option A — Determinate Nix native builder (recommended if you're on Determinate Nix)

Determinate Nix **3.8.4+** ships a built-in Linux builder that uses macOS's
Virtualization.framework directly (the same framework vfkit uses) — no QEMU, no nix-darwin.
It's a developer-preview feature, so confirm your version first.

1. **Upgrade Determinate Nix** to ≥ 3.8.4:

   ```sh
   sudo determinate-nixd upgrade
   nix --version
   ```

2. **Enable the external builder.** Add to `/etc/nix/nix.conf`:

   ```
   extra-experimental-features = external-builders
   external-builders = [{"systems":["aarch64-linux","x86_64-linux"],"program":"/usr/local/bin/determinate-nixd","args":["builder"]}]
   ```

   Then restart the daemon (`sudo launchctl kickstart -k system/systems.determinate.nix-daemon`,
   or just reboot).

3. **Verify** it's active:

   ```sh
   determinate-nixd version          # should report: native-linux-builder is enabled
   nix build nixpkgs#legacyPackages.aarch64-linux.cowsay
   ```

   If the feature isn't available yet on your account, the changelog says to email
   support@determinate.systems with your FlakeHub username for early access — in that case
   use Option B.

---

## Option B — nix-darwin `linux-builder` (stable, works on any Nix)

This is the broadly-documented path. It runs a small NixOS VM (QEMU) downloaded from the
official cache and registers it as a remote builder. It requires adopting
[nix-darwin](https://github.com/nix-darwin/nix-darwin).

In your nix-darwin configuration add:

```nix
{
  nix.linux-builder.enable = true;
  # the builder daemon must be able to talk to your nix daemon
  nix.settings.trusted-users = [ "@admin" ];
}
```

Then apply and verify:

```sh
darwin-rebuild switch
nix build nixpkgs#legacyPackages.aarch64-linux.cowsay
```

Behind the scenes this creates the `org.nixos.linux-builder` daemon (SSH keys + disk image
under `/var/lib/darwin-builder`), an SSH host-alias `linux-builder` in
`/etc/ssh/ssh_config.d/100-linux-builder.conf`, and a remote-builder entry in
`/etc/nix/machines`.

> Don't have nix-darwin and don't want to adopt it? You can run the builder VM ad hoc with
> `nix run nixpkgs#darwin.linux-builder`, but you then have to wire up `/etc/nix/machines`
> yourself — Option A or full nix-darwin is less fiddly.

---

## Build the krayt image with your local builder

Once either builder is active, build from the repo root. **Address the package by its full
`aarch64-linux` attribute path** — the short form (`#guest-agent`) resolves to your Mac's
`aarch64-darwin` and fails with "does not provide attribute …":

```sh
# 1. Compute the guest-agent vendorHash. With vendorHash = lib.fakeHash this FAILS with a
#    hash mismatch — that's expected; copy the `got:` value.
nix build ./images#packages.aarch64-linux.guest-agent
#   error: hash mismatch in fixed-output derivation …
#       specified: sha256-AAAA…
#       got:       sha256-REAL…      <-- paste this into images/flake.nix (vendorHash)

# 2. With the real vendorHash in place, build the full image:
nix build ./images#packages.aarch64-linux.vmImage
ls ./result        # vmlinuz  initrd  rootfs.img  cmdline
```

Then you can run the Phase 1 boot test against the locally-built artifacts (no registry
needed):

```sh
KRAYT_KERNEL=./result/vmlinuz \
KRAYT_INITRD=./result/initrd \
KRAYT_ROOTFS=./result/rootfs.img \
KRAYT_CMDLINE="$(cat ./result/cmdline)" \
  go test -tags 'integration darwin' -run TestBootHello -v ./internal/provider/vfkit/
```

> Note: `result/rootfs.img` is in the read-only Nix store. The vfkit provider CoW-clones
> the rootfs into its own run dir before booting (§6.3), so it doesn't write to the store
> copy — but make sure the file is the **raw** image vfkit expects (it is; the flake builds
> ext4 raw, §12).

---

## Using CI instead

If you skip the local builder, the same two values come from the `vm-image` workflow
(`.github/workflows/image.yml`) instead:

1. Run it once → the **Build VM image** step fails on the `fakeHash` mismatch and logs
   `got: sha256-…` → paste into `images/flake.nix`, commit, push.
2. Run it again → it builds and pushes to GHCR, and the **Push OCI artifact** step logs a
   `::notice` with the published `digest` → paste into `internal/vmimage/pinned.go`.

See `HUMAN_TODO.md` for the full handoff checklist.

---

## Sources
- [Determinate Nix 3.8.4 — a native Linux builder for macOS](https://determinate.systems/blog/changelog-determinate-nix-384/)
- [nix-darwin `linux-builder` module](https://github.com/nix-darwin/nix-darwin/blob/master/modules/nix/linux-builder.nix)
- [Nixcademy — Build and Deploy Linux Systems from macOS](https://nixcademy.com/posts/macos-linux-builder/)
- [NixOS Wiki — NixOS virtual machines on macOS](https://wiki.nixos.org/wiki/NixOS_virtual_machines_on_macOS)
