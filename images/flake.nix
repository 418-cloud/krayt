{
  # krayt base micro-VM image (§11). A minimal NixOS+systemd closure whose only job is
  # to run the guest-agent + containerd. Built reproducibly on a native Linux runner in
  # CI (§11.3/§11.5) — it CANNOT be built on macOS, which is why the arm64 boot-test is a
  # human/CI checkpoint (§11.6, HUMAN_TODO.md). The x86_64 variant CAN be built and booted
  # on any Linux host with /dev/kvm, which is how Phase 7 is verified.
  #
  # Outputs (packages.<system>, for aarch64-linux and x86_64-linux):
  #   vmImage     -> { vmlinuz, initrd, rootfs.img (raw), cmdline }; packaged as an OCI
  #                  artifact by CI (§11.5).
  #   guest-agent -> the static guest-agent binary (also baked into rootfs).
  #
  # The two arches pair with the two providers (§6.3):
  #   aarch64-linux -> vfkit on Apple Silicon. Boots the kernel as a PE `Image`, virtio-PCI,
  #                    console on hvc0, NAT + DHCP supplied by vfkit's built-in NAT device.
  #   x86_64-linux  -> firecracker on Linux/KVM. Boots an *uncompressed ELF* vmlinux (it
  #                    cannot boot a bzImage), virtio-MMIO, console on ttyS0, and no DHCP
  #                    server at all — the provider hands the guest its address on the
  #                    kernel cmdline instead (see the krayt.net=static unit below).
  description = "krayt base micro-VM image (kernel + initrd + raw rootfs)";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs = { self, nixpkgs }:
    let
      systems = [ "aarch64-linux" "x86_64-linux" ];
      forAllSystems = f: nixpkgs.lib.genAttrs systems f;

      imageFor = system:
        let
          pkgs = nixpkgs.legacyPackages.${system};
          isX86 = system == "x86_64-linux";

          # The guest-agent, pinned end-to-end via buildGoModule (§11.1). vendorHash is the
          # hash of the Go module dependencies (NOT the nixpkgs narHash). To regenerate after
          # changing dependencies, set it to lib.fakeHash, build, and paste the `got: sha256-…`
          # value the mismatch reports.
          #
          # NOTE (Phase 7): the firecracker provider drives Firecracker's REST API with a
          # hand-rolled client over its API unix socket (the same idiom the vfkit provider uses
          # for vfkit's REST API), so it adds NO new Go module dependencies — vendorHash is
          # unchanged from Phase 6. buildGoModule vendors the whole module's go.sum, so any
          # future dependency added anywhere in the repo does require regenerating it.
          guest-agent = pkgs.buildGoModule {
            pname = "krayt-agent";
            version = "0.0.0-dev";
            src = ../.; # repo root (go.mod, internal/, cmd/)
            subPackages = [ "cmd/krayt-agent" "cmd/krayt-proxy" "cmd/krayt-ask" ];
            vendorHash = "sha256-uQ56QZdktOJ2WIvbj7ndbEXaXgRzKTpOCKoSC0NNz2k=";
            env.CGO_ENABLED = "0";
          };

          nixos = nixpkgs.lib.nixosSystem {
            inherit system;
            modules = [
              ({ config, pkgs, lib, ... }: {
                # ---- minimal boot: no bootloader; the provider supplies kernel+initrd+cmdline ----
                boot.loader.grub.enable = false;
                boot.loader.systemd-boot.enable = false;
                # Use the classic scripted stage-1 (not systemd-in-initrd): it simply mounts
                # root and switch_roots into $closure/init, which is the proven pairing with
                # make-ext4-fs rootfs images (sd-image style) and avoids the systemd-initrd
                # find-nixos-closure machinery that our hand-built rootfs trips over.
                boot.initrd.systemd.enable = false;
                # virtio + vsock + overlay must be available early to mount root and serve
                # the control channel (§11.6). virtio_mmio is what Firecracker exposes its
                # devices on (it has no PCI bus); virtio_pci is vfkit's transport. Both are
                # listed so one image boots under either provider — the unused one simply
                # never matches a device.
                boot.initrd.availableKernelModules = [
                  "virtio_pci" "virtio_mmio" "virtio_blk" "virtio_net" "virtio_console"
                  "vsock" "vmw_vsock_virtio_transport" "overlay"
                ];
                boot.kernelModules = [ "nft_chain_nat" "nf_tables" ];

                fileSystems."/" = { device = "/dev/vda"; fsType = "ext4"; };

                # ---- networking (§6.6/§11.6) ----
                # The two backends deliver an address to the guest in genuinely different ways,
                # and the guest has to cope with both from one image.
                #
                # vfkit's virtio-net NAT device runs a DHCP server, so the guest just asks for a
                # lease — that is the 90-dhcp unit below, unchanged in behaviour since Phase 1.
                #
                # Firecracker has no NAT device and no DHCP server: it hands the VM a bare tap
                # and nothing else. So the provider puts the address on the kernel command line
                # instead, in dracut's `ip=`/`ifname=` form (see tap.go).
                #
                # The kernel's own `ip=` autoconfiguration is NOT what reads that: it needs
                # CONFIG_IP_PNP, which the nixpkgs kernel does not set, so the kernel silently
                # ignores the parameter. systemd-network-generator is the userspace equivalent —
                # it parses the same cmdline syntax and writes .network/.link files into
                # /run/systemd/network before udev and networkd start. It ships with systemd but
                # is not enabled by default, so enable it here.
                #
                # Precedence is by filename across all search dirs: the generator writes
                # 70-<iface>.network, so the DHCP fallback is named 90- to sort *after* it. With
                # a cmdline address (firecracker) the generated 70- unit wins; without one
                # (vfkit) nothing is generated and 90-dhcp applies. Neither provider needs to
                # know the other exists.
                networking.useNetworkd = true;
                networking.useDHCP = false;
                systemd.network.enable = true;

                systemd.additionalUpstreamSystemUnits = [ "systemd-network-generator.service" ];
                systemd.units."systemd-network-generator.service".wantedBy = [ "sysinit.target" ];

                systemd.network.networks."90-dhcp" = {
                  matchConfig.Name = "en* eth*";
                  networkConfig.DHCP = "yes";
                };

                # Don't let a slow or absent link stall the boot: krayt-agent only *wants*
                # network-online.target, so a wait-online timeout delays but never blocks it.
                systemd.network.wait-online.timeout = 15;
                systemd.network.wait-online.anyInterface = true;

                networking.nftables.enable = true; # per-task rules applied by the agent at run start

                # ---- egress proxy identity (§6.6) ----
                # The proxy runs as this dedicated, non-root uid; the nftables lock permits
                # egress only for `skuid "proxyd"`, so the container (a different uid) cannot
                # bypass it. The name must exist for both the credential switch and the rule.
                users.users.proxyd = {
                  isSystemUser = true;
                  group = "proxyd";
                  description = "krayt egress allowlist proxy";
                };
                users.groups.proxyd = { };

                # ---- per-run scratch disk (§6.10) ----
                # The provider attaches a sparse raw disk sized to DiskGiB as /dev/vdb. The
                # closure-sized rootfs has no room for an imported image, so format + mount the
                # scratch disk at /var/lib/containerd (containerd's content store + snapshots)
                # before containerd starts. The disk is fresh every run (a new sparse file), so
                # we format unconditionally — no detection needed. The guest-agent's TMPDIR is
                # pointed at a subdir below, so the streamed image tar + repo clone also land on
                # the scratch disk instead of /tmp (tmpfs/RAM).
                systemd.services.krayt-scratch = {
                  description = "format + mount the per-run scratch disk for containerd";
                  requiredBy = [ "containerd.service" ];
                  before = [ "containerd.service" ];
                  after = [ "dev-vdb.device" ];
                  requires = [ "dev-vdb.device" ];
                  path = [ pkgs.e2fsprogs pkgs.util-linux ];
                  serviceConfig = {
                    Type = "oneshot";
                    RemainAfterExit = true;
                  };
                  script = ''
                    mkfs.ext4 -q -F -L krayt-scratch /dev/vdb
                    mkdir -p /var/lib/containerd
                    mount /dev/vdb /var/lib/containerd
                    mkdir -p /var/lib/containerd/tmp
                  '';
                };

                # ---- container runtime (§6.10) ----
                virtualisation.containerd.enable = true;

                # ---- the guest-agent service (§6.4/§11.6) ----
                systemd.services.krayt-agent = {
                  description = "krayt guest agent (vsock control server)";
                  wantedBy = [ "multi-user.target" ];
                  after = [ "containerd.service" "network-online.target" ];
                  wants = [ "network-online.target" ];
                  requires = [ "containerd.service" ];
                  # On PATH for the agent: git for the §6.7 bundle ingest/diff; nftables (`nft`)
                  # for the §6.6 egress lock; and the guest-agent package itself so the agent can
                  # exec `krayt-proxy` (built into the same derivation).
                  path = [ pkgs.gitMinimal pkgs.nftables guest-agent ];
                  # Route the agent's working files (image tar, repo bundle, /workspace clone)
                  # onto the scratch disk rather than tmpfs/RAM (§6.10).
                  environment.TMPDIR = "/var/lib/containerd/tmp";
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

          # Raw ext4 rootfs containing the system closure. Both providers boot raw images only
          # (vfkit takes raw/ISO, firecracker takes a raw block device — neither takes qcow2),
          # and the per-run CoW clone is made from this raw image (§6.3).
          # callPackage auto-supplies pkgs/lib/e2fsprogs/… that make-ext4-fs.nix expects.
          #
          # make-ext4-fs copies only the /nix/store closure, so populateImageCommands must
          # create the root skeleton: the mountpoints the initrd needs for the initrd→rootfs
          # handoff (notably /run, mounted before the root is remounted rw) and the system
          # profile symlink so the closure resolves.
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

          # The kernel image the provider boots, as the artifact's `vmlinuz`.
          #
          # aarch64 (vfkit): the PE `Image` that kernelFile names — vfkit's Linux bootloader
          # takes it directly.
          #
          # x86_64 (firecracker): Firecracker's x86_64 loader accepts an *uncompressed ELF*
          # only — it cannot boot the `bzImage` that kernelFile names (upstream's own CI
          # kernels are `vmlinux-*` ELF binaries for exactly this reason). nixpkgs ships that
          # ELF as `vmlinux` in the kernel's `dev` output. It carries full debug_info there
          # (~379 MiB), so strip it — that costs nothing at boot and takes the artifact to
          # ~55 MiB. The kernel has CONFIG_PVH=y, so Firecracker finds the PVH entry note.
          kernelImage =
            if isX86 then
              pkgs.runCommand "krayt-vmlinux" { nativeBuildInputs = [ pkgs.binutils ]; } ''
                strip -o $out ${nixos.config.system.build.kernel.dev}/vmlinux
              ''
            else
              "${nixos.config.system.build.kernel}/${nixos.config.system.boot.loader.kernelFile}";

          # The default kernel command line for this arch's provider. The host may override it
          # (VMSpec.Cmdline), and the firecracker provider always does — it has to append the
          # per-VM `ip=` autoconf for the tap device (§6.6). `krayt.net=static` is what selects
          # the 05-krayt-static networkd unit above.
          cmdline =
            if isX86 then
              "init=${nixos.config.system.build.toplevel}/init console=ttyS0 reboot=k panic=1 root=/dev/vda"
            else
              "init=${nixos.config.system.build.toplevel}/init console=hvc0 root=/dev/vda";
        in
        {
          inherit guest-agent;
          default = self.packages.${system}.vmImage;

          vmImage = pkgs.runCommand "krayt-vmimage" { } ''
            mkdir -p $out
            cp ${kernelImage} $out/vmlinuz
            cp ${nixos.config.system.build.initialRamdisk}/initrd $out/initrd
            cp ${rootfs} $out/rootfs.img
            printf '%s' "${cmdline}" > $out/cmdline
          '';
        };
    in
    {
      packages = forAllSystems imageFor;
    };
}
