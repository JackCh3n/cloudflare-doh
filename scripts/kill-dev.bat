@echo off
chcp 65001 >nul
setlocal
rem ============================================================
rem  kill-dev.bat - 结束本地调试进程（wrangler 8787 / EdgeOne 8088、6699）
rem  仅使用 netstat + taskkill，不依赖 PowerShell
rem ============================================================

set PORTS=8787 8088 6699
set FOUND=0

for %%P in (%PORTS%) do (
    for /f "tokens=5" %%I in ('netstat -ano ^| findstr /r /c:"LISTENING" ^| findstr /c:":%%P "') do (
        echo [kill] 端口 %%P 被进程 %%I 占用，正在结束...
        taskkill /F /PID %%I >nul 2>&1
        if not errorlevel 1 set FOUND=1
    )
)

if "%FOUND%"=="0" echo [ok] 没有需要结束的调试进程。
echo.
endlocal
exit /b 0
