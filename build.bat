@echo off
setlocal enabledelayedexpansion

REM ===========================================================================
REM  teledown-go build script
REM
REM    the whole program is one package (root "."), so there is a single
REM    artifact:  dist\teledown-go-{os}-{arch}[.exe]
REM
REM  Usage:
REM    build.bat [clean] [nopause] [--no-vet] [--no-upx]
REM              [--targets=windows/amd64,linux/amd64]
REM
REM    clean       remove old artifacts before building
REM                (dist\.env.example and dist\version.txt are rewritten)
REM    nopause     do not pause at the end (for script / CI use)
REM    --no-vet    skip the go vet pre-check
REM    --no-upx    skip UPX compression
REM    --targets   comma separated platform list, e.g. linux/arm64,darwin/arm64
REM                (default: windows/amd64 linux/amd64)
REM
REM  CGO_ENABLED=0 is forced below: the session store uses a pure Go SQLite
REM  driver (modernc.org/sqlite), so cross compiling needs no C toolchain.
REM
REM  NOTE: keep this file ASCII only. A chcp switch inside a batch file makes
REM        cmd.exe lose its read offset and mangle later lines.
REM ===========================================================================

REM ---- command line ----
REM cmd splits batch arguments on space, tab, comma, semicolon and equals sign,
REM so "--targets=linux/amd64,linux/arm64" arrives as three tokens:
REM "--targets" "linux/amd64" "linux/arm64". --targets therefore consumes the
REM following platform tokens as a group.
set "DO_CLEAN=0"
set "DO_PAUSE=1"
set "DO_VET=1"
set "DO_UPX=1"
set "DO_HELP=0"
set "TARGETS=windows/amd64 linux/amd64"
set "TARGETS_SET=0"
set "EXPECT_TARGETS=0"

:parse
if "%~1"=="" goto parsed
set "ARG=%~1"
set "CONSUMED=0"
set "KNOWN=0"

REM platform token requested by a previous --targets
if "!EXPECT_TARGETS!"=="1" if not "!ARG:~0,2!"=="--" (
    set "CONSUMED=1"
    if "!TARGETS_SET!"=="0" (
        set "TARGETS=!ARG!"
        set "TARGETS_SET=1"
    ) else (
        set "TARGETS=!TARGETS! !ARG!"
    )
)
if "!EXPECT_TARGETS!"=="1" if "!CONSUMED!"=="0" set "EXPECT_TARGETS=0"

if "!CONSUMED!"=="0" (
    if /i "!ARG!"=="--targets"  (set "EXPECT_TARGETS=1" & set "KNOWN=1")
    if /i "!ARG!"=="clean"      (set "DO_CLEAN=1" & set "KNOWN=1")
    if /i "!ARG!"=="nopause"    (set "DO_PAUSE=0" & set "KNOWN=1")
    if /i "!ARG!"=="--no-pause" (set "DO_PAUSE=0" & set "KNOWN=1")
    if /i "!ARG!"=="--no-vet"   (set "DO_VET=0" & set "KNOWN=1")
    if /i "!ARG!"=="--no-upx"   (set "DO_UPX=0" & set "KNOWN=1")
    REM help only sets a flag: jumping out of the loop here would ignore
    REM the arguments that follow it, e.g. "build.bat --help nopause".
    if /i "!ARG!"=="help"       (set "DO_HELP=1" & set "KNOWN=1")
    if /i "!ARG!"=="-h"         (set "DO_HELP=1" & set "KNOWN=1")
    if /i "!ARG!"=="--help"     (set "DO_HELP=1" & set "KNOWN=1")
    REM warn instead of silently ignoring: command line typos are otherwise
    REM invisible (e.g. "--no-upxx" would just keep compressing)
    if "!KNOWN!"=="0" echo [WARN] ignoring unknown argument: !ARG!
)
shift
goto parse
:parsed
REM safety net for platforms that survived as one token or still carry commas
set "TARGETS=!TARGETS:,= !"
if "%DO_HELP%"=="1" goto usage

set "OUTDIR=dist"

echo ============================================================
echo  teledown-go build
echo ============================================================
echo.

REM ---- environment checks ----
where go >nul 2>nul
if errorlevel 1 (
    echo [ERROR] go not found in PATH. Please install Go first.
    goto fail
)
if not exist "go.mod" (
    echo [ERROR] go.mod not found. Run this script from the repository root.
    goto fail
)
if not exist "%OUTDIR%" mkdir "%OUTDIR%"

REM ---- version info (falls back when not a git repo) ----
set "VERSION="
for /f "delims=" %%a in ('git describe --tags --abbrev^=0 2^>nul') do set "VERSION=%%a"
if "!VERSION!"=="" set "VERSION=0.0.0"
set "COMMIT="
for /f "delims=" %%a in ('git rev-parse --short HEAD 2^>nul') do set "COMMIT=%%a"
if "!COMMIT!"=="" set "COMMIT=unknown"
for /f "delims=" %%a in ('powershell -NoProfile -Command "Get-Date -Format yyyy-MM-dd_HH-mm-ss"') do set "BUILDTIME=%%a"
if "!BUILDTIME!"=="" set "BUILDTIME=unknown"

echo Version: %VERSION%   Commit: %COMMIT%   BuildTime: %BUILDTIME%
echo.

