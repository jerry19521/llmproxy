echo off
rem ============================================================
rem  LLMProxy build script (Windows)
rem
rem  Usage:
rem    build.bat                       build for current OS -> llmproxy.exe
rem    build.bat <os> [arch]           cross-compile, output to dist\
rem    build.bat all                   build all Windows targets (use
rem                                    ./build.sh or GitHub Actions for
rem                                    full multi-platform matrix)
rem    build.bat -h | --help | /?      show help
rem ============================================================
setlocal
cd /d "%~dp0"

set "LDFLAGS=-s -w"
if defined VERSION set "LDFLAGS=%LDFLAGS% -X main.buildVersion=%VERSION%"

set "TARGET_OS=%~1"
set "TARGET_ARCH=%~2"

rem ---- help ----
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

if "%TARGET_OS%"=="" goto :native
if /i "%TARGET_OS%"=="all" goto :build_all
goto :cross

rem ==================== native build ====================
:native
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

rem ==================== build all (Windows only) ====================
:build_all
echo [INFO] build.bat all builds only Windows targets (3 targets).
echo        For full multi-platform matrix please use:
echo          - ./build.sh all on Linux/macOS
echo          - GitHub Actions (auto on v* tag push)
if not exist dist mkdir dist
call :do_build windows amd64
if errorlevel 1 exit /b 1
call :do_build windows arm64
if errorlevel 1 exit /b 1
call :do_build windows 386
if errorlevel 1 exit /b 1
echo.
echo [OK] all windows targets built (in dist\)
exit /b 0

rem ==================== cross-compile single target ====================
:cross
set "COMBO_OK=0"
if /i "%TARGET_OS%"=="windows" for %%A in (386 amd64 arm64) do if /i "%TARGET_ARCH%"=="%%A" set "COMBO_OK=1"
if /i "%TARGET_OS%"=="linux" for %%A in (386 amd64 arm arm64 mips mipsle mips64 mips64le) do if /i "%TARGET_ARCH%"=="%%A" set "COMBO_OK=1"
if /i "%TARGET_OS%"=="darwin" for %%A in (amd64 arm64) do if /i "%TARGET_ARCH%"=="%%A" set "COMBO_OK=1"
if /i "%TARGET_OS%"=="freebsd" for %%A in (amd64 arm64) do if /i "%TARGET_ARCH%"=="%%A" set "COMBO_OK=1"
if /i "%TARGET_OS%"=="openbsd" for %%A in (amd64 arm64) do if /i "%TARGET_ARCH%"=="%%A" set "COMBO_OK=1"
if /i "%TARGET_OS%"=="android" for %%A in (arm64) do if /i "%TARGET_ARCH%"=="%%A" set "COMBO_OK=1"
if "%COMBO_OK%"=="0" (
    echo [ERROR] unsupported combination: %TARGET_OS%/%TARGET_ARCH%
    echo        supported targets:
    echo          windows: 386  amd64  arm64
    echo          linux:   386  amd64  arm  arm64  mips  mipsle  mips64  mips64le
    echo          darwin:  amd64  arm64
    echo          freebsd: amd64  arm64
    echo          openbsd: amd64  arm64
    echo          android: arm64
    exit /b 1
)
call :do_build "%TARGET_OS%" "%TARGET_ARCH%"
if errorlevel 1 exit /b 1
exit /b 0

rem ---- worker subroutine ----
:do_build
set "SUB_OS=%~1"
set "SUB_ARCH=%~2"
set "CGO_ENABLED=0"
set "GOOS=%SUB_OS%"
set "GOARCH=%SUB_ARCH%"
set "EXT="
if /i "%SUB_OS%"=="windows" set "EXT=.exe"
set "OUT=dist\llmproxy-%SUB_OS%-%SUB_ARCH%%EXT%"
if not exist dist mkdir dist
echo.
echo ==^> [build] %SUB_OS%/%SUB_ARCH%
go build -ldflags "%LDFLAGS%" -o "%OUT%" .
if errorlevel 1 (
    echo [ERROR] failed: %SUB_OS%/%SUB_ARCH%
    exit /b 1
)
echo [OK] %OUT%
for %%A in ("%OUT%") do echo      size: %%~zA bytes
exit /b 0

rem ==================== usage ====================
:usage
echo LLMProxy build script (Windows)
echo Usage:
echo   build.bat                       build for current OS -^> llmproxy.exe
echo   build.bat ^<os^> [arch]           cross-compile, output to dist\
echo   build.bat all                   build all Windows targets
echo   build.bat -h ^| --help ^| /?     show this help
echo.
echo Supported targets (os/arch):
echo   windows: 386  amd64  arm64
echo   linux:   386  amd64  arm  arm64  mips  mipsle  mips64  mips64le
echo   darwin:  amd64  arm64
echo   freebsd: amd64  arm64
echo   openbsd: amd64  arm64
echo   android: arm64
echo.
echo Notes:
echo   build.bat only cross-compiles from Windows; for the full multi-platform
echo   matrix (linux mips, freebsd, openbsd, android, etc.) please use:
echo     - ./build.sh all on Linux/macOS, or
echo     - GitHub Actions (auto-released on v* tag push)
echo.
echo Optional environment variables:
echo   set VERSION=1.0.0 ^&^& build.bat         inject version (default: buildVersion in source)
echo   set GOPROXY=... ^&^& build.bat           override default proxy
exit /b 0
