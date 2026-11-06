@echo off
chcp 65001 >nul
cd /d "%~dp0"
set ADDR=127.0.0.1:9010

if not exist "adskull-register.exe" (
    echo [错误] 当前目录找不到 adskull-register.exe
    echo 请先在此目录执行:  go build -o adskull-register.exe .
    echo.
    pause
    goto :eof
)

rem 若 9010 已经在监听，说明服务已经在跑，直接开浏览器，不重复启动
netstat -ano -p tcp | findstr "127.0.0.1:9010" | findstr "LISTENING" >nul
if %errorlevel%==0 (
    echo [提示] 服务已经在运行，直接打开浏览器...
    start http://127.0.0.1:9010
    echo 如需停止，请到运行服务的那个窗口按 Ctrl+C。
    echo.
    pause
    goto :eof
)

echo ========================================
echo    chatgpt-register 启动中
echo    地址 : http://127.0.0.1:9010
echo    账号 : admin / admin123
echo    停止 : 在本窗口按 Ctrl+C
echo ========================================
echo.

rem 等 2 秒让服务起来，再自动打开浏览器
start "" /min cmd /c "timeout /t 2 /nobreak >nul & start http://127.0.0.1:9010"

adskull-register.exe

echo.
echo 服务已停止，按任意键关闭窗口。
pause >nul
