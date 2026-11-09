package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
}

// writeConfig writes a minimal registry config; extra is spliced in as more
// JSON members.
func writeConfig(t *testing.T, reposDir, name, extra string) string {
	t.Helper()
	if err := os.MkdirAll(reposDir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(reposDir, name+".json")
	body := `{"format_version":1,"default_branch":"main"` + extra + `}`
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// writeValidConfig writes a registry config that passes validation; extra is
// spliced in as more JSON members.
func writeValidConfig(t *testing.T, root, reposDir, name, extra string) string {
	t.Helper()
	signers := filepath.Join(root, "allowed_signers")
	must(t, os.WriteFile(signers, nil, 0o644))
	return writeConfig(t, reposDir, name,
		`,"allowed_signers":"`+signers+`","roles":{"SHA256:`+strings.Repeat("A", 43)+`":"implementer"}`+extra)
}

func listDir(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	return names
}

func TestInitRepoFailedFetchLeavesNoRepo(t *testing.T) {
	requireGit(t)
	root := t.TempDir()
	cfg := filepath.Join(root, "repos.d", "r.json")
	bare := filepath.Join(root, "srv", "r.git")
	missing := filepath.Join(root, "no-such-upstream.git")
	if rc := initRepoRun(bare, "main", missing, cfg); rc == 0 {
		t.Fatal("init-repo with an unreachable upstream succeeded")
	}
	if _, err := os.Lstat(bare); !os.IsNotExist(err) {
		t.Fatalf("a failed provisioning left %s behind (err=%v)", bare, err)
	}
	for _, n := range listDir(t, filepath.Dir(bare)) {
		if strings.HasSuffix(n, ".git") {
			t.Fatalf("a failed provisioning left a servable path %q", n)
		}
		if strings.HasSuffix(n, ".provisioning") {
			t.Fatalf("a failed provisioning left its temporary path %q", n)
		}
	}
}

func TestInitRepoRemovesLeftoverAndSucceeds(t *testing.T) {
	requireGit(t)
	root := t.TempDir()
	cfg := filepath.Join(root, "repos.d", "r.json")
	bare := filepath.Join(root, "srv", "r.git")
	leftover := provisioningPath(bare)
	if err := os.MkdirAll(filepath.Join(leftover, "junk"), 0o755); err != nil {
		t.Fatal(err)
	}
	if rc := initRepoRun(bare, "main", "", cfg); rc != 0 {
		t.Fatalf("init-repo = %d, want 0", rc)
	}
	if _, err := os.Lstat(leftover); !os.IsNotExist(err) {
		t.Fatalf("leftover %s still present (err=%v)", leftover, err)
	}
	if ok, reason, err := repoProvisioned(bare); err != nil || !ok {
		t.Fatalf("repo not provisioned after init-repo: ok=%v reason=%q err=%v", ok, reason, err)
	}
	if strings.HasSuffix(leftover, ".git") || allowedRepoPath(leftover) {
		t.Fatalf("provisioning path %q would be servable", leftover)
	}
}

func TestInitRepoRefusesExistingUnprovisionedTarget(t *testing.T) {
	requireGit(t)
	root := t.TempDir()
	cfg := filepath.Join(root, "repos.d", "r.json")
	bare := filepath.Join(root, "srv", "r.git")
	mustRun(t, "git", "init", "-q", "--bare", bare)
	keep := filepath.Join(bare, "refs", "heads", "accepted-branch")
	if err := os.WriteFile(keep, []byte("0000000000000000000000000000000000000000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if rc := initRepoRun(bare, "main", "", cfg); rc == 0 {
		t.Fatal("init-repo overwrote an existing unprovisioned repo")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("existing repo content was touched: %v", err)
	}
	if _, err := os.Stat(filepath.Join(bare, "hooks", "pre-receive")); err == nil {
		t.Fatal("init-repo baked hooks into a repo it refused")
	}
}

func TestInitRepoNamesRemoteFromConfig(t *testing.T) {
	requireGit(t)
	root := t.TempDir()
	upstream, _, _ := newSeededUpstreamAndMirror(t, root)
	cfg := writeValidConfig(t, root, filepath.Join(root, "repos.d"), "r", `,"upstream_remote":"origin-gh"`)
	bare := filepath.Join(root, "srv", "r.git")
	if rc := initRepoRun(bare, "main", upstream, cfg); rc != 0 {
		t.Fatalf("init-repo = %d, want 0", rc)
	}
	if got := gitOut(t, bare, "config", "--get", "remote.origin-gh.url"); got != upstream {
		t.Fatalf("remote.origin-gh.url = %q, want %q", got, upstream)
	}
	if out, err := exec.Command("git", "-C", bare, "config", "--get", "remote.upstream.url").CombinedOutput(); err == nil {
		t.Fatalf("an `upstream` remote exists too: %s", out)
	}
	if got, want := gitOut(t, bare, "rev-parse", "main"), gitOut(t, upstream, "rev-parse", "main"); got != want {
		t.Fatalf("default branch seeded at %s, want upstream's %s", got, want)
	}
}

// shellFixture is a provisioned repo under a repo root, with git's pack
// commands stubbed to record that they ran.
func shellFixture(t *testing.T) (bare, ranMarker string) {
	t.Helper()
	requireGit(t)
	root := t.TempDir()
	reposDir := filepath.Join(root, "repos.d")
	t.Setenv("PORTITOR_REPOS_DIR", reposDir)
	t.Setenv("PORTITOR_REPO_ROOT", root)
	bare = filepath.Join(root, "r.git")
	if rc := initRepoRun(bare, "main", "", filepath.Join(reposDir, "r.json")); rc != 0 {
		t.Fatalf("init-repo = %d", rc)
	}
	writeConfig(t, reposDir, "r", "")
	binDir := t.TempDir()
	ranMarker = filepath.Join(t.TempDir(), "ran")
	for _, name := range []string{"git-receive-pack", "git-upload-pack"} {
		body := "#!/bin/sh\ntouch '" + ranMarker + "'\n"
		if err := os.WriteFile(filepath.Join(binDir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return bare, ranMarker
}

func TestShellServesProvisionedRepoWithoutUpstream(t *testing.T) {
	bare, ran := shellFixture(t)
	for _, cmd := range []string{"git-receive-pack", "git-upload-pack"} {
		_ = os.Remove(ran)
		t.Setenv("SSH_ORIGINAL_COMMAND", cmd+" '"+bare+"'")
		if rc := shellCommand([]string{"testfp"}); rc != 0 {
			t.Fatalf("%s on a provisioned repo with no upstream = %d, want 0", cmd, rc)
		}
		if _, err := os.Stat(ran); err != nil {
			t.Fatalf("%s did not run", cmd)
		}
	}
}

func TestShellRefusesUnprovisionedRepo(t *testing.T) {
	cases := map[string]func(t *testing.T, bare string){
		"pre-receive missing":  func(t *testing.T, bare string) { must(t, os.Remove(filepath.Join(bare, "hooks", "pre-receive"))) },
		"post-receive missing": func(t *testing.T, bare string) { must(t, os.Remove(filepath.Join(bare, "hooks", "post-receive"))) },
		"foreign pre-receive": func(t *testing.T, bare string) {
			must(t, os.WriteFile(filepath.Join(bare, "hooks", "pre-receive"), []byte("#!/bin/sh\nexit 0\n"), 0o755))
		},
		"core.hooksPath set": func(t *testing.T, bare string) {
			mustRun(t, "git", "-C", bare, "config", "core.hooksPath", t.TempDir())
		},
	}
	for name, breakIt := range cases {
		t.Run(name, func(t *testing.T) {
			bare, ran := shellFixture(t)
			breakIt(t, bare)
			for _, cmd := range []string{"git-receive-pack", "git-upload-pack"} {
				t.Setenv("SSH_ORIGINAL_COMMAND", cmd+" '"+bare+"'")
				if rc := shellCommand([]string{"testfp"}); rc != 1 {
					t.Fatalf("%s = %d, want 1", cmd, rc)
				}
				if _, err := os.Stat(ran); err == nil {
					t.Fatalf("%s ran on an unprovisioned repo", cmd)
				}
			}
		})
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestAddRepoEnsureProvisionsFromConfigOnce(t *testing.T) {
	requireGit(t)
	root := t.TempDir()
	reposDir := filepath.Join(root, "repos.d")
	t.Setenv("PORTITOR_REPOS_DIR", reposDir)
	t.Setenv("PORTITOR_REPO_ROOT", filepath.Join(root, "srv"))
	writeValidConfig(t, root, reposDir, "r", "")
	cfgPath := filepath.Join(reposDir, "r.json")
	b, err := os.ReadFile(cfgPath)
	must(t, err)
	must(t, os.WriteFile(cfgPath, []byte(strings.Replace(string(b), `"default_branch":"main"`, `"default_branch":"trunk"`, 1)), 0o644))
	if rc := addRepoRun("r", "", "", true); rc != 0 {
		t.Fatalf("add-repo --ensure = %d, want 0", rc)
	}
	bare := filepath.Join(root, "srv", "r.git")
	if ok, reason, err := repoProvisioned(bare); err != nil || !ok {
		t.Fatalf("not provisioned: %q %v", reason, err)
	}
	if head := gitOut(t, bare, "symbolic-ref", "HEAD"); head != "refs/heads/trunk" {
		t.Fatalf("HEAD = %s, want the config's default branch trunk", head)
	}
	if out, err := exec.Command("git", "-C", bare, "remote").CombinedOutput(); err != nil || strings.TrimSpace(string(out)) != "" {
		t.Fatalf("a config without upstream_slug got remotes %q (err %v)", out, err)
	}
	if rc := addRepoRun("r", "", "", true); rc != 0 {
		t.Fatalf("second add-repo --ensure on a provisioned repo = %d, want 0", rc)
	}
}

func TestAddRepoEnsureRefusesUnprovisionedLeftover(t *testing.T) {
	requireGit(t)
	root := t.TempDir()
	reposDir := filepath.Join(root, "repos.d")
	t.Setenv("PORTITOR_REPOS_DIR", reposDir)
	t.Setenv("PORTITOR_REPO_ROOT", filepath.Join(root, "srv"))
	writeConfig(t, reposDir, "r", "")
	mustRun(t, "git", "init", "-q", "--bare", filepath.Join(root, "srv", "r.git"))
	if rc := addRepoRun("r", "", "", true); rc != 1 {
		t.Fatalf("add-repo --ensure over a hookless repo = %d, want 1", rc)
	}
}

func TestAddRepoEnsureSucceedsWhenARacingProvisioningWins(t *testing.T) {
	requireGit(t)
	root := t.TempDir()
	reposDir := filepath.Join(root, "repos.d")
	t.Setenv("PORTITOR_REPOS_DIR", reposDir)
	t.Setenv("PORTITOR_REPO_ROOT", filepath.Join(root, "srv"))
	cfg := writeValidConfig(t, root, reposDir, "r", "")
	bare := filepath.Join(root, "srv", "r.git")
	release, err := acquireProvisionLock(bare)
	must(t, err)
	done := make(chan int, 1)
	go func() { done <- addRepoRun("r", "", "", true) }()
	// The other provisioning, holding the lock, completes first.
	if rc := buildRepo(provisioningPath(bare), "main", "", "upstream", cfg); rc != 0 {
		t.Fatalf("buildRepo = %d", rc)
	}
	must(t, os.Rename(provisioningPath(bare), bare))
	release()
	if rc := <-done; rc != 0 {
		t.Fatalf("add-repo --ensure after a racing provisioning won = %d, want 0", rc)
	}
}
