@echo off
set "script_dir=%~dp0"
for %%i in ("%script_dir%.") do set "GONAME=%%~ni"
if not exist "go.mod" (
    echo Initializing module in ...
    go mod init %GONAME%
    if errorlevel 1 (
        echo Failed to initialize %GONAME% module
    )
)
go mod tidy
set GOOS=windows
set GOARCH=amd64
taskkill /f /im "%GONAME%.exe" 2>nul
timeout /t 2 /nobreak >nul
go build -ldflags="-s -w" -o "%GONAME%.exe" main.go