@echo off
chcp 65001 >nul
setlocal
rem ============================================================
rem  deploy-jp.bat - 交叉编译 Go 版并部署到日本节点
rem  前置：已安装 go、PuTTY（plink/pscp），并设置环境变量 JP_SSH_PASS
rem  用法：set JP_SSH_PASS=你的密码  &&  scripts\deploy-jp.bat
rem ============================================================

set HOST=203.0.113.10
set PORT=22
set USER=root
set REMOTE_DIR=/opt/doh-proxy
set PSCP=pscp.exe
set PLINK=plink.exe

if not "%JP_SSH_PASS%"=="" goto :havepass
echo [error] 未设置 JP_SSH_PASS 环境变量。
echo         例：set JP_SSH_PASS=你的密码  ^&^&  scripts\deploy-jp.bat
exit /b 1
:havepass

where pscp.exe >nul 2>&1
if not errorlevel 1 goto :havep
set PSCP="C:\Program Files (x86)\PuTTY\pscp.exe"
set PLINK="C:\Program Files (x86)\PuTTY\plink.exe"
:havep

cd /d "%~dp0..\server"

echo [1/4] 交叉编译 linux/amd64 ...
set GOOS=linux
set GOARCH=amd64
go build -trimpath -ldflags="-s -w" -o "%~dp0..\dist\doh-proxy" .
if errorlevel 1 (
    echo [error] 编译失败
    exit /b 1
)

echo [2/4] 上传二进制 ...
%PSCP% -P %PORT% -pw %JP_SSH_PASS% -batch "%~dp0..\dist\doh-proxy" %USER%@%HOST%:/tmp/doh-proxy
if errorlevel 1 (
    echo [error] 上传失败
    exit /b 1
)

echo [3/4] 安装并重启服务 ...
%PLINK% -ssh -P %PORT% -batch -pw %JP_SSH_PASS% %USER%@%HOST% "mkdir -p %REMOTE_DIR% && install -m 0755 /tmp/doh-proxy %REMOTE_DIR%/doh-proxy && systemctl restart doh-proxy && sleep 1 && systemctl is-active doh-proxy"
if errorlevel 1 (
    echo [error] 远端安装/重启失败
    exit /b 1
)

echo [4/4] 健康检查 ...
%PLINK% -ssh -P %PORT% -batch -pw %JP_SSH_PASS% %USER%@%HOST% "curl -sS -m 5 http://127.0.0.1:8080/healthz; echo"

echo.
echo 完成。公网验证：curl http://%HOST%:8080/healthz
pause
endlocal
exit /b 0
