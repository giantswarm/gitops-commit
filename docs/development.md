# Developing on gitops-commit

## Packages

### `provenance`: resolve GitOps locations

`Flux.Resolve(namespace, name)` follows a Flux Kustomization to its GitRepository and returns the GitHub repository, branch and directory (`spec.path`). `Flux.FindKustomization` returns the Kustomization itself, including its service account, interval, timeout, prune setting and keys. Use `Explicit(repository, branch, directory)` when the caller already knows the location.

The caller supplies the data as typed structs or decoded objects through `Flux.Add`. The package does not contact a cluster.

### `sopsenc`: encrypt and generate secrets

The repository's `.sops.yaml` determines which files need encryption. A file matching a creation rule's `path_regex` is encrypted for that rule's age recipients, regardless of its name. For example, a rule can cover every file under `secrets/`. A file whose name contains `secret` or `credential` is refused if no rule covers it, so it cannot be written in plaintext.

Only public recipient keys are needed. The module holds no private key and has no decryption code path; a test enforces this restriction. By default, secret files that already exist in the repository are left untouched. Callers can explicitly replace them through `layout.ReplaceSecrets`, described below.

#### Generated values

Values declared as `Generated` are created with `crypto/rand` at commit time. Each name is generated once, so files sharing a name receive the same value. The declaration specifies whether its placeholder receives the raw value or its base64 encoding. Use the encoded form when the consumer decodes the value, such as a Kubernetes Secret's `data` field or a chart that copies the value into `data` without encoding it.

Three kinds are supported:

- `base64`: random bytes.
- `alphanumeric`: random characters.
- `keypair-es256`: an ECDSA P-256 key pair. The private key uses PKCS #8 PEM and the public key uses SubjectPublicKeyInfo PEM, both written as single-line YAML scalars. Each declaration selects which half its placeholder receives.

Generated secrets appear only in encrypted output. The public half of a key pair may also appear in plain files; the private half is allowed only in secret files. Existing secret files are not regenerated unless explicitly replaced.

### `layout`: plan file and cluster changes

#### Writer directories

A `Directory` gives a writer its own directory, such as `cluster-manager` or `model-manager`, under the directory built by a Flux Kustomization. It contains one file per object and a `kustomization.yaml` listing those files. `ObjectFile` places each Secret in its own secret file. When a parent `kustomization.yaml` exists, it lists the writer's directory; the layout also works without a parent file.

`Build` compares the proposed files with the base branch and produces a plan of added, updated, removed and unchanged files. Its `Change` can be passed to `commit.Open`. Repeating a write whose files are already on the base branch produces no changes, unless it explicitly replaces a secret file.

New secret files are encrypted for the recipients in `.sops.yaml`. Existing secret files are left untouched unless the caller passes `ReplaceSecrets(paths...)` to `Build`:

```go
layout.Build(ctx, r, dir, write, remove, layout.ReplaceSecrets(path))
```

Each replacement path must be a secret file included in `write`. Its new value is encrypted for the repository's recipients under the same path, replacing the old content. Replacing an existing secret is always an `Update`, even on repeated runs, because SOPS produces new ciphertext each time. The old content is never decrypted for comparison.

If a new or replacement secret file cannot be encrypted for the repository, `Build` returns a `SecretError`, including the missing creation rule when applicable.

Both the writer's and parent's kustomization files are edited as YAML nodes to preserve comments. The writer's kustomization file and its parent entry are removed with the last file.

#### Workload clusters

`NewCluster` creates a `Cluster` from the Flux Kustomization that reconciles its Organization. `BuildCluster` follows the gitops-template layout:

- Cluster objects and their `kustomization.yaml` go in `organizations/<org>/workload-clusters/<cluster>/`.
- A sibling `<cluster>.yaml` defines the Flux Kustomization `<installation>-clusters-<cluster>`.
- `workload-clusters/kustomization.yaml` lists the cluster's Flux Kustomization file and is created if absent.

The cluster's Flux Kustomization inherits the Organization's source, service account, interval, timeout and keys. It sets `prune: true` and omits `postBuild`, so `${…}` in user values stays as written. `BuildCluster` refuses a root that has no `organizations/<org>/` directory and includes the missing path in its error.

`RemoveCluster` removes the cluster directory, the Flux Kustomization file and its entry in the parent kustomization. `ClusterPlan.Prune` indicates whether merging alone removes the cluster. If it does not, the caller must also delete the live per-cluster Kustomization so Flux can collect the cluster.

A writer adding objects later, such as node pools, uses its own `Directory` under the cluster directory.

### `commit`: write and merge changes

#### Connect to GitHub

Create a `Remote` with `NewGitHub` using the caller's token, or with `NewGitHubWithClient` using an HTTP client that already carries the intended identity. The latter supports transports such as a GitHub App installation transport or a rotated token file.

`ReadFile`, available through the `Reader` interface on `GitHub` and `Fake`, reads a file at a branch's head. Callers use it to read files such as `kustomization.yaml` and `.sops.yaml`. A missing file returns `ErrFileNotFound`. `ListFiles`, available through the `Lister` interface, lists every file under a directory for operations such as removing a whole directory.

#### Open a pull request

`Open` takes a `Remote`, a `Request` containing the head branch, title and body, and one `Change` per location. For each repository it creates one branch, one commit containing all changes, and one pull request using the caller's title and body. A path with nil content is deleted.

An existing open pull request for the same branch is reused. If an earlier run left the branch behind its base, the base is merged into it using the caller's identity. The module never force-pushes. If the merge cannot be completed, `Open` returns `ErrStaleBranch` identifying the branch and its pull request. This ensures files prepared from the current base are committed to a branch that includes that base.

#### Merge and manage pull requests

`Merge` requires the caller's approval, passing statuses and check runs on the head, and a head commit matching the one the caller reviewed. GitHub also enforces the repository's branch protection and rulesets, including required approving reviews. The module provides no administrative bypass.

The `Remote` interface also supports the caller's pull request workflow:

- `FindPullRequest` finds the open pull request for a branch.
- `OpenDraftPullRequest` opens a draft.
- `EnableAutoMerge` enables GitHub auto-merge with the repository's merge method. The caller explicitly chooses whether to enable it.
- `Approve` submits an approving review using the caller's identity, pinned to the head the caller reviewed.
- `Close` closes a pull request and optionally deletes its head branch. An already closed pull request is left alone.
- `Revert` opens a pull request undoing a merged change. It removes added files, restores changed files and allows overrides for named paths. The revert branch starts at the base's tip or is brought up to date with it like any other branch. A repeated revert adds a commit to the open revert pull request; history is never rewritten.

`Fake` is an in-process remote for this module's tests and its callers' tests. It implements every remote operation.
