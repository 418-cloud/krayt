{
  # krayt base micro-VM image (§11). A minimal NixOS+systemd closure whose only job is
  # to run the guest-agent + containerd. Built reproducibly on an arm64 Linux runner in
  # CI (§11.3/§11.5) — it CANNOT be built on macOS or in a cloud coding agent, which is
  # why the boot-test is a human/CI checkpoint (§11.6, HUMAN_TODO.md).
  #
  # Outputs (packages.aarch64-linux):
  #   vmImage     -> { vmlinuz, initrd, rootfs.img (raw), cmdline } for the vfkit Linux
  #                  bootloader; packaged as an OCI artifact by CI (§11.5).
  #   guest-agent -> the static linux/arm64 guest-agent binary (also baked into rootfs).
  description = "krayt base micro-VM image (kernel + initrd + raw rootfs)";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs = { self, nixpkgs }:
    let
      system = "aarch64-linux";
      pkgs = nixpkgs.legacyPackages.${system};

      # The guest-agent, pinned end-to-end via buildGoModule (§11.1). vendorHash is the
      # hash of the Go module dependencies (NOT the nixpkgs narHash). To regenerate after
      # changing dependencies, set it to lib.fakeHash, build, and paste the `got: sha256-…`
      # value the mismatch reports. Build runs on aarch64-linux (CI, or a Mac
      # linux-builder; §11.3).
      #
      # Phase 2 changed the Go dependency set (the guest-agent now drives containerd via
      # github.com/containerd/containerd/v2/client, §6.10), so vendorHash MUST be
      # regenerated — it is set to fakeHash to force the mismatch that prints the new hash.
      # See HUMAN_TODO.md "[Phase 2] Regenerate guest-agent vendorHash".
      guest-agent = pkgs.buildGoModule {
        pname = "krayt-agent";
        version = "0.0.0-dev";
        src = ../.; # repo root (go.mod, internal/, cmd/)
        subPackages = [ "cmd/krayt-agent" ];
        vendorHash = "sha256-6l937L2Q8MCCJqApw7EW/ZI/Q9DjKXy57GFugFkn5nM=";
        env.CGO_ENABLED = "0";
      };

      nixos = nixpkgs.lib.nixosSystem {
        inherit system;
        modules = [
          ({ config, pkgs, lib, ... }: {
            # ---- minimal boot: no bootloader; vfkit supplies kernel+initrd+cmdline ----
            boot.loader.grub.enable = false;
            boot.loader.systemd-boot.enable = false;
            # Use the classic scripted stage-1 (not systemd-in-initrd): it simply mounts
            # root and switch_roots into $closure/init, which is the proven pairing with
            # make-ext4-fs rootfs images (sd-image style) and avoids the systemd-initrd
            # find-nixos-closure machinery that our hand-built rootfs trips over.
            boot.initrd.systemd.enable = false;
            # virtio + vsock + overlay must be available early to mount root and serve
            # the control channel (§11.6).
            boot.initrd.availableKernelModules = [
              "virtio_pci" "virtio_blk" "virtio_net" "virtio_console"
              "vsock" "vmw_vsock_virtio_transport" "overlay"
            ];
            boot.kernelModules = [ "nft_chain_nat" "nf_tables" ];

            fileSystems."/" = { device = "/dev/vda"; fsType = "ext4"; };

            # ---- networking: one NAT NIC from vfkit, via systemd-networkd (§6.6/§11.6) --
            networking.useNetworkd = true;
            networking.useDHCP = false;
            systemd.network.enable = true;
            systemd.network.networks."10-nat" = {
              matchConfig.Name = "en* eth*";
              networkConfig.DHCP = "yes";
            };
            networking.nftables.enable = true; # per-task rules applied by the agent at run start

            # ---- container runtime (§6.10) ----
            virtualisation.containerd.enable = true;

            # ---- the guest-agent service (§6.4/§11.6) ----
            systemd.services.krayt-agent = {
              description = "krayt guest agent (vsock control server)";
              wantedBy = [ "multi-user.target" ];
              after = [ "containerd.service" "network-online.target" ];
              wants = [ "network-online.target" ];
              requires = [ "containerd.service" ];
              # git is required in the guest for the bundle ingest + patch generation of
              # §6.7 (bundle verify, clone, diff); gitMinimal keeps the closure small. This
              # is the one closure addition §11.6's list omits — flagged for the spec.
              path = [ pkgs.gitMinimal ];
              serviceConfig = {
                Type = "notify";
                ExecStart = "${guest-agent}/bin/krayt-agent";
                Restart = "no";
              };
            };

            # ---- trim the closure: no editors/shells/docs/package manager (§11.6) ----
            documentation.enable = false;
            documentation.nixos.enable = false;
            services.getty.autologinUser = lib.mkForce null;
            security.sudo.enable = false;
            system.stateVersion = "24.05";
          })
        ];
      };

      # Raw ext4 rootfs containing the system closure. vfkit boots raw images only
      # (no qcow2, §12); the CoW clone is an APFS clonefile of this raw image (§6.3).
      # callPackage auto-supplies pkgs/lib/e2fsprogs/… that make-ext4-fs.nix expects.
      #
      # make-ext4-fs copies only the /nix/store closure, so populateImageCommands must
      # create the root skeleton: the mountpoints systemd-initrd needs for the
      # initrd→rootfs handoff (notably /run, mounted before the root is remounted rw) and
      # the system profile symlink so initrd-find-nixos-closure can resolve the closure.
      rootfs = pkgs.callPackage "${nixpkgs}/nixos/lib/make-ext4-fs.nix" {
        storePaths = [ nixos.config.system.build.toplevel ];
        volumeLabel = "krayt-root";
        populateImageCommands = ''
          mkdir -p ./files/proc ./files/sys ./files/dev ./files/run ./files/tmp \
                   ./files/var ./files/etc ./files/root ./files/mnt ./files/sbin \
                   ./files/nix/var/nix/profiles ./files/nix/var/nix/gcroots
          ln -s ${nixos.config.system.build.toplevel} ./files/nix/var/nix/profiles/system
          # /init is the scripted initrd's default stage-2 target (no init= needed on the
          # cmdline), so the image boots self-contained with a generic `root=/dev/vda`.
          ln -s ${nixos.config.system.build.toplevel}/init ./files/init
          ln -s ${nixos.config.system.build.toplevel}/init ./files/sbin/init
        '';
      };
    in
    {
      packages.${system} = {
        inherit guest-agent;
        default = self.packages.${system}.vmImage;

        vmImage = pkgs.runCommand "krayt-vmimage" { } ''
          mkdir -p $out
          # kernelFile is "Image" on aarch64 (vs "bzImage" on x86_64).
          cp ${nixos.config.system.build.kernel}/${nixos.config.system.boot.loader.kernelFile} $out/vmlinuz
          cp ${nixos.config.system.build.initialRamdisk}/initrd $out/initrd
          cp ${rootfs} $out/rootfs.img
          printf 'init=%s/init console=hvc0 root=/dev/vda' \
            "${nixos.config.system.build.toplevel}" > $out/cmdline
        '';
      };
    };
}
