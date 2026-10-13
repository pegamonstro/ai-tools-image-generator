#!/bin/bash
# resubmit bb-board (the first run silently dropped the 20-class kit after 2 child renders were cancelled mid-mflux)
set -u
BASE=http://127.0.0.1:9106
A="$(cd "$(dirname "$0")" && pwd)/assets/board"

RESP=$(curl -s -X POST "$BASE/api/v1/asset-sets" -H 'Content-Type: application/json' -d '{
 "name":"bb-board","seed":5003,"palette_lock":true,"wait_seconds":0,"assets":[
  {"kind":"kit","name":"circuit",
   "prompt":"copper circuit board traces on dark green PCB wang tile, cyberpunk underground hacking collective, dark metal with phosphor green glow, amber BBS accents, magenta and cyan neon, hand painted illustration",
   "size":"64x64"},
  {"kind":"element","name":"datasheet",
   "prompt":"aged technical datasheet paper with schematics and grid, cyberpunk underground hacking collective, dark metal with phosphor green glow, amber BBS accents, magenta and cyan neon, hand painted illustration",
   "size":"256x256"}]}')
ID=$(echo "$RESP" | jq -r '.set_id // empty')
echo "bb-board(2) -> $ID  $RESP"
[ -z "$ID" ] && { echo FATAL; exit 1; }

for i in $(seq 1 640); do
  R=$(curl -s "$BASE/api/v1/sets/$ID")
  ST=$(echo "$R" | jq -r '.job.status // "missing"')
  case "$ST" in
    done)
      NF=$(echo "$R" | jq '.files | length')
      echo "DONE files=$NF"
      if [ "$NF" -lt 21 ]; then
        echo "PARTIAL AGAIN — files:"; echo "$R" | jq -r '.files[].name'
        exit 2
      fi
      echo "$R" > "$A/set.json"
      echo "$R" | jq -r '.files[].name' | while read -r f; do
        curl -s "$BASE/api/v1/sets/$ID/files/$f" -o "$A/$f"
      done
      echo "BOARD2 OK ($(ls "$A" | wc -l | tr -d ' ') files)"; exit 0 ;;
    failed|cancelled) echo "$R" | jq -c '.job.error? // .job'; exit 1 ;;
    *) sleep 15 ;;
  esac
done
echo TIMEOUT; exit 1