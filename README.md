# portitor

*A self-hosted git gateway between an untrusted agent and your upstream.*

[![release](https://img.shields.io/github/v/release/dmitriyb/portitor)](https://github.com/dmitriyb/portitor/releases)
[![go](https://img.shields.io/github/go-mod/go-version/dmitriyb/portitor)](go.mod)
[![license](https://img.shields.io/github/license/dmitriyb/portitor)](LICENSE)
[![ci](https://github.com/dmitriyb/portitor/actions/workflows/ci.yml/badge.svg)](https://github.com/dmitriyb/portitor/actions/workflows/ci.yml)

<!-- terminal recording: the local gate demo from Quick start, a signed push accepted, an unsigned push refused. Placeholder until recorded. -->

`portitor` is the hard enforcement boundary for an agent that works unattended.
It judges the result of a push, not the commands that produced it, and it is the
only component that holds a credential for the upstream. Its only runtime
dependency is git.

```
agent ──ssh──▶ portitor ──┬─ git gate (pre-receive): signed? role? branch? content rules
  (no creds)              ├─ forward (post-receive): mirror accepted feature branches upstream
                          ├─ auto-PR: open a PR for each forwarded branch
                          └─ action API (portitor pr): role-gated comment/review/merge/close/fetch
                                                       (the ONLY GitHub credential lives here)
```

## What it does

- **Verifies the result of a push.** A `pre-receive` hook inspects the objects being landed: every introduced commit must be signed by a trusted key, only branch refs are accepted, and the default branch is never a push target. Any internal error rejects the push.
- **Maps the signer's key fingerprint to a role.** The role follows the key, not a label in the commit, so a container holding one role's key cannot act as another role.
- **Is the only holder of the forge credential.** The agent reaches portitor over SSH through a forced command and never sees a GitHub token.
- **Forwards accepted branches and opens the PR.** A `post-receive` hook mirrors an accepted feature branch upstream with portitor's credential and opens a pull request for it.
- **Exposes a role-gated action API.** `portitor pr` runs comment, review, merge, close, checks and similar actions after a default-deny role check; merge preconditions are re-derived from upstream state, never taken from the request.

Every domain name (roles, protected paths, record fields, the record-extraction command) is configuration; portitor ships none. See [`docs/architecture.md`](docs/architecture.md).

## Install

Download the install script, verify it, then run it. Never `curl | sh`: a piped script cannot verify itself before it runs. The container image (gate + egress) is not a release artifact: build it from this repository's `Dockerfile` with `docker build -t portitor .`. Details, other shells and the trust model are in [`docs/install.md`](docs/install.md).

```bash
curl -fsSL https://github.com/dmitriyb/portitor/releases/latest/download/install.sh     -o install.sh \
&& curl -fsSL https://github.com/dmitriyb/portitor/releases/latest/download/install.sh.sig -o install.sh.sig \
&& ssh-keygen -Y verify -f <(printf 'dvbozhko@gmail.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIIhmCWVDP/Tcm3CqXNjTQTChbKxr223xMob9zc56Uuny release signing\n') \
     -I dvbozhko@gmail.com -n file -s install.sh.sig < install.sh \
&& sh install.sh \
&& rm -f install.sh install.sh.sig
```

Public key, pin it once:

```
dvbozhko@gmail.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIIhmCWVDP/Tcm3CqXNjTQTChbKxr223xMob9zc56Uuny release signing
```

<details>
<summary>fish, plain sh, verifying the archive directly, upgrading</summary>

- **fish** and **plain `sh`** variants of the block above: [`docs/install.md`](docs/install.md#primary-verified-install-script).
- **Maximal**: skip the script and verify the release archive itself by SSHSIG, SLSA attestation or the Go checksum database: [`docs/install.md`](docs/install.md#maximal-verify-the-binary-archive-directly).
- **Upgrading**: `portitor upgrade` runs the same signed installer against its own path, forward-only; `--check`, `--version`, `--rollback`: [`docs/install.md`](docs/install.md#upgrading).

</details>

## Quick start

Needs git, ssh-keygen and `portitor` on `PATH`. No container, no GitHub, no network.

```sh
mkdir demo && cd demo
ssh-keygen -q -t ed25519 -N '' -C implementer -f implementer      # the one signing key
mkdir repos.d
echo "implementer namespaces=\"git\" $(cut -d' ' -f1,2 implementer.pub)" > allowed_signers
FP=$(ssh-keygen -lf implementer.pub | cut -d' ' -f2)
printf '{"format_version":1,"default_branch":"main","allowed_signers":"%s","roles":{"%s":"implementer"}}\n' "$PWD/allowed_signers" "$FP" > repos.d/demo.json
portitor validate-config --config repos.d/demo.json               # fingerprint -> role, bound in config
portitor init-repo --bare demo.git --config "$PWD/repos.d/demo.json"   # the gated bare repo

git init -q -b main work && cd work                               # the agent side: a repo that signs with that key
git config user.name implementer
git config user.email implementer@example.com
git config gpg.format ssh
git config user.signingkey ../implementer
git config commit.gpgsign true
echo hello > hello.txt && git add hello.txt && git commit -q -m "add hello"
git push ../demo.git HEAD:refs/heads/feature                      # accepted: signed, feature branch
echo unsigned > unsigned.txt && git add unsigned.txt && git commit -q --no-gpg-sign -m "add unsigned"
git push ../demo.git HEAD:refs/heads/feature-unsigned             # rejected: unsigned-or-untrusted-commit
git push ../demo.git HEAD~1:refs/heads/main                       # rejected: no-push-to-default
```

That is the gate in one sitting: a signed commit on a feature branch lands, an unsigned one is refused by name, and a push to the default branch is refused whatever it carries. With `--upstream` set on `init-repo`, an accepted branch is forwarded and its PR number is printed back over the push. The deployed shape, the gate in a container reached over SSH with an upstream on GitHub, is four commands in [`docs/deploy.md`](docs/deploy.md): `docker build`, `deploy/run.sh`, `add-repo`, `add-role`.

## How it compares

| | Where the check runs | Bypassed by | Who holds the credential |
|---|---|---|---|
| Branch protection + required reviews | On the forge, after the push has already reached it | Any identity the rule exempts; nothing gates *what* a given role may change inside a file | The agent, since it must push to the forge itself |
| Agent-side deny lists, managed settings | Inside the agent's environment, on the command string | Any command the list does not name, or a tool that writes git objects directly | The agent |
| A bot token held by the agent | Nowhere before the push; forge rules only | Anything the token's scope permits | The agent |
| portitor | Server side, in `pre-receive`, on the resulting objects | Nothing short of a trusted role's signing key | portitor only |

The argument in one line: trust the gate, not the agent. A tricked agent, a buggy agent and a malicious agent hit the same wall, because the wall judges output, not intent.

## Learn more

- [`docs/deploy.md`](docs/deploy.md) — registry, container bring-up, provisioning, the MCP mediator.
- [`docs/configuration.md`](docs/configuration.md) — the per-repo config schema, `allowed_signers`, content rules, the multi-repo registry.
- [`docs/commands.md`](docs/commands.md) — the full command reference and the `pr` action API.
- [`docs/architecture.md`](docs/architecture.md) — how the gate decides, the three enforcement tiers, links to the spec.
- [`docs/install.md`](docs/install.md) — every install channel, the public key, upgrading.
- [`deploy/DEPLOY.md`](deploy/DEPLOY.md) — a live end-to-end runbook against a real GitHub sandbox.
- `spec/**` — the authoritative, requirement-level specification (spexmachina format).

## License

Apache-2.0, see [`LICENSE`](LICENSE).
