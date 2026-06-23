package gate

import "testing"

// TestAncestry verifies the opt-in "branch must be based on current default" check.
func TestAncestry(t *testing.T) {
	requireBins(t, "git", "ssh-keygen")
	e := newTestEnv(t)

	base := e.commitFile("README.md", "base")
	e.push("main")

	// feature forks from base
	e.checkout("-b", "feature")
	feat := e.commitFile("a.txt", "a")
	e.push("feature")

	// default moves on (base -> main2)
	e.checkout("main")
	main2 := e.commitFile("m.txt", "m")
	e.push("main")

	// feature2 forks from the current default (main2)
	e.checkout("-b", "feature2")
	feat2 := e.commitFile("b.txt", "b")
	e.push("feature2")

	strict := Config{AllowedSigners: e.allowedSigners, RequireUpToDateWithDefault: true}
	loose := Config{AllowedSigners: e.allowedSigners}

	t.Run("stale base rejected", func(t *testing.T) {
		vs, err := Check(e.bare, []RefUpdate{{OldSHA: base, NewSHA: feat, Ref: "refs/heads/feature"}}, strict)
		if err != nil {
			t.Fatal(err)
		}
		assertRules(t, vs, []string{"stale-base"})
	})

	t.Run("ancestry not enforced when not required", func(t *testing.T) {
		vs, err := Check(e.bare, []RefUpdate{{OldSHA: base, NewSHA: feat, Ref: "refs/heads/feature"}}, loose)
		if err != nil {
			t.Fatal(err)
		}
		assertRules(t, vs, nil)
	})

	t.Run("up-to-date base accepted", func(t *testing.T) {
		vs, err := Check(e.bare, []RefUpdate{{OldSHA: main2, NewSHA: feat2, Ref: "refs/heads/feature2"}}, strict)
		if err != nil {
			t.Fatal(err)
		}
		assertRules(t, vs, nil)
	})
}
