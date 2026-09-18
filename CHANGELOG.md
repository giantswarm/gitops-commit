# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).



## [Unreleased]

### Added

- `provenance`: the owning GitHub repository, branch and directory of a target from its Flux Kustomization and GitRepository, or explicit.
- `sopsenc`: SOPS encryption of `*secret*`/`*credential*` files for the age recipients of the repository's `.sops.yaml` creation rule, with generated values that exist only in the encrypted output; no decryption code path, asserted by a test.


[Unreleased]: https://github.com/giantswarm/gitops-commit/tree/main
