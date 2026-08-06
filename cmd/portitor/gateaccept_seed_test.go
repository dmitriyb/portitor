//go:build acceptance

package main

// Seed/reset for the disposable GitHub repo the gate acceptance suite drives
// (testdata/realgh.local.json), plus the scenario 3 per-repo config builder.
// The repo starts genuinely empty (spec/proposals/2026-08-05-
// gate-acceptance-suite.md): ensureSeed creates its pristine state once (an
// initial commit + the `gateaccept-seed` tag); resetDisposableRepo restores
// that state before every scenario (closes open PRs, deletes non-default
// branches, force-pushes the default branch back to the tag) — the
// faber-e2e reset shape (shared/.local/bin/faber-e2e), reimplemented here in
// Go and scoped to the forge side only (no gate mirror persists across
// acceptance tests — each scenario stands up its own fresh container).

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
	seedTag   = "gateaccept-seed"
	probeFile = "gateaccept-probe.txt"
	beadsFile = ".beads/issues.jsonl"
	beadID    = "gateaccept-bh-1"

	structRuleBeadsReviewerOnly = "beads-file-reviewer-only"
	semanticRuleBeadClose       = "bead-close-reviewer-only"
	mergeCheckBeadClosed        = "bead-closed"
)

// jsonlWrapScript is the content_rules semantic check command for
// .beads/issues.jsonl: the file is one JSON object per line (beads' own
// jsonl shape); this wraps stdin's lines into the {"issues":[...]} array
// check.Records expects at records_path "issues" — a tiny stand-in for a
// real record-extractor tool (like the `br` CLI DEPLOY.md's worked example
// uses), written directly against alpine's busybox awk/sh so the suite needs
// nothing beyond what the shipped Dockerfile already installs. Verified by
// hand against both BSD awk (macOS) and busybox awk (the gate image) to
// produce valid JSON, including on a single-line/no-trailing-comma input.
const jsonlWrapScript = `awk 'BEGIN{printf "{\"issues\":["} {if(NR>1)printf ","; printf "%s", $0} END{printf "]}"}'`

// beadClosedScript is the merge_gate.checks "bead-closed" predicate: exit 0
// (met) iff the given head (RunPredicate's extraArgs put it at $2, after the
// command's own leading "bead-closed" arg at $0) has a closed bead recorded
// in .beads/issues.jsonl; exit 1 (unmet, incl. when the file/ref is absent —
// fail-closed) otherwise. Runs with cwd = the bare repo dir (RunPredicate's
// workdir contract), where a bare-aware `git show` needs no --git-dir.
const beadClosedScript = `git show "$2":.beads/issues.jsonl 2>/dev/null | grep -q '"status":"closed"'`

// beadRecord is the one line of the seeded .beads/issues.jsonl fixture.
type beadRecord struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Title  string `json:"title"`
}

func seedBeadJSONL(status string) string {
	b, err := json.Marshal(beadRecord{ID: beadID, Status: status, Title: "gateaccept probe bead"})
	if err != nil {
		panic(err) // beadRecord is a fixed, always-marshalable shape
	}
	return string(b) + "\n"
}

// ---- gh api helpers (JSON-parsing; distinct from realgh_test.go's rawGH,
// which merges stdout+stderr and is fine for non-parsed calls) ----

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

// ghAPIOK is ghAPI without the test-fatal: for existence probes (the seed
// tag may legitimately not exist yet) and best-effort cleanup calls.
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
// .mergeable had gone non-null) is what avoids racing that compute. A
// production merge after a review has natural delay; this tight test does not.
func waitMergeClean(t *testing.T, repo string, pr int) {
	t.Helper()
	deadline := time.Now().Add(45 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		out, err := ghAPIOK("pr", "view", strconv.Itoa(pr), "--repo", repo, "--json", "mergeStateStatus", "--jq", ".mergeStateStatus")
		last = strings.TrimSpace(string(out))
		if err == nil && last == "CLEAN" {
			return
		}
		time.Sleep(1 * time.Second)
	}
	t.Fatalf("PR #%d mergeStateStatus never reached CLEAN within 45s (last: %q)", pr, last)
}

// ensureSeed makes the disposable repo's `gateaccept-seed` tag exist,
// creating it (one commit carrying the trivial tracked file + the one-bead
// .beads/issues.jsonl fixture, then the tag) the first time this suite ever
// runs against the repo. Idempotent — a later run finds the tag and returns.
func ensureSeed(t *testing.T, env realGHEnv) {
	t.Helper()
	if _, err := ghAPIOK("api", fmt.Sprintf("repos/%s/git/ref/tags/%s", env.GH.Repo, seedTag)); err == nil {
		return // already seeded
	}
	dir := cloneRepo(t, env) // realgh_test.go: PAT-authenticated clone, signing disabled
	runGit(t, dir, "checkout", "-B", "main")
	if err := os.MkdirAll(filepath.Join(dir, filepath.Dir(beadsFile)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, beadsFile), []byte(seedBeadJSONL("open")), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, probeFile), []byte("gateaccept seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-q", "-m", "gateaccept: seed")
	runGit(t, dir, "push", "-q", "-u", "origin", "main")
	runGit(t, dir, "tag", seedTag)
	runGit(t, dir, "push", "-q", "origin", seedTag)
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
// pristine seed state before every scenario (the faber-e2e reset shape):
// seed if needed, close every open PR, delete every non-default branch, then
// force the default branch back to the seed tag's commit.
func resetDisposableRepo(t *testing.T, env realGHEnv) {
	t.Helper()
	ensureSeed(t, env)

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
// content)"): merge_gate.review "none" + a bead-closed checks predicate,
// content_rules gating a bead-close (status -> closed) to the reviewer role,
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
						Name:       structRuleBeadsReviewerOnly,
						Paths:      []string{".beads/**"},
						Operations: []string{"delete", "rename"},
						Roles:      &rules.RolePredicate{NotIn: []string{"reviewer"}},
						Effect:     "deny",
					}},
				},
				Semantic: &rules.SemanticRules{
					Files: []rules.SemanticFile{{
						Path: beadsFile,
						Check: rules.CheckDef{
							Command:     []string{"/bin/sh", "-c", jsonlWrapScript},
							RecordsPath: "issues",
							IDField:     "id",
						},
						Rules: []rules.SemanticRule{{
							Name:   semanticRuleBeadClose,
							Match:  rules.Matcher{Type: "field", Field: "status", HasTo: true, To: "closed"},
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
				Name:    mergeCheckBeadClosed,
				Command: []string{"/bin/sh", "-c", beadClosedScript, mergeCheckBeadClosed},
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

// closeBead rewrites the beads fixture to "closed" and commits it in dir
// (whose git identity/signing key the caller has already configured via
// gitConfigSigning) — the git-content approval act the single-account world
// (2026-08-05-transparent-approve) uses in place of a native approve.
func closeBead(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, beadsFile), []byte(seedBeadJSONL("closed")), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRun(t, "git", "-C", dir, "add", "-A")
	mustRun(t, "git", "-C", dir, "commit", "-q", "-S", "-m", "gateaccept: close bead "+beadID)
}
