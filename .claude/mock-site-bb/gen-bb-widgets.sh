#!/bin/bash
# BIKINI CLUB — submit + download ONLY bb-widgets (bb-elements/bb-crew/bb-board already in flight)
set -u
BASE=http://127.0.0.1:9106
SITE=/Users/archcore/Projects/img-gen/.claude/worktrees/asset-expansion/.claude/mock-site-bb
A=$SITE/assets
STYLE="cyberpunk underground hacking collective, dark metal with phosphor green glow, amber BBS accents, magenta and cyan neon, hand painted illustration"
NEG="photograph, blurry, watermark"

R=$(curl -s -X POST "$BASE/api/v1/asset-sets" -H 'Content-Type: application/json' -d '{
 "name":"bb-widgets","seed":5111,"palette_lock":true,"wait_seconds":0,"assets":[
 {"kind":"bar","name":"signal","prompt":"phosphor green signal strength meter in dark metal chassis, '"$STYLE"'","negative":"'"$NEG"'","size":"224x32","frame_margins":6,"bar_fill":"flowing phosphor green energy","fill_states":[20,40,60,80,100]},
 {"kind":"bar","name":"load","prompt":"magenta and cyan cpu load meter in dark metal chassis, '"$STYLE"'","negative":"'"$NEG"'","size":"224x32","frame_margins":6,"bar_fill":"crackling magenta and cyan electric plasma","fill_states":[20,40,60,80,100]},
 {"kind":"atlas","name":"leds","prompt":"status indicator LED lens: green ok amber warn magenta busy, '"$STYLE"'","negative":"'"$NEG"'","size":"32x32","transparent":true,"frames":3},
 {"kind":"atlas","name":"chips","prompt":"component icon: integrated circuit chip, capacitor, resistor, crystal diode, dark metal with neon trim, '"$STYLE"'","negative":"'"$NEG"'","size":"64x64","transparent":true,"frames":4},
 {"kind":"element","name":"glitch","prompt":"datamosh glitch divider strip torn pixels magenta cyan, '"$STYLE"'","negative":"'"$NEG"'","size":"384x32","transparent":true},
 {"kind":"element","name":"hexfield","prompt":"wall of terminal hex dump with scattered green glyphs, '"$STYLE"'","negative":"'"$NEG"'","size":"512x128"},
 {"kind":"element","name":"scan","prompt":"CRT scanline overlay pattern, faint phosphor glow rows, '"$STYLE"'","negative":"'"$NEG"'","size":"256x256","transparent":true}
]}')
echo "bb-widgets <- $R"
S=$(echo "$R" | jq -r '.set_id // empty')
[ -z "$S" ] && { echo FATAL; exit 1; }
for i in $(seq 1 480); do
  RESP=$(curl -s "$BASE/api/v1/sets/$S")
  ST=$(echo "$RESP" | jq -r '.job.status // "missing"')
  case "$ST" in
    done)
      echo "$RESP" > "$A/widgets/set.json"
      echo "$RESP" | jq -r '.files[].name' | while read -r f; do
        curl -s "$BASE/api/v1/sets/$S/files/$f" -o "$A/widgets/$f"
      done
      echo "SET bb-widgets DONE ($(ls "$A/widgets" | wc -l | tr -d ' ') files)"; exit 0 ;;
    failed|cancelled) echo "$RESP" | jq -c '.job.error? // .job'; echo "SET bb-widgets $ST"; exit 1 ;;
    *) sleep 12 ;;
  esac
done
echo "SET bb-widgets TIMEOUT"; exit 1