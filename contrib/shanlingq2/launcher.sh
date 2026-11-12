#!/bin/sh
# Installed as /mnt/mmc/.jellyfin/run. Q2 Pod starts it from Streaming's Jellyfin row (a card app,
# q2-pod's patch/addon.c). It runs from /tmp, never the card: the app and this launcher are
# copied there first and the card's copy of this script is left at once (exec), so nothing keeps
# the card busy and USB storage can always take it. The loop restarts the app after a failure,
# waiting 5 s and doubling to a minute; an exit of 0 (stopped from the Q2 or the phone, or idle) ends
# it. A failed run's stderr (a panic's trace) and exit code are kept in /tmp/q2-jellyfin.crash, which
# the next start puts in .jellyfin/log.txt. Q2 Pod stops it with a TERM to the loop's pid in
# /tmp/q2-jellyfin.
pid=/tmp/q2-jellyfin
if [ "$1" != --loop ]; then
    [ -f $pid ] && kill -0 "$(cat $pid)" 2>/dev/null && exit 0
    dir=$(cd "$(dirname "$0")" && pwd)
    cp -f "$dir/q2-jellyfin" /tmp/q2-jellyfin.bin && chmod 755 /tmp/q2-jellyfin.bin || exit 1
    cp -f "$0" /tmp/q2-jellyfin.run || exit 1
    cd /tmp
    exec /bin/sh /tmp/q2-jellyfin.run --loop "$dir"
fi
dir=$2
(
    child=
    # A TERM keeps a crash for the next start: a crash loop stopped from the Q2 is when it matters.
    # An app that hasn't ended 2 s after its TERM is killed, so the loop is gone within the 3 s Q2
    # Pod waits (addon.c's AWAY_MS).
    trap '[ -n "$child" ] && kill -TERM $child 2>/dev/null && { (sleep 2; kill -KILL $child) 2>/dev/null & w=$!; wait $child; kill $w 2>/dev/null; }; rm -f $pid /tmp/q2-jellyfin.bin /tmp/q2-jellyfin.run /tmp/q2-jellyfin.err; exit 0' TERM INT
    delay=5
    while :; do
        /tmp/q2-jellyfin.bin -dir "$dir" 2>/tmp/q2-jellyfin.err &
        child=$!
        wait $child
        code=$? child=
        [ $code = 0 ] && break
        { cat /tmp/q2-jellyfin.err; echo "exit $code"; } >/tmp/q2-jellyfin.crash
        sleep $delay &
        wait $! # not sleep alone: a TERM interrupts a wait, but waits for a foreground sleep
        [ $delay -lt 60 ] && delay=$((delay * 2))
    done
    rm -f $pid /tmp/q2-jellyfin.bin /tmp/q2-jellyfin.run /tmp/q2-jellyfin.err /tmp/q2-jellyfin.crash
) </dev/null >/dev/null 2>&1 &
echo $! > $pid
