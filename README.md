# portitor

**A self-hosted git gateway between an untrusted agent and your upstream.**

[![Release](https://img.shields.io/github/v/release/dmitriyb/portitor)](https://github.com/dmitriyb/portitor/releases)
[![Go](https://img.shields.io/github/go-mod/go-version/dmitriyb/portitor)](go.mod)
[![License](https://img.shields.io/github/license/dmitriyb/portitor)](LICENSE)
[![CI](https://github.com/dmitriyb/portitor/actions/workflows/ci.yml/badge.svg)](https://github.com/dmitriyb/portitor/actions/workflows/ci.yml)

<!-- recording placeholder: the local gate demo from Quick start, signed push accepted, unsigned push refused -->

```
agent ──ssh──▶ portitor ──┬─ git gate (pre-receive): signed? role? branch? content rules
  (no creds)              ├─ forward (post-receive): mirror accepted feature branches upstream
                          ├─ auto-PR: open a PR for each forwarded branch
                          └─ action API (portitor pr): role-gated comment/review/merge/close/fetch
                                                       (the ONLY GitHub credential lives here)
```

portitor is the hard enforcement boundary for an agent that works unattended: it judges the **result** of a push, not the commands that produced it, and it is the only component that holds a credential for the upstream.
Its only runtime dependency is git.

## What it does

- **Verifies the result of a push.** A `pre-receive` hook inspects the objects being landed: every introduced commit must be signed by a trusted key, only branch refs are accepted, and the default branch is never a push target. Any internal error rejects the push.
- **Maps the signer's key fingerprint to a role.** The role follows the key, not a label in the commit, so a container holding one role's key cannot act as another role.
- **Is the only holder of the forge credential.** The agent reaches portitor over SSH through a forced command and never sees a GitHub token.
- **Forwards accepted branches and opens the PR.** A `post-receive` hook mirrors an accepted feature branch upstream with portitor's credential and opens a pull request for it.
- **Exposes a role-gated action API.** `portitor pr` runs comment, review, merge, close, checks and similar actions after a default-deny role check; merge preconditions are re-derived from upstream state, never taken from the request.

Every domain name (roles, protected paths, record fields, the record-extraction command) is configuration; portitor ships none.
See [`docs/architecture.md`](docs/architecture.md) for the full model.

## Install

The `portitor` binary is published on the [GitHub Releases page](https://github.com/dmitriyb/portitor/releases) for linux/darwin, amd64/arm64.
The install script is verified against the release signing key before it runs, and the script verifies the binary the same way.

```bash
curl -fsSL https://github.com/dmitriyb/portitor/releases/latest/download/install.sh     -o install.sh \
&& curl -fsSL https://github.com/dmitriyb/portitor/releases/latest/download/install.sh.sig -o install.sh.sig \
&& ssh-keygen -Y verify -f <(printf 'dvbozhko@gmail.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIIhmCWVDP/Tcm3CqXNjTQTChbKxr223xMob9zc56Uuny release signing\n') \
     -I dvbozhko@gmail.com -n file -s install.sh.sig < install.sh \
&& sh install.sh \
&& rm -f install.sh install.sh.sig
```

<details>
<summary>fish</summary>

```fish
curl -fsSL https://github.com/dmitriyb/portitor/releases/latest/download/install.sh -o install.sh
and curl -fsSL https://github.com/dmitriyb/portitor/releases/latest/download/install.sh.sig -o install.sh.sig
and ssh-keygen -Y verify -f (printf 'dvbozhko@gmail.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIIhmCWVDP/Tcm3CqXNjTQTChbKxr223xMob9zc56Uuny release signing\n' | psub) -I dvbozhko@gmail.com -n file -s install.sh.sig < install.sh
and sh install.sh
and rm -f install.sh install.sh.sig
```

</details>

<details>
<summary>plain sh (no process substitution)</summary>

```sh
printf 'dvbozhko@gmail.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIIhmCWVDP/Tcm3CqXNjTQTChbKxr223xMob9zc56Uuny release signing\n' > allowed_signers
ssh-keygen -Y verify -f allowed_signers -I dvbozhko@gmail.com -n file -s install.sh.sig < install.sh
sh install.sh
```

</details>

<details>
<summary>maximal: verify the archive directly, no script</summary>

```bash
ssh-keygen -Y verify -f allowed_signers -I dvbozhko@gmail.com -n file \
  -s portitor_<version>_<os>_<arch>.tar.gz.sig < portitor_<version>_<os>_<arch>.tar.gz
gh attestation verify portitor_<version>_<os>_<arch>.tar.gz --repo dmitriyb/portitor
go install github.com/dmitriyb/portitor/cmd/portitor@<tag>
```

</details>

<details>
<summary>upgrading</summary>

`portitor upgrade` updates the installed binary in place through the same signed installer. It is forward-only and refuses a "latest" that is older than the installed version; `--version vX.Y.Z` installs an exact release, `--rollback` restores the previous binary.

</details>

The container image (gate + egress) is not a release artifact: build it from this repository's `Dockerfile` with `docker build -t portitor .`.
The public key, what each channel protects, and the residual risks are in [`docs/install.md`](docs/install.md).

## Quick start

Part 1 needs git, ssh-keygen and the `portitor` binary on `PATH`. No container, no GitHub, no network.

Operator side: one signing key, a config that binds its fingerprint to a role, and a gated bare repo.

```bash
mkdir demo && cd demo
ssh-keygen -q -t ed25519 -N '' -C implementer -f implementer
mkdir repos.d
echo "implementer namespaces=\"git\" $(cut -d' ' -f1,2 implementer.pub)" > allowed_signers
FP=$(ssh-keygen -lf implementer.pub | cut -d' ' -f2)
printf '{"format_version":1,"default_branch":"main","allowed_signers":"%s","roles":{"%s":"implementer"}}\n' "$PWD/allowed_signers" "$FP" > repos.d/demo.json
portitor validate-config --config repos.d/demo.json
portitor init-repo --bare demo.git --config "$PWD/repos.d/demo.json"
```

Agent side: a work repo that signs with that key, and a signed push of a feature branch.

```bash
git init -q -b main work && cd work
git config user.name implementer
git config user.email implementer@example.com
git config gpg.format ssh
git config user.signingkey ../implementer
git config commit.gpgsign true
echo hello > hello.txt && git add hello.txt && git commit -q -m "add hello"
git push ../demo.git HEAD:refs/heads/feature
```

```
remote: portitor: refs/heads/feature accepted, not forwarded (no upstream remote "upstream" in this repo)
To ../demo.git
 * [new branch]      HEAD -> feature
```

The gate accepted the branch. With `--upstream` set on `init-repo`, the same line reads `forwarded refs/heads/feature -> upstream`, followed by the PR number.

Now an unsigned commit, and a push straight to the default branch:

```bash
echo unsigned > unsigned.txt && git add unsigned.txt && git commit -q --no-gpg-sign -m "add unsigned"
git push ../demo.git HEAD:refs/heads/feature-unsigned
git push ../demo.git HEAD~1:refs/heads/main
```

```
remote: portitor: push rejected
remote:   [unsigned-or-untrusted-commit] refs/heads/feature-unsigned: commit 2c6130b62b63 is not signed by an allowed signer (no signature)
 ! [remote rejected] HEAD -> feature-unsigned (pre-receive hook declined)

remote: portitor: push rejected
remote:   [no-push-to-default] refs/heads/main: push to the default branch "main" is not allowed — use a feature branch and open a PR
 ! [remote rejected] HEAD~1 -> main (pre-receive hook declined)
```

Part 2 is the deployed shape: the gate in a container, the agent reaching it over SSH, an upstream on GitHub.

```bash
docker build -t portitor .
deploy/run.sh --config-dir ./portitor-config --keys ./implementer.pub
docker exec -u git portitor portitor add-repo --repo myrepo --upstream https://github.com/you/myrepo.git
docker exec -u git portitor portitor add-role --repo myrepo --role implementer --fingerprint SHA256:… --pub ./implementer.pub
```

The agent then clones and pushes `ssh://git@portitor/srv/git/myrepo.git`; an accepted branch is forwarded and its PR number printed back over the push.
Registry layout, the PAT source, and network attachment are in [`docs/deploy.md`](docs/deploy.md).

## How it compares

| | Where the check runs | Bypassed by | Who holds the credential |
|---|---|---|---|
| Branch protection + required reviews | On the forge, after the push has already reached it | Any identity the rule exempts; nothing gates *what* a given role may change inside a file | The agent, since it must push to the forge itself |
| Agent-side deny lists, managed settings | Inside the agent's environment, on the command string | Any command the list does not name, or a tool that writes git objects directly | The agent |
| A bot token held by the agent | Nowhere before the push; forge rules only | Anything the token's scope permits | The agent |
| portitor | Server side, in `pre-receive`, on the resulting objects | Nothing short of a trusted role's signing key | portitor only |

The argument in one line: trust the gate, not the agent. A tricked agent, a buggy agent and a malicious agent hit the same wall, because the wall judges output, not intent.

## Learn more

- [`docs/deploy.md`](docs/deploy.md): registry, container bring-up, provisioning, the MCP mediator.
- [`docs/configuration.md`](docs/configuration.md): the per-repo config schema, `allowed_signers`, content rules, the multi-repo registry.
- [`docs/commands.md`](docs/commands.md): the full command reference and the `pr` action API.
- [`docs/architecture.md`](docs/architecture.md): how the gate decides, the three enforcement tiers, links to the spec.
- [`docs/install.md`](docs/install.md): every install channel, the public key, upgrading.
- [`deploy/DEPLOY.md`](deploy/DEPLOY.md): a live end-to-end runbook against a real GitHub sandbox.
- `spec/**`: the authoritative, requirement-level specification (spexmachina format).

## License

Apache-2.0 (see `LICENSE`).
