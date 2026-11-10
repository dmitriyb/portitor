//go:build acceptance

package main

// Seed/reset for the disposable GitHub repo the gate acceptance suite drives
// (PORTITOR_ACCEPT_REPO), plus the scenario 3 per-repo config builder.
// The repo's pristine state is prepared once by the operator from
// testdata/acceptance-seed (scripts/prepare-acceptance-seed.sh) and tagged
// with testdata/acceptance-seed.tag: it carries a workflow file, which the
// suite's own PAT is deliberately unable to push. verifySeed proves the tag
// still holds exactly that tree; resetDisposableRepo restores that state
// before every scenario (closes open PRs, deletes non-default
// branches, force-pushes the default branch back to the tag), scoped to the
// forge side only (no gate mirror persists across acceptance tests — each
// scenario stands up its own fresh gate).

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dmitriyb/portitor/internal/action"
	"github.com/dmitriyb/portitor/internal/config"
	"github.com/dmitriyb/portitor/internal/gate"
	"github.com/dmitriyb/portitor/internal/rules"
)

const (
	probeFile     = "gateaccept-probe.txt"
	approvalsFile = "approvals.jsonl"
	approvalID    = "gateaccept-change-1"
	seedDir       = "testdata/acceptance-seed"

	structRuleApprovalsReviewerOnly = "approvals-file-reviewer-only"
	semanticRuleApprove             = "approve-reviewer-only"
	mergeCheckApproved              = "approved"
)

// seedTag is the tag the operator put on the prepared seed.
var seedTag = func() string {
	b, err := os.ReadFile("testdata/acceptance-seed.tag")
	if err != nil {
		panic(err) // the fixture is part of the tree; its absence is a broken checkout
	}
	return strings.TrimSpace(string(b))
}()

// jsonlWrapScript is the content_rules semantic check command for the
// approvals file (one JSON object per line): it wraps stdin's lines into the
// {"records":[...]} array check.Records expects at records_path "records" — a
// stand-in for a real record-extractor tool, written against plain POSIX awk/sh
// so it needs nothing beyond the gate image's fixed part (gawk).
const jsonlWrapScript = `awk 'BEGIN{printf "{\"records\":["} {if(NR>1)printf ","; printf "%s", $0} END{printf "]}"}'`

// approvedScript is the merge_gate.checks "approved" predicate: exit 0 (met)
// iff the given head (RunPredicate's extraArgs put it at $2, after the
// command's own leading arg at $0) records the change as approved; exit 1
// otherwise, including when the file or ref is absent. It needs grep, which is
// not in the gate image's fixed part: the scenario declares it as an image
// package, the way an operator declares the tools their policy calls.
const approvedScript = `git show "$2":` + approvalsFile + ` 2>/dev/null | grep -q '"state":"approved"'`

// approvalRecord is the one line of the seeded approvals fixture.
type approvalRecord struct {
	ID    string `json:"id"`
	State string `json:"state"`
}

func seedApprovalJSONL(state string) string {
	b, err := json.Marshal(approvalRecord{ID: approvalID, State: state})
	if err != nil {
		panic(err) // approvalRecord is a fixed, always-marshalable shape
	}
	return string(b) + "\n"
}

// ---- gh api helpers (JSON-parsing; distinct from rawGH, which merges
// stdout+stderr and is fine for non-parsed calls) ----

// ghAPI runs `gh <args...>`, capturing stdout/stderr separately, and fails
// the test loudly on error — for calls whose JSON stdout this file parses.
func ghAPI(t *testing.T, args ...string) []byte {
	t.Helper()
	out, err := ghAPIOK(args...)
	if err != nil {
		t.Fatalf("gh %s: %v", strings.Join(args, " "), err)
	}
	return out
}

// ghAPIOK is ghAPI without the test-fatal: for existence probes whose failure
// the caller reports itself, and best-effort cleanup calls.
func ghAPIOK(args ...string) ([]byte, error) {
	cmd := exec.Command("gh", args...)
	cmd.Env = os.Environ()
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(errb.String()))
	}
	return out.Bytes(), nil
}

