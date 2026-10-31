@echo off
rem Starts the 24-hour AQAR crawler using config.json.
rem
rem   STOPPING:
rem     - TUI mode (-tui): press q or Ctrl+C
rem     - Plain mode:      press Ctrl+C
rem     - Force kill (if nothing works): taskkill /IM aqar-crawler.exe /F
rem   It is safe to force-kill: progress is saved after every page and the
rem   crawler resumes where it stopped on the next start.
rem
rem Exit code 2 = API rejected our requests -> read logs\crawler.log.
cd /d %~dp0
aqar-crawler.exe -config config.json
echo.
echo Crawler exited with code %ERRORLEVEL%
pause
