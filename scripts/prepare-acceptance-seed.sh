#!/bin/sh
# Prepare the acceptance suite's disposable repo, once per seed change (see
# spec/delivery/arch_ci.md, "The acceptance tier"): in <clone>, make main a
# single parentless commit whose tree is exactly cmd/portitor/testdata/acceptance-seed
# as committed at portitor's HEAD, and tag it with testdata/acceptance-seed.tag.
# Pushing stays with the operator: the seed carries a workflow file, which the
# suite's own PAT cannot push.
set -eu
[ $# -eq 1 ] || { echo "usage: $0 <clone of the disposable repo>" >&2; exit 2; }
clone=$1
root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
tag=$(git -C "$root" show HEAD:cmd/portitor/testdata/acceptance-seed.tag)
top=$(git -C "$clone" rev-parse --show-toplevel)
if [ "$(CDPATH= cd -- "$top" && pwd -P)" = "$(CDPATH= cd -- "$root" && pwd -P)" ]; then
	echo "$0: $clone is portitor itself; give it a clone of the disposable repo" >&2
	exit 2
fi
if [ -n "$(git -C "$clone" status --porcelain --ignored)" ]; then
	echo "$0: $clone has local changes or untracked files; use a fresh clone of the disposable repo" >&2
	exit 1
fi
if git -C "$clone" rev-parse -q --verify "refs/tags/$tag" >/dev/null; then
	echo "$0: tag $tag already exists in $clone; a changed seed needs a new name in testdata/acceptance-seed.tag" >&2
	exit 1
fi
work="acceptance-seed-$$"
git -C "$clone" checkout -q --orphan "$work"
git -C "$clone" rm -rqf --cached --ignore-unmatch .
find "$clone" -mindepth 1 -maxdepth 1 ! -name .git -exec rm -rf {} +
# From the commit, not the working tree: an uncommitted edit or a stray ignored
# file in the fixture dir must never reach the seed.
git -C "$root" archive HEAD:cmd/portitor/testdata/acceptance-seed | tar -x -C "$clone"
git -C "$clone" add -A -f
git -C "$clone" commit -q -m "acceptance seed $tag"
git -C "$clone" branch -M "$work" main
git -C "$clone" tag "$tag"
git -C "$clone" ls-tree -r --name-only main
cat <<NEXT
main is now one commit holding exactly the seed, tagged $tag. Push it, and drop superseded seed tags:
  git -C $clone push --force origin main $tag
  git -C $clone push origin --delete <each older gateaccept-seed-* tag>
NEXT