REM ---- go vet pre-check (fail early instead of shipping broken code) ----
if "%DO_VET%"=="1" (
    echo [check] go vet ./...
    go vet ./...
    if errorlevel 1 (
        echo.
        echo [ERROR] go vet failed, build aborted. Use --no-vet to ignore.
        goto fail
    )
) else (
    echo [check] go vet skipped
)

REM ---- UPX availability (probe once) ----
set "UPX_OK=0"
if "%DO_UPX%"=="1" (
    where upx >nul 2>nul
    if not errorlevel 1 set "UPX_OK=1"
)

REM ---- clean old artifacts (version.txt and .env.example are rewritten) ----
if "%DO_CLEAN%"=="1" (
    echo [clean] removing %OUTDIR%\teledown-go-* and %OUTDIR%\version.txt
    if exist "%OUTDIR%\teledown-go-*" del /f /q "%OUTDIR%\teledown-go-*" >nul 2>nul
    if exist "%OUTDIR%\version.txt" del /f /q "%OUTDIR%\version.txt" >nul 2>nul
)

REM ---- version file and sample config (both ship with the binary) ----
>  "%OUTDIR%\version.txt" echo Version=%VERSION%
>> "%OUTDIR%\version.txt" echo Commit=%COMMIT%
>> "%OUTDIR%\version.txt" echo BuildTime=%BUILDTIME%
if exist ".env.example" copy /Y ".env.example" "%OUTDIR%\" >nul

REM ======================= build =======================
echo.
echo === building teledown-go ===
for %%T in (%TARGETS%) do (
    call :build_one "%%T"
    if errorlevel 1 goto fail
)

REM ======================= summary =======================
echo.
echo ============================================================
echo  build finished
echo ============================================================
echo  output: %CD%\%OUTDIR%
echo    binary : %OUTDIR%\teledown-go-*
echo.
echo  notes:
echo    - ship %OUTDIR%\teledown-go-* together with .env.example and version.txt
echo    - never ship .env or *.session: they hold api_hash and the account
echo      auth key, anyone who gets them can use the account
echo    - ffmpeg / ffprobe are optional, they only add thumbnails and video
echo      attributes on upload
set "RC=0"
goto finish

REM ======================= build one target =======================
:build_one
REM %~1 = platform os/arch
set "SPEC=%~1"
set "GOOS="
set "GOARCH="
set "GOARM="
for /f "tokens=1,2 delims=/" %%a in ("%SPEC%") do (
    set "GOOS=%%a"
    set "GOARCH=%%b"
)
if "!GOOS!"=="" (
    echo [ERROR] cannot parse platform "!SPEC!", expected os/arch such as linux/amd64
    exit /b 1
)
if "!GOARCH!"=="" (
    echo [ERROR] platform "!SPEC!" has no arch, expected os/arch
    exit /b 1
)

set "DISPARCH=!GOARCH!"
if /i "!GOARCH!"=="amd64" set "DISPARCH=x86_64"
if /i "!GOARCH!"=="armv7" (
    set "GOARCH=arm"
    set "GOARM=7"
)
set "EXT="
if /i "!GOOS!"=="windows" set "EXT=.exe"

set "PKG=."
set "OUTFILE=%OUTDIR%\teledown-go-!GOOS!-!DISPARCH!!EXT!"

REM delete first: a locked file gets a clear message instead of "Access is denied"
if exist "!OUTFILE!" (
    del /f /q "!OUTFILE!" >nul 2>nul
    if exist "!OUTFILE!" (
        echo [ERROR] cannot overwrite !OUTFILE!
        echo         the file is in use, close the running program and retry.
        exit /b 1
    )
)

echo ------------------------------------------------------------
echo building !OUTFILE!   [target !GOOS!/!DISPARCH!]
echo ------------------------------------------------------------
set "CGO_ENABLED=0"
go build -trimpath -o "!OUTFILE!" -ldflags "-s -w" !PKG!
if errorlevel 1 (
    echo.
    echo [ERROR] build failed: !GOOS!/!DISPARCH!
    exit /b 1
)

if "%UPX_OK%"=="1" (
    echo   compressing with UPX...
    upx --best --lzma "!OUTFILE!"
) else (
    echo   UPX compression disabled
)

set "SZKB=0"
for %%F in ("!OUTFILE!") do set /a "SZKB=%%~zF/1024"
echo   done [!SZKB! KB]
echo.
exit /b 0

REM ======================= usage =======================
:usage
echo Usage: build.bat [clean] [nopause] [--no-vet] [--no-upx] [--targets=os/arch,...]
echo.
echo   clean       remove old artifacts before building
echo   nopause     do not pause at the end
echo   --no-vet    skip the go vet pre-check
echo   --no-upx    skip UPX compression
echo   --targets   custom platforms, e.g. --targets=linux/arm64,darwin/arm64
echo.
echo output: dist\teledown-go-{os}-{arch}[.exe]
echo default targets: windows/amd64 linux/amd64
set "RC=0"
goto finish

REM ======================= failure exit =======================
:fail
echo.
echo ============================================================
echo  build failed
echo ============================================================
set "RC=1"

REM ======================= common exit =======================
:finish
if "%DO_PAUSE%"=="1" (
    echo.
    echo Press any key to exit...
    pause >nul
)
endlocal & exit /b %RC%
