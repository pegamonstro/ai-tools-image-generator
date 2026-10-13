#!/bin/bash
# poll-only downloader for the three BIKINI CLUB sets whose original pollers exited.
# bb-widgets is owned by the gen-bb-widgets.sh task — not touched here.
set -u
BASE=http://127.0.0.1:9106
A=/Users/archcore/Projects/img-gen/.claude/worktrees/asset-expansion/.claude/mock-site-bb/assets

fetch_set() {
  local id="$1" dir="$2"
  mkdir -p "$dir"
  for i in $(seq 1 640); do
    RESP=$(curl -s "$BASE/api/v1/sets/$id")
    ST=$(echo "$RESP" | jq -r '.job.status // "missing"')
    case "$ST" in
      done)
        echo "$RESP" > "$dir/set.json"
        echo "$RESP" | jq -r '.files[].name' | while read -r f; do
          curl -s "$BASE/api/v1/sets/$id/files/$f" -o "$dir/$f"
        done
        echo "SET $id OK ($(ls "$dir" | wc -l | tr -d ' ') files)"; return 0 ;;
      failed|cancelled)
        echo "$RESP" | jq -c '.job.error? // .job'; echo "SET $id $ST"; return 1 ;;
      *) sleep 15 ;;
    esac
  done
  echo "SET $id TIMEOUT"; return 1
}

fetch_set c991d24ac8ccc3e8 "$A/hud" &   P1=$!
fetch_set 03cc02fe6021f884 "$A/crew" &  P2=$!
fetch_set c7674d37210a3492 "$A/board" & P3=$!
wait $P1 || echo CONTINUE_HUD_FAILED
wait $P2 || echo CONTINUE_CREW_FAILED
wait $P3 || echo CONTINUE_BOARD_FAILED
echo "ALL SETS FETCHED"