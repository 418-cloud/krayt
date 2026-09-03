{
  description = "krayt — dev shell (oras pinned for the image pipeline)";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs = { self, nixpkgs, flake-utils }:
    flake-utils.lib.eachDefaultSystem (system:
      let
        pkgs = import nixpkgs { inherit system; };
      in
      {
        devShells.default = pkgs.mkShell {
          # oras: OCI push/pull for the VM image pipeline (internal/vmimage, images/) — kept
          # until retire-vm-image-pipeline.md removes it. The protoc/protoc-gen-go/protoc-gen-go-grpc/buf
          # pins that used to live here went with internal/protocol at the
          # run-tasks-on-microsandbox.md cut-over.
          packages = [
            pkgs.oras
            pkgs.go
            pkgs.golangci-lint
          ];
        };
      });
}
