#!/usr/bin/env bash
set -u
cd "$(dirname "$0")"

mkdir -p state logs
rm -f state/stopfile state/update_restart

if [ ! -f ./aqar-crawler ]; then
    echo "[BUILD] Compiling aqar-crawler..."
    go build -p 2 -o aqar-crawler .
fi

while true; do
    echo "[$(date -u +%Y-%m-%dT%H:%M:%SZ)] Starting aqar-crawler -web..."
    ./aqar-crawler -web -config config.json "$@"
    EXIT_CODE=$?

    # 1. Zero-Downtime Auto-Update Handoff (Exit Code 42 or state/update_restart)
    if [ "$EXIT_CODE" -eq 42 ] || [ -f state/update_restart ]; then
        rm -f state/update_restart
        echo "[$(date -u +%Y-%m-%dT%H:%M:%SZ)] [AUTO-UPDATE] Binary swap complete — relaunching immediately (0s)..."
        continue
    fi

    # 2. Intentional Shutdown via API
    if [ -f state/stopfile ]; then
        rm -f state/stopfile
        echo "[$(date -u +%Y-%m-%dT%H:%M:%SZ)] Intentional shutdown (stopfile detected). Exiting."
        exit 0
    fi

    # 3. API Rejected Requests (Exit Code 2)
    if [ "$EXIT_CODE" -eq 2 ]; then
        echo "[$(date -u +%Y-%m-%dT%H:%M:%SZ)] CRITICAL: API rejected requests (exit code 2). Check logs/crawler.log."
        exit 2
    fi

    # 4. Clean Manual Exit
    if [ "$EXIT_CODE" -eq 0 ]; then
        echo "[$(date -u +%Y-%m-%dT%H:%M:%SZ)] Clean exit (code 0)."
        exit 0
    fi

    # 5. Crash Recovery & Automatic Rollback
    echo "[$(date -u +%Y-%m-%dT%H:%M:%SZ)] Process crashed (code $EXIT_CODE)."
    if [ -f ./aqar-crawler.old ]; then
        if ! ./aqar-crawler -self-test -config config.json >/dev/null 2>&1; then
            echo "[$(date -u +%Y-%m-%dT%H:%M:%SZ)] [ROLLBACK] Restoring aqar-crawler.old..."
            mv -f ./aqar-crawler.old ./aqar-crawler
        fi
    fi

    sleep 3
done
