# gitops-commit

Go module that lands rendered files in GitOps repositories as the person: Flux provenance, SOPS encryption for the repository's recipients, branch, commit, pull request and merge.

## Packages

- `provenance` — find the GitHub repository, branch and directory behind a Flux Kustomization.
- `sopsenc` — encrypt secret files for the repository's SOPS recipients and generate secret values.
- `layout` — organize files and plan changes to GitOps directories and workload clusters.
- `commit` — create commits and pull requests, then merge approved changes using the caller's GitHub identity.

See the [development guide](docs/development.md) for package details.

## Who imports it

[GitHub dependents](https://github.com/giantswarm/gitops-commit/network/dependents)

## Identity

The caller supplies the GitHub token used for every operation. Commits, pull requests and merges use the identity associated with that token. The module does not store tokens or private keys, or manage a GitHub App.

If GitHub rejects the token with a 401 or 403 response, the module returns `commit.ErrAuth` with the status code. The caller handles sign-in. The module never retries with a different identity.
