#!/usr/bin/env bash
# Test harness for scripts/release-announcement.sh.
# Feeds a release-notes fixture through the script's dry run (the default,
# which prints the announcement body) and compares it with the expected body:
# wrapped paragraphs and list items joined onto one line each; headings,
# tables, blockquotes and fenced code kept line for line.

set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel)"
SCRIPT="$REPO_ROOT/scripts/release-announcement.sh"

[[ -x "$SCRIPT" ]] || { echo "FAIL: $SCRIPT is missing or not executable"; exit 1; }

scratch=$(mktemp -d)
trap 'rm -rf "$scratch"' EXIT

cat > "$scratch/notes.md" <<'EOF'
# Cyoda-Go v9.9.9

A one-line tagline
that wraps.

_Released 1 January 2030 · 2 issues delivered_

## ✨ Highlights

- **First.** A list item that
  wraps onto a second line.
- **Second.** One line.
  - A nested item
    that wraps.
- **Third.** An item
whose lazy continuation has no indent.
1. A numbered item
   that wraps.

A paragraph that names
#737 at the start of a line.

#738 starts this paragraph,
which wraps.

| A | B |
|---|---|
| 1 | 2 |

> A quote
> on two lines.

```bash
echo keep
   this indentation
```

Last paragraph
ends here.
EOF

cat > "$scratch/want.md" <<'EOF'
# Cyoda-Go v9.9.9

A one-line tagline that wraps.

_Released 1 January 2030 · 2 issues delivered_

## ✨ Highlights

- **First.** A list item that wraps onto a second line.
- **Second.** One line.
  - A nested item that wraps.
- **Third.** An item whose lazy continuation has no indent.
1. A numbered item that wraps.

A paragraph that names #737 at the start of a line.

#738 starts this paragraph, which wraps.

| A | B |
|---|---|
| 1 | 2 |

> A quote
> on two lines.

```bash
echo keep
   this indentation
```

Last paragraph ends here.
EOF

"$SCRIPT" --notes "$scratch/notes.md" > "$scratch/got.md"

if ! diff -u "$scratch/want.md" "$scratch/got.md"; then
	echo "FAIL: the announcement body differs from the expected unwrapped notes"
	exit 1
fi

# The title is derived from the version in the first heading.
title="$("$SCRIPT" --notes "$scratch/notes.md" --print-title)"
if [[ "$title" != "Cyoda-go v9.9.9 released" ]]; then
	echo "FAIL: title = '$title', want 'Cyoda-go v9.9.9 released'"
	exit 1
fi

# A version that does not match the notes' heading is refused.
if "$SCRIPT" v1.0.0 --notes "$scratch/notes.md" >/dev/null 2>&1; then
	echo "FAIL: a version that does not match the notes' heading was accepted"
	exit 1
fi

# Missing notes are refused.
if "$SCRIPT" --notes "$scratch/absent.md" >/dev/null 2>&1; then
	echo "FAIL: a missing notes file was accepted"
	exit 1
fi

echo "PASS: release-announcement"
