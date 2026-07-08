@echo off
setlocal enabledelayedexpansion
cd /d "%~dp0"

echo ========================================
echo   Kiro-Go Server
echo ========================================
echo.

REM Kiem tra Go da cai chua
where go >nul 2>nul
if errorlevel 1 (
    echo [LOI] Khong tim thay Go. Hay cai dat Go: https://go.dev/dl/
    pause
    exit /b 1
)

set "CONFIG=data\config.json"
if not exist "%CONFIG%" (
    echo [LOI] Khong tim thay %CONFIG%
    pause
    exit /b 1
)

REM Dung moi instance server cu truoc, de no khong ghi de config.json (mat API key
REM vua them tay). Xem scripts\stop_prev.ps1.
echo [1/4] Dung server cu (neu co)...
powershell -NoProfile -ExecutionPolicy Bypass -File "scripts\stop_prev.ps1"
echo.

REM Tim port trong: doc port hien tai trong config, neu bi chiem thi tang dan
REM va ghi lai port trong vao config.json. In ra port cuoi cung.
echo [2/4] Kiem tra port...
for /f "usebackq delims=" %%P in (`powershell -NoProfile -ExecutionPolicy Bypass -File "scripts\ensure_port.ps1" -ConfigPath "%CONFIG%"`) do set "PORT=%%P"

if not defined PORT (
    echo [LOI] Khong xac dinh duoc port.
    pause
    exit /b 1
)

echo     -^> Dung port %PORT%
echo.

echo [3/4] Dang build...
go build ./...
if errorlevel 1 (
    echo [LOI] Build that bai. Xem loi ben tren.
    pause
    exit /b 1
)

echo [4/4] Dang khoi chay server...
echo.
echo   Admin panel : http://127.0.0.1:%PORT%/admin
echo   Claude API  : http://127.0.0.1:%PORT%/v1/messages
echo   OpenAI API  : http://127.0.0.1:%PORT%/v1/chat/completions
echo.
echo   (Nhan Ctrl+C de dung server)
echo ========================================
echo.

go run .

echo.
echo Server da dung.
pause
