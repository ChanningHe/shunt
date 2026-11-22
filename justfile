formula := "Formula/shunt.rb"
repo := "https://github.com/ChanningHe/shunt.git"

# list the recipes
default:
    @just --list

# gofmt, go vet, go test -race
check:
    .claude/check

# tag HEAD as vVERSION and point the Homebrew formula at it; nothing is pushed
release version:
    #!/usr/bin/env bash
    set -euo pipefail
    v={{ version }}
    tag="v$v"
    [[ $v =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo "version must look like 1.2.3" >&2; exit 1; }
    [[ $(git branch --show-current) == main ]] || { echo "not on main" >&2; exit 1; }
    [[ -z $(git status --porcelain) ]] || { echo "the working tree is not clean" >&2; exit 1; }
    git rev-parse -q --verify "refs/tags/$tag" >/dev/null && { echo "tag $tag exists" >&2; exit 1; }
    .claude/check
    rev=$(git rev-parse HEAD)
    git tag -a "$tag" -m "$tag"
    awk -v repo={{ repo }} -v tag="$tag" -v rev="$rev" '
      /^  url / { skip = 1 }
      skip { if (/revision:/) skip = 0; next }
      { print }
      /^  homepage / {
        printf "  url \"%s\",\n      tag:      \"%s\",\n      revision: \"%s\"\n", repo, tag, rev
      }' {{ formula }} > {{ formula }}.new
    mv {{ formula }}.new {{ formula }}
    git add {{ formula }}
    git commit -q -m "chore(release): $tag"
    echo "tagged $tag at ${rev:0:7}; publish with:"
    echo "  git push origin main $tag"
