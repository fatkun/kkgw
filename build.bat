@echo off
setlocal

set "OUTPUT=kkapi_test.exe"

echo [INFO] Building Windows binary: %OUTPUT%
go build -trimpath -ldflags="-s -w" -o "%OUTPUT%" .
if errorlevel 1 (
    echo [ERROR] Build failed.
    exit /b 1
)

echo [INFO] Build completed: %OUTPUT%
exit /b 0
