# Autonomous Git Push & Live Remote Server Update Playbook

Whenever you modify code, configuration, or documentation in this repository, you MUST automatically test, commit, push, and update the live remote server without waiting for the user to ask.

## Mandatory 5-Step Deployment Sequence

### Step 1: Local Verification Gate (Never push broken code)
Run all checks locally before committing:
```powershell
go vet ./...
go test ./...
go build -o aqar-crawler.exe .
.\aqar-crawler.exe -self-test
```

### Step 2: Clean Git Commit (Never commit runtime files or secrets)
Ensure `state/`, `logs/`, `*.db*`, `*.exe`, `*.new*`, `*.old*`, and `config.json` (which contains live Telegram/proxy secrets) remain untracked:
```powershell
git status --short
git add <changed-source-files>
git commit -m "<concise description of change>"
```

### Step 3: Push to `origin/main`
```powershell
git push origin main
```

### Step 4: Trigger Immediate Live Server Update via HTTP API
Trigger the remote server's `AutoUpdater` immediately (do not wait for the 2-minute background poll):
```powershell
.\aqar-crawler.exe -remote <SERVER_IP>:9090 -remote-cmd update
# Or via curl:
curl.exe -s -X POST http://<SERVER_IP>:9090/api/update
```
- **Plan A (`hot_reload`)**: Completes in `<5ms` with `0ms` downtime when only config/docs/scripts changed.
- **Plan B (`binary_handoff`)**: Compiles `aqar-crawler.new.exe` in the background while the crawler continues running, runs `-self-test`, swaps binaries, checkpoints mid-cycle state + SQLite WAL, and restarts in `<1s` via exit code `42`.

### Step 5: Post-Handoff Verification
Wait 2 seconds after a `binary_handoff`, then query `/api/status`:
```powershell
Start-Sleep -Seconds 2
.\aqar-crawler.exe -remote <SERVER_IP>:9090 -remote-cmd status
```
Confirm:
- `update.current_commit` matches the newly pushed commit SHA on `origin/main`.
- `update.handoff_pending` is `false` and `update.handoff_duration_ms` is recorded.
- `running` is `true` and `live.cycle` / `db.listings` continuity is preserved.
