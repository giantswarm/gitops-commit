# gitops-commit

Go module that lands rendered files in GitOps repositories as the person: Flux provenance, SOPS encryption for the repository's recipients, branch, commit, pull request and merge.

## Packages

- `provenance` — where a target's files live. `Flux.Resolve(namespace, name)` follows a Flux Kustomization to its GitRepository and returns the GitHub repository, branch and directory (`spec.path`); `Explicit(repository, branch, directory)` for a location the caller names. Input is data the caller has read (typed structs or decoded objects via `Flux.Add`); the package talks to no cluster.
- `sopsenc` — encryption of a repository's secret files. Files named `*secret*`/`*credential*` are SOPS-encrypted for the age recipients of the first matching creation rule of the repository's `.sops.yaml`, public keys only. Values declared as `Generated` are drawn from `crypto/rand` at commit time — once per name, so two files sharing a name carry the same value — and exist only in the encrypted output. A secret file that already exists in the repository is left untouched, never re-generated. The module holds no private key and has no decryption code path; a test fails the build if one appears.
