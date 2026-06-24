# Deploying portitor + running a live dca e2e

The git/gate/role/forward path is unit- and locally-tested; this is the **real**
run: portitor mirrors a GitHub repo, the agent works through it, and a real PR is
opened. Run these on your machine (your keychain, tokens, GitHub).

Prereqs: the PAT in your keychain (`service portitor account dca-sandbox`,
Contents+PR RW on the sandbox), and the dca scripts on PATH (from dot).

## 1. Role keys + portitor config (one-time, ~/.dca-keys)

```bash
mkdir -p ~/.dca-keys/portitor-config && cd ~/.dca-keys
for r in implementer reviewer merger; do ssh-keygen -t ed25519 -N "" -C "dca-$r" -f "$r"; done

# allowed_signers (gate verifies commit signatures against this)
for r in implementer reviewer merger; do
  printf 'dca-%s namespaces="git" %s\n' "$r" "$(awk '{print $1,$2}' "$r.pub")"
done > portitor-config/allowed_signers

# portitor.json (role map by fingerprint + content rule: only reviewer closes beads).
# Build with jq, NOT a heredoc — a shell heredoc mangles the regex backslashes.
i=$(ssh-keygen -lf implementer.pub|awk '{print $2}')
v=$(ssh-keygen -lf reviewer.pub|awk '{print $2}')
m=$(ssh-keygen -lf merger.pub|awk '{print $2}')
jq -n --arg i "$i" --arg v "$v" --arg m "$m" '{
  default_branch:"main", allowed_signers:"/etc/portitor/allowed_signers", upstream_remote:"upstream",
  upstream_slug:"dmitriyb/dca-sandbox",
  roles: {($i):"implementer", ($v):"reviewer", ($m):"merger"},
  role_rules: [{name:"bead-close-reviewer-only", path_glob:".beads/issues.jsonl",
    added_regex:"\"status\"\\s*:\\s*\"closed\"", allowed_roles:["reviewer","owner"]}]
}' > portitor-config/portitor.json
```

## 2. Build the portitor image + deploy

```bash
docker build -t portitor ~/Work/portitor          # from main (after deploy-wiring merges) or the branch

docker network inspect dca-net >/dev/null 2>&1 || docker network create --internal dca-net

~/Work/portitor/deploy/run.sh \
  --config-dir ~/.dca-keys/portitor-config \
  --keys ~/.dca-keys/implementer.pub,~/.dca-keys/reviewer.pub,~/.dca-keys/merger.pub
docker network connect dca-net portitor           # portitor on bridge (GitHub) + dca-net (agent)
```

## 3. Mirror the sandbox

```bash
# Run as the git user: gh's credential helper (for the https fetch) is configured
# for git, not root (docker exec defaults to root).
docker exec -u git portitor portitor init-repo --bare /srv/git/dca-sandbox.git \
  --upstream https://github.com/dmitriyb/dca-sandbox.git \
  --config /etc/portitor/portitor.json
```

## 4. Live run

```bash
dca --repo dca-sandbox --skill implement --bead <open-bead-id>
```

dca builds the lean agent images + the egress proxy on first run, loads the
implementer key from `~/.dca-keys`, and launches the headless agent on `dca-net`.
Flow: clone from portitor → `start-implement` → `claude -p` works + commits +
pushes → gate accepts → forward to `dca-sandbox` → **auto-PR**. Check the PR on
`github.com/dmitriyb/dca-sandbox`.

## Notes / first-run gotchas to expect
- The agent's `PORTITOR_HOST` defaults to `portitor` (the container name on
  `dca-net`); `known_hosts` is `accept-new` for the sandbox.
- The role key in `~/.dca-keys/implementer` must be the same key whose pubkey is
  in `portitor.json` + `authorized_keys` — they're generated together above.
- This spends Anthropic budget (a real `claude` run) and opens a real PR. Reset
  between runs: close the PR, delete the agent branch, `git checkout` the beads
  jsonl in the sandbox.
- If `claude` doesn't honor `HTTPS_PROXY`, the egress lock blocks it — we'd then
  switch to an `ANTHROPIC_BASE_URL` reverse-proxy (the work-mode pattern).