// waitMergeClean polls the PR's mergeStateStatus — the EXACT GraphQL field the
// gate's merge precondition checks — until it is CLEAN. Right after a push
// GitHub reports UNKNOWN while it recomputes mergeability asynchronously; the
// gate correctly refuses a non-CLEAN state, so a merge issued in that window
// fails spuriously. Polling this same field (not REST .mergeable, which can
// settle independently — GraphQL mergeStateStatus was still UNKNOWN after REST
// .mergeable had gone non-null) is what avoids racing that compute. It waits
// for CLEAN of head specifically: right after a push GitHub can still report
// the previous head's CLEAN, and a merge issued then meets the new head's
// UNKNOWN. A production merge after a review has natural delay; this tight
// test does not.
func waitMergeClean(t *testing.T, repo string, pr int, head string) {
	t.Helper()
	deadline := time.Now().Add(45 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		out, err := ghAPIOK("pr", "view", strconv.Itoa(pr), "--repo", repo, "--json", "mergeStateStatus,headRefOid",
			"--jq", `.headRefOid + " " + .mergeStateStatus`)
		last = strings.TrimSpace(string(out))
		if err == nil && last == head+" CLEAN" {
			return
		}
		time.Sleep(1 * time.Second)
	}
	t.Fatalf("PR #%d never reached CLEAN at head %s within 45s (last: %q)", pr, head, last)
}

// verifySeed fails unless the disposable repo's seed tag exists and its tree is
// exactly testdata/acceptance-seed, path for path and byte for byte — a stale
// or hand-edited seed would otherwise make every scenario test the wrong
// starting state.
func verifySeed(t *testing.T, env realGHEnv) {
	t.Helper()
	fix := "prepare it: clone the disposable repo, run scripts/prepare-acceptance-seed.sh <clone>, then push main and " + seedTag + " as it prints"
	if _, err := ghAPIOK("api", fmt.Sprintf("repos/%s/git/ref/tags/%s", env.GH.Repo, seedTag)); err != nil {
		t.Fatalf("the disposable repo %s has no seed tag %s: %v\n%s", env.GH.Repo, seedTag, err, fix)
	}
	want := map[string][]byte{}
	err := filepath.WalkDir(seedDir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(seedDir, p)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(p)
		want[filepath.ToSlash(rel)] = b
		return err
	})
	if err != nil {
		t.Fatalf("read %s: %v", seedDir, err)
	}
	dir := cloneRepo(t, env)
	runGit(t, dir, "fetch", "-q", "origin", "tag", seedTag)
	got := strings.Split(strings.TrimSuffix(runGit(t, dir, "ls-tree", "-r", "-z", "--name-only", seedTag), "\x00"), "\x00")
	if len(got) != len(want) {
		t.Fatalf("seed tag %s holds %v, want exactly the files of %s\n%s", seedTag, got, seedDir, fix)
	}
	for _, path := range got {
		w, ok := want[path]
		if !ok || runGit(t, dir, "show", seedTag+":"+path) != string(w) {
			t.Fatalf("seed tag %s: %s differs from %s\n%s", seedTag, path, seedDir, fix)
		}
	}
}

type ghPR struct {
	Number int `json:"number"`
}
type ghBranch struct {
	Name string `json:"name"`
}
type ghRepoInfo struct {
	DefaultBranch string `json:"default_branch"`
}
type ghRef struct {
	Object struct {
		SHA  string `json:"sha"`
		Type string `json:"type"`
	} `json:"object"`
}
type ghTagObject struct {
	Object struct {
		SHA string `json:"sha"`
	} `json:"object"`
}

// resetDisposableRepo restores the forge side of the disposable repo to its
// pristine seed state before every scenario: verify the seed, close every open
// PR, delete every non-default branch, then force the default branch back to
// the seed tag's commit.
func resetDisposableRepo(t *testing.T, env realGHEnv) {
	t.Helper()
	verifySeed(t, env)

	var prs []ghPR
	if err := json.Unmarshal(ghAPI(t, "api", fmt.Sprintf("repos/%s/pulls?state=open&per_page=100", env.GH.Repo)), &prs); err != nil {
		t.Fatalf("parse open PRs: %v", err)
	}
	for _, pr := range prs {
		if _, err := ghAPIOK("api", fmt.Sprintf("repos/%s/pulls/%d", env.GH.Repo, pr.Number), "-X", "PATCH", "-f", "state=closed"); err != nil {
			t.Logf("reset: close PR #%d: %v (best-effort)", pr.Number, err)
		}
	}

	var repoInfo ghRepoInfo
	if err := json.Unmarshal(ghAPI(t, "api", fmt.Sprintf("repos/%s", env.GH.Repo)), &repoInfo); err != nil {
		t.Fatalf("parse repo info: %v", err)
	}
	def := repoInfo.DefaultBranch
	if def == "" {
		def = "main"
	}

	var branches []ghBranch
	if err := json.Unmarshal(ghAPI(t, "api", fmt.Sprintf("repos/%s/branches?per_page=100", env.GH.Repo)), &branches); err != nil {
		t.Fatalf("parse branches: %v", err)
	}
	for _, b := range branches {
		if b.Name == def {
			continue
		}
		if _, err := ghAPIOK("api", fmt.Sprintf("repos/%s/git/refs/heads/%s", env.GH.Repo, url.PathEscape(b.Name)), "-X", "DELETE"); err != nil {
			t.Logf("reset: delete branch %s: %v (best-effort)", b.Name, err)
		}
	}

	var tagRef ghRef
	if err := json.Unmarshal(ghAPI(t, "api", fmt.Sprintf("repos/%s/git/ref/tags/%s", env.GH.Repo, seedTag)), &tagRef); err != nil {
		t.Fatalf("parse seed tag ref: %v", err)
	}
	sha := tagRef.Object.SHA
	if tagRef.Object.Type == "tag" { // annotated tag: dereference to the commit
		var tagObj ghTagObject
		if err := json.Unmarshal(ghAPI(t, "api", fmt.Sprintf("repos/%s/git/tags/%s", env.GH.Repo, sha)), &tagObj); err != nil {
			t.Fatalf("dereference annotated seed tag: %v", err)
		}
		sha = tagObj.Object.SHA
	}
	if len(sha) != 40 {
		t.Fatalf("resolved seed tag sha %q is not a full commit sha", sha)
	}
	if _, err := ghAPIOK("api", fmt.Sprintf("repos/%s/git/refs/heads/%s", env.GH.Repo, def), "-X", "PATCH", "-f", "sha="+sha, "-F", "force=true"); err != nil {
		t.Fatalf("reset: force %s -> seed tag %s: %v", def, seedTag, err)
	}
}

