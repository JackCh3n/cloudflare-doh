@echo off
chcp 65001 >nul
setlocal
rem ============================================================
rem  dev.bat - 本地调试 Cloudflare Worker
rem  流程：清理旧进程 -> 安装依赖（首次） -> 启动 wrangler dev -> 打开浏览器
rem ============================================================

cd /d "%~dp0.."

echo [1/4] 清理旧的调试进程...
call "%~dp0kill-dev.bat"

echo [2/4] 检查依赖...
if not exist "node_modules" (
    echo       未发现 node_modules，正在安装依赖...
    call npm install
    if errorlevel 1 (
        echo [error] 依赖安装失败，请检查网络后重试。
        pause
        exit /b 1
    )
) else (
    echo       依赖已就绪。
)

echo [3/4] 启动本地服务 http://127.0.0.1:8787 ...
start "cloudflare-doh-dev" cmd /c "npm run dev -- --port 8787"

echo [4/4] 等待服务就绪后打开浏览器...
timeout /t 6 /nobreak >nul
start "" "http://127.0.0.1:8787/google/query-dns?name=example.com&type=A"

echo.
echo 提示：已在新窗口启动 wrangler dev，关闭该窗口即停止服务。
endlocal
exit /b 0
