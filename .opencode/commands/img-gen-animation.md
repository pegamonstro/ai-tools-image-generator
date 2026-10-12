---
description: Build an animated GIF with img-gen (from frames, or generate a frame sequence)
---
Follow the img-gen-animation skill ($HOME/.claude/skills/img-gen-animation/SKILL.md,
or the packaged skills/ directory of the img-gen repo). Ask me for the frame
source (generation spec or existing frames), timing, and destination. Then
submit via img-gen (make_gif MCP tool or POST /api/v1/gifs), fetch the result
with read_export, and report the manifest.