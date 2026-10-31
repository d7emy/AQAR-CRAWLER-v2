@echo off
rem Builds aqar-crawler.exe (requires Go 1.22+ installed)
go build -o aqar-crawler.exe .
if %ERRORLEVEL% NEQ 0 (
    echo Build failed. Install Go from https://go.dev/dl/ and try again.
    pause
) else (
    echo Build OK: aqar-crawler.exe
)
