@echo off
setlocal enabledelayedexpansion
set AQAR_FROM_BAT=1
title AQAR Crawler v2 (24/7 Watchdog + Self-Update Supervisor)
cd /d "%~dp0"

if not exist state mkdir state
if not exist logs mkdir logs
if exist state\stopfile del /q state\stopfile >nul 2>&1
if exist state\update_restart del /q state\update_restart >nul 2>&1

if not exist aqar-crawler.exe (
    echo [BUILD] Compiling aqar-crawler.exe...
    go build -p 2 -o aqar-crawler.exe .
)

:loop
echo [%DATE% %TIME%] Starting aqar-crawler.exe -web...
.\aqar-crawler.exe -web -config config.json %*
set EXIT_CODE=%ERRORLEVEL%

:: 1. Zero-Downtime Auto-Update Handoff (Exit Code 42 or state\update_restart marker)
if "%EXIT_CODE%"=="42" (
    if exist state\update_restart del /q state\update_restart >nul 2>&1
    echo [%DATE% %TIME%] [AUTO-UPDATE] Binary swap complete — relaunching new binary immediately ^(0s^)...
    goto loop
)
if exist state\update_restart (
    del /q state\update_restart >nul 2>&1
    echo [%DATE% %TIME%] [AUTO-UPDATE] Restart marker found — relaunching new binary immediately ^(0s^)...
    goto loop
)

:: 2. Intentional Shutdown via API (/api/control {"action":"shutdown"})
if exist state\stopfile (
    del /q state\stopfile >nul 2>&1
    echo [%DATE% %TIME%] Intentional shutdown ^(stopfile detected^). Exiting watchdog cleanly.
    exit /b 0
)

:: 3. API Rejected Requests (Exit Code 2) — halt so we never hammer a refusing API
if "%EXIT_CODE%"=="2" (
    echo [%DATE% %TIME%] CRITICAL: API rejected requests ^(exit code 2^). Check logs\crawler.log.
    exit /b 2
)

:: 4. Clean Manual Exit (Ctrl+C / Exit Code 0)
if "%EXIT_CODE%"=="0" (
    echo [%DATE% %TIME%] Clean exit ^(code 0^).
    exit /b 0
)

:: 5. Crash Recovery & Automatic Rollback to Previous Binary
echo [%DATE% %TIME%] Process exited unexpectedly ^(code %EXIT_CODE%^).
if exist aqar-crawler.old.exe (
    .\aqar-crawler.exe -self-test -config config.json >nul 2>&1
    if not "!ERRORLEVEL!"=="0" (
        echo [%DATE% %TIME%] [ROLLBACK] Current binary failed self-test! Restoring aqar-crawler.old.exe...
        move /y aqar-crawler.old.exe aqar-crawler.exe >nul 2>&1
    )
)

echo [%DATE% %TIME%] Restarting in 3 seconds... ^(Press Ctrl+C to abort^)
timeout /t 3 /nobreak >nul
goto loop
