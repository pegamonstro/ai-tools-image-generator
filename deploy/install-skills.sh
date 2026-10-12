#!/bin/bash
# Install img-gen's packaged agent skills and opencode commands into the
# current user's toolboxes. Idempotent: re-running refreshes the copies.
set -euo pipefail

repo_dir="$(cd "$(dirname "$0")/.." && pwd)"

claude_skills="${CLAUDE_SKILLS_DIR:-$HOME/.claude/skills}"
opencode_cmds="${OPENCODE_CMDS_DIR:-$HOME/.opencode/commands}"

mkdir -p "$claude_skills" "$opencode_cmds"

for skill_dir in "$repo_dir"/skills/*/; do
  [ -e "$skill_dir" ] || continue
  name="$(basename "$skill_dir")"
  rm -rf "$claude_skills/$name"
  cp -R "$skill_dir" "$claude_skills/$name"
  echo "installed skill $name -> $claude_skills/$name"
done

for cmd in "$repo_dir"/.opencode/commands/*.md; do
  [ -e "$cmd" ] || continue
  cp "$cmd" "$opencode_cmds/"
  echo "installed command $(basename "$cmd") -> $opencode_cmds/"
done