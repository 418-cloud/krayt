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
      lib = nixpkgs.lib;

      # The guest-agent, pinned end-to-end via buildGoModule (§11.1). vendorHash is the
      # hash of the Go module dependencies (NOT the nixpkgs narHash). Leave it as
      # lib.fakeHash; the first build fails with `got: sha256-…` — paste that value here.
      # The build must run on aarch64-linux (CI, or a Mac linux-builder; §11.3). See
      # HUMAN_TODO.md "[Phase 1] Fill guest-agent vendorHash".
      guest-agent = pkgs.buildGoModule {
        pname = "krayt-agent";
        version = "0.0.0-dev";
        src = ../.; # repo root (go.mod, internal/, cmd/)
        subPackages = [ "cmd/krayt-agent" ];
        vendorHash = lib.fakeHash;
        env.CGO_ENABLED = "0";
      };

      nixos = nixpkgs.lib.nixosSystem {
        inherit system;
        modules = [
          ({ config, pkgs, lib, ... }: {
            # ---- minimal boot: no bootloader; vfkit supplies kernel+initrd+cmdline ----
            boot.loader.grub.enable = false;
            boot.loader.systemd-boot.enable = false;
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
      rootfs = import "${nixpkgs}/nixos/lib/make-ext4-fs.nix" {
        inherit pkgs;
        storePaths = [ nixos.config.system.build.toplevel ];
        volumeLabel = "krayt-root";
      };
    in
    {
      packages.${system} = {
        inherit guest-agent;
        default = self.packages.${system}.vmImage;

        vmImage = pkgs.runCommand "krayt-vmimage" { } ''
          mkdir -p $out
          # arm64 kernel image is named Image (not bzImage).
          cp ${nixos.config.system.build.kernel}/Image $out/vmlinuz
          cp ${nixos.config.system.build.initialRamdisk}/initrd $out/initrd
          cp ${rootfs} $out/rootfs.img
          printf 'init=%s/init console=hvc0 root=/dev/vda' \
            "${nixos.config.system.build.toplevel}" > $out/cmdline
        '';
      };
    };
}