// ---- scenario 3 per-repo config ----

// scenario3Settings builds the single-account merge model config (spec/
// proposals/2026-08-05-transparent-approve.md "single account (git
// content)"): merge_gate.review "none" + an "approved" checks predicate,
// content_rules gating the approval (state -> approved) to the reviewer role,
// and the merger role landing-only (identity_only_roles) — it signs nothing,
// it only merges/closes.
func scenario3Settings(roles map[string]roleKey, upstreamSlug, allowedSignersPath string) config.Settings {
	rolesMap := make(map[string]string, len(roles))
	for _, rk := range roles {
		rolesMap[rk.fingerprint] = rk.role
	}
	return config.Settings{
		FormatVersion: config.SupportedFormatVersion,
		Config: gate.Config{
			DefaultBranch:  "main",
			AllowedSigners: allowedSignersPath,
			Roles:          rolesMap,
			Content: &rules.ContentRules{
				Version: rules.SupportedVersion,
				Structural: &rules.StructuralRules{
					Rules: []rules.StructuralRule{{
						Name:       structRuleApprovalsReviewerOnly,
						Paths:      []string{approvalsFile},
						Operations: []string{"delete", "rename"},
						Roles:      &rules.RolePredicate{NotIn: []string{"reviewer"}},
						Effect:     "deny",
					}},
				},
				Semantic: &rules.SemanticRules{
					Files: []rules.SemanticFile{{
						Path: approvalsFile,
						Check: rules.CheckDef{
							Command:     []string{"/bin/sh", "-c", jsonlWrapScript},
							RecordsPath: "records",
							IDField:     "id",
						},
						Rules: []rules.SemanticRule{{
							Name:   semanticRuleApprove,
							Match:  rules.Matcher{Type: "field", Field: "state", HasTo: true, To: "approved"},
							Roles:  &rules.RolePredicate{NotIn: []string{"reviewer"}},
							Effect: "deny",
						}},
						Default: "allow",
					}},
				},
			},
		},
		UpstreamSlug: upstreamSlug,
		ActionRoles: map[string][]string{
			"fetch":    {"implementer", "reviewer", "merger"},
			"comment":  {"implementer", "reviewer", "merger"},
			"review":   {"reviewer"},
			"describe": {"implementer"},
			"merge":    {"merger"},
			"close":    {"merger"},
		},
		MergeGate: &action.MergeGateConfig{
			Review: "none",
			Checks: []action.CheckPredicate{{
				Name:    mergeCheckApproved,
				Command: []string{"/bin/sh", "-c", approvedScript, mergeCheckApproved},
			}},
		},
		IdentityOnlyRoles: []string{"merger"},
	}
}

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

// approveChange rewrites the approvals fixture to "approved" and commits it in
// dir (whose git identity/signing key the caller has already configured via
// gitConfigSigning) — the git-content approval act the single-account world
// (2026-08-05-transparent-approve) uses in place of a native approve.
func approveChange(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, approvalsFile), []byte(seedApprovalJSONL("approved")), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRun(t, "git", "-C", dir, "add", "-A")
	mustRun(t, "git", "-C", dir, "commit", "-q", "-S", "-m", "gateaccept: approve "+approvalID)
}
