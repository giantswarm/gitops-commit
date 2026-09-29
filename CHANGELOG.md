# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).



## [Unreleased]

### Fixed

- Dependencies with known vulnerabilities bumped (nancy): golang.org/x/crypto v0.57.0, google.golang.org/grpc v1.84.0, go.opentelemetry.io/otel and otel/sdk v1.46.0, go.mongodb.org/mongo-driver v1.17.10.

- `provenance`: a GitRepository on GitHub's SSH endpoint on port 443 (`ssh://git@ssh.github.com:443/owner/name.git`, for networks that block port 22) names its GitHub repository; `ParseRepositoryURL` refused the host `ssh.github.com` as not GitHub, so a target reconciled from such a source had no commit target.

### Changed

- `sopsenc`: which files are secret files follows the repository's `.sops.yaml` — a file a creation rule's `path_regex` matches is encrypted whatever its name (`secrets/api-key.yaml` by its directory); a `*secret*`/`*credential*`-named file no rule covers is refused, never written in plaintext; kustomize's entry-point files stay plaintext. `Config.IsSecretFile` and `Encryptor.IsSecretFile` expose the decision.

### Added

- `layout`: a new workload cluster's place in the repository that owns its Organization, the shape a gitops-template repository keeps every cluster in — `NewCluster` follows the Organization's Flux Kustomization (refusing one without it, `ErrNotReconciled`, and one not built from a GitHub branch); `BuildCluster` writes the objects under `organizations/<org>/workload-clusters/<cluster>/` with their own `kustomization.yaml`, the per-cluster Kustomization `<installation>-clusters-<cluster>` (the Organization's source, service account, interval, timeout and keys, `prune: true`, no `postBuild`) and its entry in `workload-clusters/kustomization.yaml`, created when absent, and refuses a root without `organizations/<org>/` (`ErrNoOrganization`, naming the path); `RemoveCluster` removes the directory, the Kustomization file and the entry, and reports whether the Organization's Kustomization prunes (`ClusterPlan.Prune`).
- `commit`: `ListFiles` (the `Lister` seam of `GitHub` and `Fake`) lists every file under a directory at a branch's head.
- `provenance`: a Kustomization carries its service account, interval, timeout, prune and keys (`spec.decryption`), read by `Flux.Add`; `Flux.FindKustomization` looks one up.

- `layout`: a writer's directory in a GitOps repository and what one write changes there — one file per object (`Directory.ObjectFile`, a Secret in a secret file), the directory's own `kustomization.yaml` listing them and the parent's entry for the directory, both edited with comments kept and removed with the last file; `Build` decides every file against the base (add, update, remove, unchanged), encrypts new secret files for the repository's `.sops.yaml` and never re-encrypts one that exists, refuses secret files the repository cannot take encrypted (`SecretError`), and hands `commit.Open` its `Change`. The layout cluster-manager's commit mode established, for every manager.

- `commit`: a path whose content is nil is removed in the commit; `ReadFile` (the `Reader` seam of `GitHub` and `Fake`) reads a file at a branch's head and answers `ErrFileNotFound` for a missing one.
- `sopsenc`: a `Generated` declaration names the `Encoding` its placeholder receives — the value as it is, or its standard base64 (`EncodingBase64`) where the consumer decodes the leaf — so one generated value lands raw in the file that reads it and encoded in the file whose consumer decodes it.
- `provenance`: the owning GitHub repository, branch and directory of a target from its Flux Kustomization and GitRepository, or explicit.
- `sopsenc`: SOPS encryption of `*secret*`/`*credential*` files for the age recipients of the repository's `.sops.yaml` creation rule, with generated values that exist only in the encrypted output; no decryption code path, asserted by a test.
- `commit`: a branch, one commit per repository and the pull request with the caller's title and body, through a `Remote` built from the caller's token; the merge as the person, refused before the caller's approval, before green checks and when the head moved; a refused token as `ErrAuth` with the GitHub status; a `Fake` remote for tests.
- `commit`: the pull-request seams on `Remote` and `Fake` — `FindPullRequest`, `OpenDraftPullRequest`, `EnableAutoMerge` (the caller's choice, with the repository's merge method), `Close` and `Revert` — and `NewGitHubWithClient` for a remote on an HTTP client that already carries the caller's identity.
- `commit`: `Approve` on `Remote` and `Fake` — an approving review on a pull request as the person, pinned to the head the caller saw; the `Fake` records the approvals and reports the rollup.
- `sopsenc`: the generated kind `keypair-es256` — an ECDSA P-256 key pair drawn once per name, its halves PEM (PKCS #8 private, SubjectPublicKeyInfo public) written as one-line YAML scalars; a declaration names the half its placeholder receives, the private half in secret files only, the public half in plain files too.


[Unreleased]: https://github.com/giantswarm/gitops-commit/tree/main
