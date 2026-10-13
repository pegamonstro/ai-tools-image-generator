#!/bin/bash
# BIKINI CLUB GIFs: code-rain (from_spec) + circuit-pulse (from_spec) + hero composite (composite over code-rain children)
set -u
BASE=http://127.0.0.1:9106
SITE=/Users/archcore/Projects/img-gen/.claude/worktrees/asset-expansion/.claude/mock-site-bb
A=$SITE/assets/gif
mkdir -p "$A"
S1=c991d24ac8ccc3e8   # bb-elements: logo, terminal-frame, alert-panel
S4=c9544f3d60fbf7b3   # bb-widgets: signal-track, leds-sheet
C2=03cc02fe6021f884   # bb-crew: runner-down

poll() {
  local id="$1" dir="$2"
  for i in $(seq 1 640); do
    RESP=$(curl -s "$BASE/api/v1/sets/$id")
    ST=$(echo "$RESP" | jq -r '.job.status // "missing"')
    case "$ST" in
      done)
        echo "$RESP" > "$dir/$2-.set.json"
        echo "$RESP" | jq -r '.files[].name' | while read -r f; do
          curl -s "$BASE/api/v1/sets/$id/files/$f" -o "$dir/$f"
        done
        echo "GIFSET $id DONE"; return 0 ;;
      failed|cancelled) echo "$RESP" | jq -c '.job.error? // .job'; echo "GIFSET $id $ST"; return 1 ;;
      *) sleep 12 ;;
    esac
  done
  echo "GIFSET $id TIMEOUT"; return 1
}

GIF1=$(curl -s -X POST "$BASE/api/v1/gifs" -H 'Content-Type: application/json' -d '{
 "name":"code-rain","delay_ms":140,"loop":0,"formats":["gif","webp"],"wait_seconds":0,
 "source":{"type":"from_spec","request":{"mode":"asset","seed":5101,
 "prompt":"falling phosphor green data glyphs and hex rain over a black terminal void, cyberpunk underground hacking collective, dark metal with phosphor green glow, hand painted illustration","size":"480x272"},"count":6}}')
G1=$(echo "$GIF1" | jq -r '.set_id // empty'); echo "code-rain -> $GIF1"
[ -z "$G1" ] && { echo FATAL1; exit 1; }

GIF2=$(curl -s -X POST "$BASE/api/v1/gifs" -H 'Content-Type: application/json' -d '{
 "name":"circuit-pulse","delay_ms":160,"loop":0,"formats":["gif","webp"],"wait_seconds":0,
 "source":{"type":"from_spec","request":{"mode":"asset","seed":5201,
 "prompt":"copper and neon circuit traces pulsing energy across a dark green PCB, magenta and cyan neon highlights, hand painted illustration","size":"320x192"},"count":6}}')
G2=$(echo "$GIF2" | jq -r '.set_id // empty'); echo "circuit-pulse -> $GIF2"
[ -z "$G2" ] && { echo FATAL2; exit 1; }

poll "$G1" "$A" code-rain & P1=$!
poll "$G2" "$A" circuit-pulse & P2=$!
wait $P1 || exit 1

# ---- hero composite: anim = code-rain child jobs (seeds 5101..5106) ----
KIDS=$(curl -s "$BASE/api/v1/jobs?limit=200" | jq -r '[.[] | select(.mode=="asset" and (.seed>=5101 and .seed<=5106) and .status=="done") | .id] | unique | .[0:6] | join("\n")')
N=$(echo "$KIDS" | grep -c .)
echo "code-rain kids: $N"
[ "$N" -lt 6 ] && { echo FATALKIDS; exit 1; }
ANIM=""
i=0
while IFS= read -r j; do
  ANIM="$ANIM{\"job\":\"$j\"},"
  i=$((i+1))
done <<EOF3
$KIDS
EOF3
ANIM="[${ANIM%,}]"

GIF3=$(curl -s -X POST "$BASE/api/v1/gifs" -H 'Content-Type: application/json' -d '{
 "name":"hero-banner","delay_ms":180,"loop":0,"formats":["gif","webp"],"wait_seconds":0,
 "source":{"type":"composite","composite":{
  "anim":'"$ANIM"',
  "statics":[
   {"from":{"set":"'"$S4"'","file":"signal-track.png"},"pos":[24,224]},
   {"from":{"set":"'"$S1"'","file":"terminal-frame.png"},"pos":[24,16]},
   {"from":{"set":"'"$S1"'","file":"alert-panel.png"},"pos":[256,128]},
   {"from":{"set":"'"$C2"'","file":"runner-down.png"},"pos":[288,16]},
   {"from":{"set":"'"$S1"'","file":"logo.png"},"pos":[352,8]},
   {"from":{"set":"'"$S4"'","file":"leds-sheet.png"},"pos":[384,240]}
  ]}}}')
G3=$(echo "$GIF3" | jq -r '.set_id // empty'); echo "hero-banner -> $GIF3"
[ -z "$G3" ] && { echo FATAL3; exit 1; }
poll "$G3" "$A" hero-banner || exit 1
wait $P2
echo "ALL GIFS DONE"