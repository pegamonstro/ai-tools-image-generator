#!/bin/bash
# BIKINI CLUB asset sets — bb-elements / bb-crew / bb-board on :9106 (direct routing)
set -u
BASE=http://127.0.0.1:9106
SITE="$(cd "$(dirname "$0")" && pwd)"
A=$SITE/assets
mkdir -p "$A"

STYLE="cyberpunk underground hacking collective, dark metal with phosphor green glow, amber BBS accents, magenta and cyan neon, hand painted illustration"
NEG="photograph, blurry, watermark"

sub() { curl -s -X POST "$BASE/api/v1/asset-sets" -H 'Content-Type: application/json' -d "$1"; }

E1=$(cat <<EOF
{"name":"bb-elements","seed":5001,"palette_lock":true,"wait_seconds":0,"assets":[
 {"kind":"element","name":"logo","prompt":"skull-faced hacker mask emblem wired with glowing circuits, $STYLE","negative":"$NEG","size":"128x128","transparent":true},
 {"kind":"frame","name":"terminal-frame","prompt":"CRT phosphor terminal window bezel with chipped steel corners, $STYLE","negative":"$NEG","size":"256x192","transparent":true,"frame_margins":24},
 {"kind":"frame","name":"alert-panel","prompt":"amber warning alert panel with diagonal hazard strip bezel, $STYLE","negative":"$NEG","size":"208x128","transparent":true,"frame_margins":16},
 {"kind":"frame","name":"tip","prompt":"slim dark tooltip bezel with green edge glow, $STYLE","negative":"$NEG","size":"192x48","transparent":true,"frame_margins":8},
 {"kind":"element","name":"btn-hot","prompt":"magenta neon keypad button raised chrome lip, $STYLE","negative":"$NEG","size":"128x48","transparent":true},
 {"kind":"element","name":"btn-cool","prompt":"cyan neon keypad button raised chrome lip, $STYLE","negative":"$NEG","size":"128x48","transparent":true},
 {"kind":"element","name":"btn-dim","prompt":"dark steel keypad button pressed-in look, $STYLE","negative":"$NEG","size":"128x48","transparent":true}
]}
EOF
)
R1=$(sub "$E1"); echo "bb-elements <- $R1"; S1=$(echo "$R1"|jq -r '.set_id // empty')

E4=$(cat <<EOF
{"name":"bb-widgets","seed":5111,"palette_lock":true,"wait_seconds":0,"assets":[
 {"kind":"bar","name":"signal","prompt":"phosphor green signal strength meter in dark metal chassis, $STYLE","negative":"$NEG","size":"224x32","frame_margins":6,"bar_fill":"flowing phosphor green energy","fill_states":[20,40,60,80,100]},
 {"kind":"bar","name":"load","prompt":"magenta and cyan cpu load meter in dark metal chassis, $STYLE","negative":"$NEG","size":"224x32","frame_margins":6,"bar_fill":"crackling magenta and cyan electric plasma","fill_states":[20,40,60,80,100]},
 {"kind":"atlas","name":"leds","prompt":"status indicator LED lens: green ok amber warn magenta busy, $STYLE","negative":"$NEG","size":"32x32","transparent":true,"frames":3},
 {"kind":"atlas","name":"chips","prompt":"component icon: integrated circuit chip, capacitor, resistor, crystal diode, dark metal with neon trim, $STYLE","negative":"$NEG","size":"64x64","transparent":true,"frames":4},
 {"kind":"element","name":"glitch","prompt":"datamosh glitch divider strip torn pixels magenta cyan, $STYLE","negative":"$NEG","size":"384x32","transparent":true},
 {"kind":"element","name":"hexfield","prompt":"wall of terminal hex dump with scattered green glyphs, $STYLE","negative":"$NEG","size":"512x128"},
 {"kind":"element","name":"scan","prompt":"CRT scanline overlay pattern, faint phosphor glow rows, $STYLE","negative":"$NEG","size":"256x256","transparent":true}
]}
EOF
)
R4=$(sub "$E4"); echo "bb-widgets <- $R4"; S4=$(echo "$R4"|jq -r '.set_id // empty')

E2=$(cat <<EOF
{"name":"bb-crew","seed":5002,"palette_lock":true,"wait_seconds":0,"assets":[
 {"kind":"npc","name":"ghost","shape":"variants","variants":["calm stare","mid-laugh grin","typing focus glow","system crash panic"],"prompt":"hooded rogue admin avatar with glowing visor, chunky painterly sprite, $STYLE","negative":"$NEG","size":"64x64","columns":4},
 {"kind":"npc","name":"runner","shape":"walk4","prompt":"sneaker-running courier hacker sprite mid stride, chunky painterly sprite, $STYLE","negative":"$NEG","size":"64x64"},
 {"kind":"element","name":"phreaker","prompt":"portrait of a phreaker with soldered headset and grin, chunky painterly portrait, $STYLE","negative":"$NEG","size":"128x128","transparent":true},
 {"kind":"npc","name":"spectre","shape":"variants","variants":["smug flicker","corrupting apart"],"prompt":"translucent signal spectre made of scanlines, chunky painterly sprite, $STYLE","negative":"$NEG","size":"48x48","columns":2}
]}
EOF
)
R2=$(sub "$E2"); echo "bb-crew <- $R2"; S2=$(echo "$R2"|jq -r '.set_id // empty')

E3=$(cat <<EOF
{"name":"bb-board","seed":5003,"palette_lock":true,"wait_seconds":0,"assets":[
 {"kind":"kit","name":"circuit","prompt":"copper circuit board traces on dark green PCB wang tile, $STYLE","negative":"$NEG","size":"64x64"},
 {"kind":"element","name":"datasheet","prompt":"aged technical datasheet paper with schematics and grid, $STYLE","negative":"$NEG","size":"256x256"}
]}
EOF
)
if [ -n "${SKIP_BOARD+x}" ]; then S3=c7674d37210a3492; else R3=$(sub "$E3"); echo "bb-board <- $R3"; S3=$(echo "$R3"|jq -r '.set_id // empty'); fi

poll() {
  local id="$1" dir="$2"
  for i in $(seq 1 480); do
    RESP=$(curl -s "$BASE/api/v1/sets/$id")
    ST=$(echo "$RESP" | jq -r '.job.status // "missing"')
    case "$ST" in
      done)
        echo "$RESP" > "$dir/set.json"
        echo "$RESP" | jq -r '.files[].name' | while read -r f; do
          curl -s "$BASE/api/v1/sets/$id/files/$f" -o "$dir/$f"
        done
        echo "SET $id DONE ($(ls "$dir" | wc -l | tr -d ' ') files)"; return 0 ;;
      failed|cancelled)
        echo "$RESP" | jq -c '.job.error? // .job'; echo "SET $id $ST"; return 1 ;;
      *) sleep 12 ;;
    esac
  done
  echo "SET $id TIMEOUT"; return 1
}

[ -z "$S1" ] || [ -z "$S2" ] || [ -z "$S3" ] || [ -z "$S4" ] && { echo FATALSUB; exit 1; }
poll "$S1" "$A/hud" & PH1=$!
poll "$S2" "$A/crew" & PH2=$!
poll "$S3" "$A/board" & PH3=$!
poll "$S4" "$A/widgets" & PH4=$!
OK=0
for P in $PH1 $PH2 $PH3 $PH4; do wait $P || OK=1; done
[ "$OK" = 0 ] && echo "ALL SETS DONE" || echo "SOME SETS FAILED"