@echo off
rem ============================================================
rem  LLMProxy build script (Windows)
rem  Usage:
rem    build.bat                    build for current OS -> llmproxy.exe
rem    build.bat linux              build linux/amd64
rem    build.bat linux arm64        build linux/arm64
rem    build.bat darwin arm64       build macOS (Apple Silicon)
rem    build.bat windows amd64      build Windows amd64
rem  Cross-compile outputs go to dist\ with platform suffix.
rem  (ASCII-only on purpose: cmd.exe chcp 65001 is unreliable
rem   for multi-byte characters inside batch files.)
rem ============================================================
setlocal
cd /d "%~dp0"

set "TARGET_OS=%~1"
set "TARGET_ARCH=%~2"
set "LDFLAGS=-s -w"
if defined VERSION set "LDFLAGS=%LDFLAGS% -X main.buildVersion=%VERSION%"

if "%TARGET_OS%"=="-h" goto :usage
if "%TARGET_OS%"=="--help" goto :usage
if "%TARGET_OS%"=="/?" goto :usage

rem ---- check go ----
where go >nul 2>nul
if errorlevel 1 (
    echo [ERROR] go not found. Install Go 1.26.5+ and add it to PATH.
    exit /b 1
)

rem ---- default GOPROXY mirror if not set ----
if not defined GOPROXY set "GOPROXY=https://goproxy.cn,direct"

if not "%TARGET_OS%"=="" goto :cross

rem ==================== build for current OS ====================
rem clear externally set cross-compile vars to make sure we build natively
set "GOOS="
set "GOARCH="
set "CGO_ENABLED=0"
go build -ldflags "%LDFLAGS%" -o llmproxy.exe .
if errorlevel 1 (
    echo [ERROR] build failed.
    exit /b 1
)
echo [OK] llmproxy.exe
for %%A in (llmproxy.exe) do echo      size: %%~zA bytes
exit /b 0

rem ==================== cross-compile ====================
:cross
if "%TARGET_ARCH%"=="" set "TARGET_ARCH=amd64"

rem ---- validate OS ----
set "OS_OK=0"
for %%O in (windows linux darwin) do if "%TARGET_OS%"=="%%O" set "OS_OK=1"
if "%OS_OK%"=="1" goto :check_arch
echo [ERROR] unsupported target OS: %TARGET_OS%
echo        supported: windows / linux / darwin
exit /b 1

:check_arch
rem ---- validate OS/arch combination ----
set "COMBO_OK=0"
if "%TARGET_OS%"=="windows" for %%A in (386 amd64 arm64) do if "%TARGET_ARCH%"=="%%A" set "COMBO_OK=1"
if "%TARGET_OS%"=="linux" for %%A in (386 amd64 arm arm64) do if "%TARGET_ARCH%"=="%%A" set "COMBO_OK=1"
if "%TARGET_OS%"=="darwin" for %%A in (amd64 arm64) do if "%TARGET_ARCH%"=="%%A" set "COMBO_OK=1"
if "%COMBO_OK%"=="1" goto :do_build
echo [ERROR] unsupported combination: %TARGET_OS%/%TARGET_ARCH%
echo        windows: 386  amd64  arm64
echo        linux:   386  amd64  arm  arm64
echo        darwin:  amd64  arm64
exit /b 1

:do_build
set "CGO_ENABLED=0"
set "GOOS=%TARGET_OS%"
set "GOARCH=%TARGET_ARCH%"
set "EXT="
if "%TARGET_OS%"=="windows" set "EXT=.exe"
set "OUT=dist\llmproxy-%TARGET_OS%-%TARGET_ARCH%%EXT%"
if not exist dist mkdir dist
go build -ldflags "%LDFLAGS%" -o "%OUT%" .
if errorlevel 1 (
    echo [ERROR] build failed.
    exit /b 1
)
echo [OK] %OUT%
for %%A in ("%OUT%") do echo      size: %%~zA bytes
exit /b 0

:usage
echo LLMProxy build script (Windows)
echo Usage:
echo   build.bat                    build for current OS -^> llmproxy.exe
echo   build.bat ^<os^> [arch]        cross-compile, output to dist\
echo Examples:
echo   build.bat linux              build linux/amd64
echo   build.bat linux arm64        build linux/arm64
echo   build.bat darwin arm64       build macOS (Apple Silicon)
echo   build.bat windows amd64      build Windows amd64
echo Supported targets:
echo   windows: 386  amd64  arm64
echo   linux:   386  amd64  arm  arm64
echo   darwin:  amd64  arm64
echo Optional environment variables:
echo   set VERSION=1.2.3 ^&^& build.bat    inject version (default: buildVersion in source main.go)
echo   set GOPROXY=... ^&^& build.bat      override default proxy
exit /b 0

