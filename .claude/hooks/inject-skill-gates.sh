#!/bin/bash
# PreToolUse(Skill): inject cyoda-go's per-skill gates when a planning skill runs.
# Compact, project-specific directives the generic superpowers skills don't carry.
# No-op (tool proceeds) for any other skill or on any error.
set -euo pipefail
skill=$(cat | jq -r '.tool_input.skill // ""')
case "$skill" in
  superpowers:brainstorming) f=gate-brainstorming.md ;;
  superpowers:writing-plans) f=gate-writing-plans.md ;;
  *) exit 0 ;;
esac
ctx=$(cat "${CLAUDE_PROJECT_DIR}/.claude/rules/$f" 2>/dev/null) || exit 0
jq -n --arg c "$ctx" '{hookSpecificOutput:{hookEventName:"PreToolUse",additionalContext:$c,permissionDecision:"allow"}}'
