#!/usr/bin/env bash
# Build, and on request post, the release announcement for a cyoda-go release:
# a GitHub Discussion in the Announcements category, titled
# "Cyoda-go vX.Y.Z released", whose body is docs/release-notes/vX-Y-Z.md.
#
# GitHub Discussions render a single line break as a hard break, so the body
# joins each wrapped paragraph and list item onto one line. Headings, tables,
# blockquotes and fenced code are kept line for line. As in Markdown, a line
# that starts a block (`- `, `1. `, `> `, `|`, a heading `# `) is not joined to
# the line before it: do not begin a wrapped prose line with one.
#
# Usage:
#   scripts/release-announcement.sh vX.Y.Z            # print the body (dry run)
#   scripts/release-announcement.sh vX.Y.Z --post     # create the discussion
#   scripts/release-announcement.sh --notes FILE [--print-title]
#
# --post refuses when a discussion with the same title exists. Discussions have
# no REST API, so posting uses `gh api graphql`.
#
# Compatible with bash 3.2+ (macOS system bash).
set -euo pipefail

REPO_OWNER="cyoda"
REPO_NAME="cyoda-go"
CATEGORY_SLUG="announcements"

version=""
notes=""
post=false
print_title=false
while [ $# -gt 0 ]; do
	case "$1" in
	--post) post=true ;;
	--print-title) print_title=true ;;
	--notes)
		shift
		notes="${1:?--notes needs a file}"
		;;
	v*) version="$1" ;;
	*)
		echo "usage: $0 vX.Y.Z [--post] | --notes FILE [--print-title]" >&2
		exit 2
		;;
	esac
	shift
done

if [ -z "$notes" ]; then
	[ -n "$version" ] || {
		echo "usage: $0 vX.Y.Z [--post] | --notes FILE [--print-title]" >&2
		exit 2
	}
	notes="$(git rev-parse --show-toplevel)/docs/release-notes/$(echo "${version}" | tr . -).md"
fi
[ -f "$notes" ] || {
	echo "ERROR: release notes not found: $notes" >&2
	exit 1
}

# The version comes from the notes' first heading: "# Cyoda-Go vX.Y.Z".
heading_version="$(sed -n '1s/^# Cyoda-Go \(v[0-9][0-9.]*\)$/\1/p' "$notes")"
[ -n "$heading_version" ] || {
	echo "ERROR: the first line of $notes is not '# Cyoda-Go vX.Y.Z'" >&2
	exit 1
}
if [ -n "$version" ] && [ "$version" != "$heading_version" ]; then
	echo "ERROR: $notes is for $heading_version, not $version" >&2
	exit 1
fi
title="Cyoda-go ${heading_version} released"

if $print_title; then
	echo "$title"
	exit 0
fi

body="$(python3 -I -c '
import re, sys
item = re.compile(r"^\s*([-*+]|\d+\.)\s")
heading = re.compile(r"^#{1,6}(\s|$)")
out, fence = [], False
for line in open(sys.argv[1], encoding="utf-8").read().split("\n"):
    s = line.strip()
    if s.startswith("```"):
        fence = not fence
        out.append(line)
        continue
    keep = fence or s == "" or heading.match(s) or s.startswith(("|", ">")) or item.match(line)
    prev = out[-1].strip() if out else ""
    if keep or not out or prev == "" or heading.match(prev) or prev.startswith(("|", ">", "```")):
        out.append(line)
    else:
        out[-1] = out[-1].rstrip() + " " + s
sys.stdout.write("\n".join(out))
' "$notes")"

if ! $post; then
	printf '%s\n' "$body"
	exit 0
fi

existing="$(gh api graphql -f query='query($o:String!,$n:String!){ repository(owner:$o, name:$n){ discussions(first:50, orderBy:{field:CREATED_AT, direction:DESC}){ nodes { title url } } } }' \
	-f o="$REPO_OWNER" -f n="$REPO_NAME" \
	-q ".data.repository.discussions.nodes[] | select(.title == \"$title\") | .url")"
if [ -n "$existing" ]; then
	echo "ERROR: '$title' is already posted: $existing" >&2
	exit 1
fi

ids="$(gh api graphql -f query='query($o:String!,$n:String!){ repository(owner:$o, name:$n){ id discussionCategories(first:25){ nodes { id slug } } } }' \
	-f o="$REPO_OWNER" -f n="$REPO_NAME" \
	-q "[.data.repository.id, (.data.repository.discussionCategories.nodes[] | select(.slug == \"$CATEGORY_SLUG\") | .id)] | @tsv")"
repo_id="$(printf '%s' "$ids" | cut -f1)"
category_id="$(printf '%s' "$ids" | cut -s -f2)"
[ -n "$category_id" ] || {
	echo "ERROR: no '$CATEGORY_SLUG' discussion category in $REPO_OWNER/$REPO_NAME" >&2
	exit 1
}

body_file="$(mktemp)"
trap 'rm -f "$body_file"' EXIT
printf '%s\n' "$body" >"$body_file"

gh api graphql -f query='mutation($r:ID!,$c:ID!,$t:String!,$b:String!){ createDiscussion(input:{repositoryId:$r, categoryId:$c, title:$t, body:$b}){ discussion { url } } }' \
	-f r="$repo_id" -f c="$category_id" -f t="$title" -F b=@"$body_file" \
	-q '.data.createDiscussion.discussion.url'
