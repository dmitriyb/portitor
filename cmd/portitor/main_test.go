package main

import (
	"reflect"
	"testing"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		orig string
		kind string
		rest []string
		ok   bool
	}{
		{"git-receive-pack '/srv/git/repo.git'", "git", []string{"git-receive-pack", "/srv/git/repo.git"}, true},
		{"git-upload-pack '/srv/git/repo.git'", "git", []string{"git-upload-pack", "/srv/git/repo.git"}, true},
		{"portitor pr comment --pr 5", "pr", []string{"comment", "--pr", "5"}, true},
		{"portitor pr fetch --pr 7", "pr", []string{"fetch", "--pr", "7"}, true},
		{"portitor shell deadbeef", "reject", nil, false},
		{"rm -rf /", "reject", nil, false},
		{"git-receive-pack a b", "reject", nil, false},
		{"", "reject", nil, false},
	}
	for _, c := range cases {
		kind, rest, err := classify(c.orig)
		if kind != c.kind {
			t.Errorf("classify(%q) kind=%q want %q", c.orig, kind, c.kind)
		}
		if c.ok && err != nil {
			t.Errorf("classify(%q) unexpected err %v", c.orig, err)
		}
		if !c.ok && err == nil {
			t.Errorf("classify(%q) expected err", c.orig)
		}
		if c.rest != nil && !reflect.DeepEqual(rest, c.rest) {
			t.Errorf("classify(%q) rest=%v want %v", c.orig, rest, c.rest)
		}
	}
}

func TestRoleCan(t *testing.T) {
	allow := map[string][]string{
		"comment": {"implementer", "fixer", "reviewer", "merger", "owner"},
		"fetch":   {"implementer", "fixer", "reviewer", "merger", "owner"},
		"review":  {"reviewer", "owner"},
		"merge":   {"merger", "owner"},
		"close":   {"merger", "owner"},
	}
	allRoles := []string{"implementer", "fixer", "reviewer", "merger", "owner", "", "bogus"}
	for act, allowed := range allow {
		for _, role := range allRoles {
			want := contains(allowed, role)
			if got := roleCan(role, act); got != want {
				t.Errorf("roleCan(%q,%q)=%v want %v", role, act, got, want)
			}
		}
	}
	// implementer must NOT review/merge/close (the teeth of the model)
	for _, act := range []string{"review", "merge", "close"} {
		if roleCan("implementer", act) {
			t.Errorf("implementer should not be able to %q", act)
		}
	}
	if roleCan("anything", "unknown-action") {
		t.Error("unknown action must be denied")
	}
}

func TestShellSplit(t *testing.T) {
	got, err := shellSplit("git-receive-pack '/srv/git/my repo.git'")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"git-receive-pack", "/srv/git/my repo.git"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	if _, err := shellSplit("unterminated 'quote"); err == nil {
		t.Fatal("expected unterminated-quote error")
	}
}

func TestDeriveSlug(t *testing.T) {
	for url, want := range map[string]string{
		"git@github.com:dmitriyb/portitor.git":          "dmitriyb/portitor",
		"git@github.com-personal:dmitriyb/portitor.git": "dmitriyb/portitor",
		"https://github.com/dmitriyb/portitor.git":      "dmitriyb/portitor",
		"https://github.com/dmitriyb/portitor":          "dmitriyb/portitor",
	} {
		if got := deriveSlug(url); got != want {
			t.Errorf("deriveSlug(%q)=%q want %q", url, got, want)
		}
	}
}

func TestAllowedRepoPath(t *testing.T) {
	t.Setenv("PORTITOR_REPO_ROOT", "/srv/git")
	ok := []string{"/srv/git/repo.git", "/srv/git/team/repo.git"}
	bad := []string{"/etc/passwd", "/srv/git/../../etc/x.git", "/srv/git/repo", "/other/repo.git"}
	for _, p := range ok {
		if !allowedRepoPath(p) {
			t.Errorf("allowedRepoPath(%q) = false, want true", p)
		}
	}
	for _, p := range bad {
		if allowedRepoPath(p) {
			t.Errorf("allowedRepoPath(%q) = true, want false", p)
		}
	}
}

// contains is a tiny test helper (mirrors gate.contains).
func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
