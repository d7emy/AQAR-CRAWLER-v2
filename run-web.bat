@echo off
rem Starts the crawler + web control panel.
rem Open http://127.0.0.1:8080 in a browser to view progress and control
rem the crawler (pause / resume / stop / start).
rem
rem   STOPPING: close the window or press Ctrl+C, or use the Stop button.
rem   Exit code 2 = API rejected our requests -> read logs\crawler.log.
cd /d %~dp0
aqar-crawler.exe -web
echo.
echo Crawler exited with code %ERRORLEVEL%
pause
