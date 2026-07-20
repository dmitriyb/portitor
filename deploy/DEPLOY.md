# Deploying portitor + running a live dca e2e

The git/gate/role/forward path is unit- and locally-tested; this is the **real**
run: portitor mirrors a GitHub repo, the agent works through it, and a real PR is
opened. Run these on your machine (your keychain, tokens, GitHub).

Prereqs: the PAT in your keychain (`service portitor account dca-sandbox`,
Contents+PR RW on the sandbox), and the dca scripts on PATH (from dot).

## 1. Role keys + portitor config (one-time, ~/.dca-keys)

```bash
mkdir -p ~/.dca-keys/portitor-config/repos.d && cd ~/.dca-keys
for r in implementer reviewer merger; do ssh-keygen -t ed25519 -N "" -C "dca-$r" -f "$r"; done

# allowed_signers (gate verifies commit signatures against this).
# The merger key is deliberately ABSENT: it is a landing-only identity
# (identity_only_roles below) and must never gain commit-signing trust —
# add-role refuses to trust it, and listing it here would collapse that
# isolation.
for r in implementer reviewer; do
  printf 'dca-%s namespaces="git" %s\n' "$r" "$(awk '{print $1,$2}' "$r.pub")"
done > portitor-config/allowed_signers

# repos.d/dca-sandbox.json — DEPLOY POLICY (this file, not portitor, knows the domain).
# The registry (repos.d/<name>.json) is the single canonical config identity: the
# gate hooks, add-role, and the pr API all read this same file.
# portitor's content_rules are generic mechanism: the semantic check command
# below is *this deployment's* record extractor (br, the beads CLI, pinned by
# the toolset overlay); portitor itself ships no knowledge of it.
# Rules: only reviewer closes beads; only reviewer/owner may delete/rename the
# beads files. --all is required (br's default listing hides closed records —
# without it a close would read as a record removal); --limit 0 pins unlimited.
i=$(ssh-keygen -lf implementer.pub|awk '{print $2}')
v=$(ssh-keygen -lf reviewer.pub|awk '{print $2}')
m=$(ssh-keygen -lf merger.pub|awk '{print $2}')
jq -n --arg i "$i" --arg v "$v" --arg m "$m" '{
  format_version: 1,
  default_branch:"main", allowed_signers:"/etc/portitor/allowed_signers", upstream_remote:"upstream",
  upstream_slug:"dmitriyb/dca-sandbox",
  roles: {($i):"implementer", ($v):"reviewer", ($m):"merger"},
  identity_only_roles: ["merger"],
  action_roles: {
    fetch:   ["implementer","reviewer","merger","owner"],
    comment: ["implementer","reviewer","merger","owner"],
    review:  ["reviewer","owner"],
    merge:   ["merger","owner"],
    close:   ["merger","owner"]
  },
  audit_log: "/srv/git/audit/dca-sandbox.jsonl",
  content_rules: {
    version: 1,
    structural: {rules: [{name:"beads-file-reviewer-only", paths:[".beads/**"],
      operations:["delete","rename"], roles:{not_in:["reviewer","owner"]}, effect:"deny"}]},
    semantic: {files: [{path:".beads/issues.jsonl",
      check: {command:["br","--no-db","list","--json","--all","--limit","0"],
              input_file:".beads/issues.jsonl", records_path:"issues", id_field:"id"},
      rules: [{name:"bead-close-reviewer-only",
        match:{type:"field", field:"status", to:"closed"},
        roles:{not_in:["reviewer","owner"]}, effect:"deny"}],
      default:"allow"}]}
  }
}' > portitor-config/repos.d/dca-sandbox.json
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
# add-repo uses the registry conventions: bare at /srv/git/dca-sandbox.git,
# config at /etc/portitor/repos.d/dca-sandbox.json (placed there in step 1).
docker exec -u git portitor portitor add-repo --repo dca-sandbox \
  --upstream https://github.com/dmitriyb/dca-sandbox.git
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
  in `repos.d/dca-sandbox.json` + `authorized_keys` — they're generated together above.
- This spends Anthropic budget (a real `claude` run) and opens a real PR. Reset
  between runs: close the PR, delete the agent branch, `git checkout` the beads
  jsonl in the sandbox.
- If `claude` doesn't honor `HTTPS_PROXY`, the egress lock blocks it — we'd then
  switch to an `ANTHROPIC_BASE_URL` reverse-proxy (the work-mode pattern).
