#!/usr/bin/env bash
# Exercises the Makefile version derivation in scratch repositories.
#
# A fake go records the linker flags so no compilation is needed. Builds must
# carry the newest stable MAJOR.MINOR.PATCH release reachable from HEAD, and
# must fail instead of falling back to the commit when there is none.

set -euo pipefail

makefile="$(cd "$(dirname "$0")/../.." && pwd)/Makefile"

dir=$(mktemp -d)
cleanup() { chmod -R u+w "$dir" && rm -r -- "$dir"; }
trap cleanup EXIT

# isolate from the surrounding repository, user config and build environment
export GIT_CEILING_DIRECTORIES="$dir" GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1
unset TAG_NAME RELEASE SHA MAKEFLAGS

mkdir "$dir/bin"
cat >"$dir/bin/go" <<'EOF'
#!/bin/sh
case $1 in
env) echo /nonexistent/goroot ;;
build) printf '%s\n' "$@" >"$GO_ARGS" ;;
esac
EOF
chmod +x "$dir/bin/go"
export PATH="$dir/bin:$PATH" GO_ARGS="$dir/go-args"

failed=0
fail() {
	echo "FAIL: $*" >&2
	failed=1
}

run_make() { # run_make <dir> <make args...>
	local repo=$1
	shift
	make --no-print-directory -s -C "$repo" -f "$makefile" "$@" 2>"$dir/stderr"
}

commit() { git -C "$1" -c user.email=t@t -c user.name=t commit --quiet --allow-empty -m "$2"; }
sha() { git -C "$1" rev-parse --short HEAD; }

expect_build() { # expect_build <name> <target> <dir> <version> <commit> [make args...]
	local name=$1 target=$2 repo=$3 version=$4 commit=$5
	shift 5
	local want="-ldflags=-X github.com/evcc-io/evcc/util.Version=$version -X github.com/evcc-io/evcc/util.Commit=$commit -s -w"

	: >"$GO_ARGS"
	if ! run_make "$repo" "$target" "$@" >/dev/null; then
		fail "$name: make $target failed: $(cat "$dir/stderr")"
		return
	fi

	local got
	got=$(grep -e '^-ldflags=' "$GO_ARGS" || true)
	[[ $got == "$want" ]] || fail "$name: make $target got '$got', want '$want'"
}

expect_gok() { # expect_gok <name> <dir> <version> <commit>
	local want="-X github.com/evcc-io/evcc/util.Version=$3 -X github.com/evcc-io/evcc/util.Commit=$4 -s -w"
	local got
	if ! got=$(run_make "$2" -n gok); then
		fail "$1: make -n gok failed: $(cat "$dir/stderr")"
	elif [[ $got != *"$want"* ]]; then
		fail "$1: make -n gok does not pass '$want'"
	fi
}

expect_version() { # expect_version <name> <dir> <version>
	local got
	got=$(run_make "$2" version) || got="FAIL: $(cat "$dir/stderr")"
	[[ $got == "$3" ]] || fail "$1: make version got '$got', want '$3'"
}

expect_no_version() { # expect_no_version <name> <dir> <target>
	: >"$GO_ARGS"
	if run_make "$2" "$3" >/dev/null; then
		fail "$1: make $3 succeeded without a release tag"
	elif ! grep -q 'no stable release tag' "$dir/stderr"; then
		fail "$1: make $3 failed without a clear error: $(cat "$dir/stderr")"
	fi
	[[ ! -s $GO_ARGS ]] || fail "$1: make $3 invoked go build"
}

expect_dry_run() { # expect_dry_run <name> <dir> <target>
	run_make "$2" -n "$3" >/dev/null || fail "$1: make -n $3 failed: $(cat "$dir/stderr")"
}

repo="$dir/repo"
git init --quiet --initial-branch=master "$repo"
commit "$repo" old
git -C "$repo" tag 0.315.0
commit "$repo" release
git -C "$repo" tag 0.316.0

expect_build "exact release" build "$repo" 0.316.0 "$(sha "$repo")"
expect_build "exact release" soc "$repo" 0.316.0 "$(sha "$repo")"
expect_gok "exact release" "$repo" 0.316.0 "$(sha "$repo")"
expect_version "exact release" "$repo" 0.316.0

git -C "$repo" checkout --quiet -b fork
commit "$repo" fork-1
commit "$repo" fork-2

expect_build "commits above release" build "$repo" 0.316.0 "$(sha "$repo")"
expect_build "release hides commit" build "$repo" 0.316.0 "" RELEASE=1
expect_build "explicit tag" build "$repo" 1.2.3 "$(sha "$repo")" TAG_NAME=1.2.3

# releases from other lines and tags that are no stable release
git -C "$repo" checkout --quiet -b side 0.316.0
commit "$repo" side
git -C "$repo" tag 0.316.1
git -C "$repo" tag 0.400.0
git -C "$repo" checkout --quiet fork
for tag in 0.399.0-rc1 0.399.0-fork.1 v0.399.0 0.399 0.399.0.1 9.9.9-fork; do
	git -C "$repo" tag "$tag"
done

expect_build "unrelated and non-release tags ignored" build "$repo" 0.316.0 "$(sha "$repo")"

# upstream releases again, the fork rebases on top of it
git -C "$repo" checkout --quiet master
commit "$repo" upstream
git -C "$repo" tag 0.317.0
git -C "$repo" checkout --quiet fork
git -C "$repo" -c user.email=t@t -c user.name=t rebase --quiet master

expect_build "rebased onto new release" build "$repo" 0.317.0 "$(sha "$repo")"
expect_version "rebased onto new release" "$repo" 0.317.0

git -C "$repo" worktree add --quiet --detach "$dir/worktree" fork
expect_build "worktree" build "$dir/worktree" 0.317.0 "$(sha "$repo")"

notags="$dir/notags"
git init --quiet "$notags"
commit "$notags" untagged

nogit="$dir/nogit"
mkdir "$nogit"

for target in build soc gok version; do
	expect_no_version "no tags" "$notags" "$target"
	expect_no_version "no git" "$nogit" "$target"
done
expect_build "explicit tag without tags" build "$notags" 1.2.3 "$(sha "$notags")" TAG_NAME=1.2.3

# docker runs these before the repository is copied into the build context
for target in install ui assets test patch-asn1; do
	expect_dry_run "no tags" "$notags" "$target"
	expect_dry_run "no git" "$nogit" "$target"
done

[[ $failed == 0 ]] && echo "version test ok"
exit $failed
