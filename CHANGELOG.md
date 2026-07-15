# Changelog

## [0.5.1](https://github.com/418-cloud/krayt/compare/v0.5.0...v0.5.1) (2026-07-15)


### Bug Fixes

* build linux/amd64 binary on release ([#65](https://github.com/418-cloud/krayt/issues/65)) ([f730dad](https://github.com/418-cloud/krayt/commit/f730dad775781b56609975fc1dee74c01d7a95a8))

## [0.5.0](https://github.com/418-cloud/krayt/compare/v0.4.0...v0.5.0) (2026-07-15)


### Features

* phase 7 firecracker linux backend ([#56](https://github.com/418-cloud/krayt/issues/56)) ([b133125](https://github.com/418-cloud/krayt/commit/b133125f0d739a33ca0fb5884012e77b660a05a4))


### Dependencies

* update module github.com/containerd/containerd/v2 to v2.3.3 ([#57](https://github.com/418-cloud/krayt/issues/57)) ([bc0eb21](https://github.com/418-cloud/krayt/commit/bc0eb21af4b0adbd2e8ba782707ffe212f0fd9d3))
* update vmimage to 0.3.0 and krayt-dev to latest version ([#62](https://github.com/418-cloud/krayt/issues/62)) ([1e93faa](https://github.com/418-cloud/krayt/commit/1e93faaff2a76c950dbcaeab249045b0c7ad04f9))

## [0.4.0](https://github.com/418-cloud/krayt/compare/v0.3.1...v0.4.0) (2026-07-12)


### Features

* image list and prune operations ([#51](https://github.com/418-cloud/krayt/issues/51)) ([018e645](https://github.com/418-cloud/krayt/commit/018e64573fbf16d1e926e3b0210a2a7c77bc12e0))
* perform a preflight resource check before kraty run start ([#54](https://github.com/418-cloud/krayt/issues/54)) ([3d6b7bd](https://github.com/418-cloud/krayt/commit/3d6b7bd036609fb128ecfdd9227bf0a1bad783b6))
* shell completion for krayt commands ([#49](https://github.com/418-cloud/krayt/issues/49)) ([b4ed097](https://github.com/418-cloud/krayt/commit/b4ed097e07e42a3802836bd22d623f6d54f51aa9))


### Bug Fixes

* defer running state until repo pushed to agent ([#53](https://github.com/418-cloud/krayt/issues/53)) ([3b57f9e](https://github.com/418-cloud/krayt/commit/3b57f9eba6ca0b9c7bc18ceaed01662a2f30d922))

## [0.3.1](https://github.com/418-cloud/krayt/compare/v0.3.0...v0.3.1) (2026-07-10)


### Bug Fixes

* patch oras-go hardlink CVE-2026-50163, clean up rejected image p… ([#45](https://github.com/418-cloud/krayt/issues/45)) ([d315d9d](https://github.com/418-cloud/krayt/commit/d315d9db2fc8e93cd419d02da38e59173d848b91))
* report a wall-clock timeout during setup as timed_out, not a raw error ([#47](https://github.com/418-cloud/krayt/issues/47)) ([a33141b](https://github.com/418-cloud/krayt/commit/a33141bf2034ebab5dd44fa996db01a952c78817))

## [0.3.0](https://github.com/418-cloud/krayt/compare/v0.2.0...v0.3.0) (2026-07-10)


### Features

* harden container OCI spec — drop capabilities, enforce non-root, seccomp ([#34](https://github.com/418-cloud/krayt/issues/34)) ([52b08bb](https://github.com/418-cloud/krayt/commit/52b08bb8861786062392d53c0563b916475a0b7a))


### Bug Fixes

* add proxy ssrf guard ([#40](https://github.com/418-cloud/krayt/issues/40)) ([308775b](https://github.com/418-cloud/krayt/commit/308775baaf1620dd9f1f163855e1b6209661f072))
* harden vfkit socket root ([#41](https://github.com/418-cloud/krayt/issues/41)) ([8eb31f3](https://github.com/418-cloud/krayt/commit/8eb31f353f797b5546a13d0d93ff05f9464ac1bb))
* isolate guest patch generation from container-writable .git config ([#36](https://github.com/418-cloud/krayt/issues/36)) ([d6dd145](https://github.com/418-cloud/krayt/commit/d6dd14585ad2009b84c2716dcc228aca5d974c85))
* redact secrets in report.md and ask_human text, scan patch for l… ([#38](https://github.com/418-cloud/krayt/issues/38)) ([8b00fb0](https://github.com/418-cloud/krayt/commit/8b00fb04b8182f49c1f8067d33a6e3bcbdbcc4ee))


### Dependencies

* update gomod non-major dependencies ([#33](https://github.com/418-cloud/krayt/issues/33)) ([e98144f](https://github.com/418-cloud/krayt/commit/e98144fab480a2d64d504e07990c406fedd4e3df))
* update pinned vmimage digest to latest version ([#43](https://github.com/418-cloud/krayt/issues/43)) ([a4cb69e](https://github.com/418-cloud/krayt/commit/a4cb69e412b38b1d43d86f8c053c3584d4f3216f))

## [0.2.0](https://github.com/418-cloud/krayt/compare/v0.1.2...v0.2.0) (2026-07-06)


### Features

* add prompt input from stdin to support headless and simplify sm… ([#29](https://github.com/418-cloud/krayt/issues/29)) ([379098c](https://github.com/418-cloud/krayt/commit/379098c5a0fb5066f45d9b7734a93c36643fa007))

## [0.1.2](https://github.com/418-cloud/krayt/compare/v0.1.1...v0.1.2) (2026-07-05)


### Bug Fixes

* upgrade vm oci image digest ([e4e2a7a](https://github.com/418-cloud/krayt/commit/e4e2a7a61f8f767dea9a02f13ddb7d8e02ad3da7))

## [0.1.1](https://github.com/418-cloud/krayt/compare/v0.1.0...v0.1.1) (2026-07-05)


### Bug Fixes

* make forward git bundle self-contained for repos with history ([#16](https://github.com/418-cloud/krayt/issues/16)) ([eefbf7f](https://github.com/418-cloud/krayt/commit/eefbf7f74ce445727330e6c2510b68eaa21ab84c))


### Dependencies

* update gomod non-major dependencies ([#18](https://github.com/418-cloud/krayt/issues/18)) ([fc40b93](https://github.com/418-cloud/krayt/commit/fc40b9377f3e41c970668f1ce46b1e1b9f99928d))

## 0.1.0 (2026-07-04)


### Features

* add `krayt questions` (alias q) with --pending-only/--sort + ls pending hint (§6.13) ([#7](https://github.com/418-cloud/krayt/issues/7)) ([3546c31](https://github.com/418-cloud/krayt/commit/3546c319a7808843077ad0d1efd9381de8fb5606))
* implement phase 4 ([#5](https://github.com/418-cloud/krayt/issues/5)) ([0651292](https://github.com/418-cloud/krayt/commit/065129247f1d756c39b3a7d2908a14746604a451))
* implement phase 5 ([#6](https://github.com/418-cloud/krayt/issues/6)) ([f92d462](https://github.com/418-cloud/krayt/commit/f92d462c490f42f9283f3572afd2771200715012))
* implementation of phase 1 ([#2](https://github.com/418-cloud/krayt/issues/2)) ([356170a](https://github.com/418-cloud/krayt/commit/356170a37ac16b4c3477e3d930c74d2d55bdd0ff))
* implementation of phase 3 ([#4](https://github.com/418-cloud/krayt/issues/4)) ([c80bb8e](https://github.com/418-cloud/krayt/commit/c80bb8eb27f0c1bfa9cf3df064f8300ebe7f5b1d))
* Implementing Phase 2 of krayt  ([#3](https://github.com/418-cloud/krayt/issues/3)) ([64d2d95](https://github.com/418-cloud/krayt/commit/64d2d959d4f138b99ab29e19e5da9ce54ead7858))


### Dependencies

* pin to v0.1.0 digest of vmimage ([ff35092](https://github.com/418-cloud/krayt/commit/ff3509257b6beca1c223eff2d87ca80204d6be7f))


### Miscellaneous Chores

* release 0.1.0 ([578d94e](https://github.com/418-cloud/krayt/commit/578d94e54784038796785cbc1ef5e93a7985bf4f))

## Changelog

All notable changes are recorded here. This file is maintained by
[release-please](https://github.com/googleapis/release-please) from Conventional Commit
messages; do not edit it by hand.
