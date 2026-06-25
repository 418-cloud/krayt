{
  description = "krayt — dev shell (protoc/buf/oras pinned) + protocol codegen (§9.2)";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs = { self, nixpkgs, flake-utils }:
    flake-utils.lib.eachDefaultSystem (system:
      let
        pkgs = import nixpkgs { inherit system; };

        # Toolchain pinned via flake.lock. Tiers 2–3 of the prereq tiers (§9.2):
        # regenerating the protocol and packaging the VM image.
        codegenTools = [
          pkgs.protobuf
          pkgs.protoc-gen-go
          pkgs.protoc-gen-go-grpc
          pkgs.buf
          pkgs.oras
        ];

        # `make proto` shells out to this so plugin/version skew never produces
        # noisy diffs. Generated Go is committed to internal/protocol/pb.
        proto = pkgs.writeShellApplication {
          name = "krayt-proto";
          runtimeInputs = codegenTools;
          text = ''
            set -euo pipefail
            root="$(git rev-parse --show-toplevel)"
            cd "$root"
            mkdir -p internal/protocol/pb
            protoc \
              --proto_path=internal/protocol \
              --go_out=. --go_opt=module=github.com/418-cloud/krayt \
              --go-grpc_out=. --go-grpc_opt=module=github.com/418-cloud/krayt \
              internal/protocol/krayt.proto
            echo "generated internal/protocol/pb"
          '';
        };
      in
      {
        packages.proto = proto;

        devShells.default = pkgs.mkShell {
          packages = codegenTools ++ [
            pkgs.go
            pkgs.golangci-lint
          ];
        };
      });
}
