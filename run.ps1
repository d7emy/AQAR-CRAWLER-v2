# PowerShell 24/7 Watchdog Supervisor (Never prompts "Terminate batch job (Y/N)?")
Set-Location $PSScriptRoot
New-Item -ItemType Directory -Force -Path "state", "logs" | Out-Null
Remove-Item "state\stopfile", "state\update_restart" -Force -ErrorAction SilentlyContinue

if (-not (Test-Path ".\aqar-crawler.exe")) {
    Write-Host "[BUILD] Compiling aqar-crawler.exe..."
    go build -p 2 -o aqar-crawler.exe .
}

while ($true) {
    Write-Host "[$(Get-Date -Format 'yyyy-MM-dd HH:mm:ss')] Starting aqar-crawler.exe -web..."
    .\aqar-crawler.exe -web -config config.json @args
    $code = $LASTEXITCODE

    if ($code -eq 42 -or (Test-Path "state\update_restart")) {
        Remove-Item "state\update_restart" -Force -ErrorAction SilentlyContinue
        Write-Host "[$(Get-Date -Format 'yyyy-MM-dd HH:mm:ss')] [AUTO-UPDATE] Binary swap complete — relaunching immediately (0s)..."
        continue
    }
    if (Test-Path "state\stopfile") {
        Remove-Item "state\stopfile" -Force -ErrorAction SilentlyContinue
        Write-Host "[$(Get-Date -Format 'yyyy-MM-dd HH:mm:ss')] Intentional shutdown (stopfile detected). Exiting."
        exit 0
    }
    if ($code -eq 2) {
        Write-Host "[$(Get-Date -Format 'yyyy-MM-dd HH:mm:ss')] CRITICAL: API rejected requests (exit code 2). Check logs\crawler.log."
        exit 2
    }
    if ($code -eq 0) {
        Write-Host "[$(Get-Date -Format 'yyyy-MM-dd HH:mm:ss')] Clean exit (code 0)."
        exit 0
    }

    Write-Host "[$(Get-Date -Format 'yyyy-MM-dd HH:mm:ss')] Process crashed (code $code)."
    if (Test-Path ".\aqar-crawler.old.exe") {
        .\aqar-crawler.exe -self-test -config config.json *> $null
        if ($LASTEXITCODE -ne 0) {
            Write-Host "[ROLLBACK] Current binary failed self-test! Restoring aqar-crawler.old.exe..."
            Move-Item -Force ".\aqar-crawler.old.exe" ".\aqar-crawler.exe"
        }
    }
    Start-Sleep -Seconds 3
}
