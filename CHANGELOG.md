# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).



## [Unreleased]

### Changed

- `sopsenc`: which files are secret files follows the repository's `.sops.yaml` — a file a creation rule's `path_regex` matches is encrypted whatever its name (`secrets/api-key.yaml` by its directory); a `*secret*`/`*credential*`-named file no rule covers is refused, never written in plaintext; kustomize's entry-point files stay plaintext. `Config.IsSecretFile` and `Encryptor.IsSecretFile` expose the decision.

### Added

- `provenance`: the owning GitHub repository, branch and directory of a target from its Flux Kustomization and GitRepository, or explicit.
- `sopsenc`: SOPS encryption of `*secret*`/`*credential*` files for the age recipients of the repository's `.sops.yaml` creation rule, with generated values that exist only in the encrypted output; no decryption code path, asserted by a test.
- `commit`: a branch, one commit per repository and the pull request with the caller's title and body, through a `Remote` built from the caller's token; the merge as the person, refused before the caller's approval, before green checks and when the head moved; a refused token as `ErrAuth` with the GitHub status; a `Fake` remote for tests.
- `commit`: the pull-request seams on `Remote` and `Fake` — `FindPullRequest`, `OpenDraftPullRequest`, `EnableAutoMerge` (the caller's choice, with the repository's merge method), `Close` and `Revert` — and `NewGitHubWithClient` for a remote on an HTTP client that already carries the caller's identity.
- `commit`: `Approve` on `Remote` and `Fake` — an approving review on a pull request as the person, pinned to the head the caller saw; the `Fake` records the approvals and reports the rollup.
- `sopsenc`: the generated kind `keypair-es256` — an ECDSA P-256 key pair drawn once per name, its halves PEM (PKCS #8 private, SubjectPublicKeyInfo public) written as one-line YAML scalars; a declaration names the half its placeholder receives, the private half in secret files only, the public half in plain files too.


[Unreleased]: https://github.com/giantswarm/gitops-commit/tree/main
