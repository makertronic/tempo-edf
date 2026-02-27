@echo off
echo === Build Tempo EDF ===
echo.

echo [1/3] Windows...
go build -ldflags "-H windowsgui" -o TempoEDF.exe .
if %errorlevel% neq 0 (
    echo ERREUR build Windows
    pause
    exit /b 1
)
echo OK : TempoEDF.exe

echo [2/3] macOS Intel...
set CGO_ENABLED=1
set GOOS=darwin
set GOARCH=amd64
go build -o TempoEDF_macos_intel . 2>nul
if %errorlevel% neq 0 (
    echo SKIP : cross-compilation macOS impossible depuis Windows
    echo        -^> utiliser build.sh sur un Mac ou GitHub Actions
) else (
    echo OK : TempoEDF_macos_intel
)

echo [3/3] macOS ARM64...
set GOARCH=arm64
go build -o TempoEDF_macos_arm64 . 2>nul
if %errorlevel% neq 0 (
    echo SKIP : cross-compilation macOS impossible depuis Windows
    echo        -^> utiliser build.sh sur un Mac ou GitHub Actions
) else (
    echo OK : TempoEDF_macos_arm64
)

rem Reset env
set CGO_ENABLED=
set GOOS=
set GOARCH=

echo.
echo === Termine ===
pause
