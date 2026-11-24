#!/usr/bin/env bash
# rollback.sh: hands the queue back from Shunter to the queue it replaced,
# in minutes (docs/cutover.md, Rollback). Every step is timed, and the run
# fails loudly at the first step that does not complete.
#
#   SHUNTER_STORE=/var/lib/shunter/store SHUNTER_SOCKET=/var/lib/shunter/shunter.sock \
#   STOP_CMD='systemctl stop shunter shunter-linecheck@* shunter-intake' \
#   MAP_CMD='/srv/old-queue/map-lines' CHECK_CMD='/srv/old-queue/list-lines' \
#   START_CMD='systemctl start old-queue' examples/rollback.sh OUT_DIR
#
# SHUNT        the CLI (default: shunt)
# STOP_CMD     stops the daemon and everything that calls it (the line-check
#              workers, the intake). It must return only once they have stopped.
# MAP_CMD      run as "MAP_CMD OUT_DIR": writes the line files in OUT_DIR into the old
#              queue's own lists, under the old queue's own lock.
# CHECK_CMD    prints the old queue's lines after the map, one a row, as
#              "<change> <change>..." (a leading "b", "#" on a change, and a
#              trailing "alone" are ignored).
# START_CMD    starts the old queue. Leave it empty to start it by hand.
# LANDING_WAIT how long to wait for a train that is landing (default 300s:
#              the push's own deadline). A landing is never interrupted.
#
# Steps: pause; cancel each live train with requeue (a landing one is
# waited for); wait until no train is live and no effect is owed; stop;
# export the line files offline; map; check that every exported line is in
# the old lists exactly once; start the old queue.
set -euo pipefail
OUT=${1:?usage: rollback.sh OUT_DIR}
SHUNT=${SHUNT:-shunt}
: "${SHUNTER_STORE:?the store directory}" "${STOP_CMD:?}" "${MAP_CMD:?}" "${CHECK_CMD:?}"
LANDING_WAIT=${LANDING_WAIT:-300}
T0=$(date +%s)
step() { printf '%s +%ds %s\n' "$(date -u +%H:%M:%SZ)" $(( $(date +%s) - T0 )) "$*"; }
fail() { step "FAILED: $*"; exit 1; }
trains() { $SHUNT status | awk '$1 == "train" { print $3 }'; }

step "pause"
$SHUNT pause --reason "rollback to the old queue" >/dev/null || fail "pause"

step "cancel the live trains, with requeue"
deadline=$(( $(date +%s) + LANDING_WAIT + 60 ))
while :; do
  live=$(trains) || fail "status"
  owed=$($SHUNT status | awk '/^effects owed/ { print $3 }')
  [ -z "$live" ] && [ "${owed:-0}" = 0 ] && break
  [ "$(date +%s)" -lt "$deadline" ] || fail "trains still live after ${LANDING_WAIT}s: $live"
  for t in $live; do
    # A landing train refuses: it is past the point of no return, and waited for.
    $SHUNT cancel "$t" --action requeue --reason "rollback to the old queue" >/dev/null 2>&1 || true
  done
  sleep 2
done

step "stop Shunter and its callers"
sh -c "$STOP_CMD" || fail "stop"

step "export the line files"
mkdir -p "$OUT"
$SHUNT export --offline --store "$SHUNTER_STORE" --line-files "$OUT" >/dev/null || fail "export"

step "map into the old queue's lists"
sh -c "$MAP_CMD \"\$1\"" map "$OUT" || fail "map"

step "check: every exported line is in the old lists exactly once"
norm() { sed -e 's/--.*//' -e 's/#//g' | awk '{ s = ""; for (i = 1; i <= NF; i++) { if ((i == 1 && $i == "b") || $i == "alone") continue; s = s (s == "" ? "" : " ") $i } if (s != "") print s }'; }
want=$(grep -hE '^b ' "$OUT"/waiting.list "$OUT"/parked.list 2>/dev/null | norm | sort || true)
have=$(sh -c "$CHECK_CMD" | norm | sort) || fail "check command"
missing=$(comm -23 <(printf '%s\n' "$want" | sed '/^$/d') <(printf '%s\n' "$have" | sed '/^$/d' | sort -u))
twice=$(printf '%s\n' "$have" | sed '/^$/d' | uniq -d)
[ -z "$missing" ] || fail "lines exported and not in the old lists: $(paste -sd';' <<<"$missing")"
[ -z "$twice" ] || fail "lines in the old lists twice: $(paste -sd';' <<<"$twice")"
step "$(printf '%s\n' "$want" | sed '/^$/d' | wc -l) lines handed back"

if [ -n "${START_CMD:-}" ]; then
  step "start the old queue"
  sh -c "$START_CMD" || fail "start"
fi
total=$(( $(date +%s) - T0 ))
step "done in ${total}s$([ "$total" -lt 600 ] || echo ': OVER the ten-minute budget')"
