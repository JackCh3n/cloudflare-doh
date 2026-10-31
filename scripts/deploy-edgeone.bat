@echo off
chcp 65001 >nul
setlocal enabledelayedexpansion
rem ============================================================
rem  deploy-edgeone.bat - 一键部署到腾讯 EdgeOne Pages
rem  说明：
rem    1) 若存在 .edgeone\.token 则用该 API Token 部署（推荐，免交互）
rem    2) 否则走浏览器登录
rem    3) 项目名固定为 cloudflare-doh，可用第一个参数覆盖
rem ============================================================

cd /d "%~dp0.."
set PROJECT=%~1
if "%PROJECT%"=="" set PROJECT=cloudflare-doh

echo [1/3] 检查 EdgeOne CLI...
where edgeone >nul 2>&1
if errorlevel 1 (
    echo       未安装，正在安装 edgeone CLI...
    call npm install -g edgeone@latest
    if errorlevel 1 (
        echo [error] EdgeOne CLI 安装失败，请检查网络后重试。
        pause
        exit /b 1
    )
)

echo [2/3] 部署项目 %PROJECT% ...
set TOKEN=
if exist ".edgeone\.token" (
    set /p TOKEN=<".edgeone\.token"
    echo       检测到 .edgeone\.token，使用 Token 部署。
)

set PAGES_SOURCE=skills
if "!TOKEN!"=="" (
    call edgeone makers deploy -n %PROJECT% --json
) else (
    call edgeone makers deploy -n %PROJECT% -t !TOKEN! --json
)

if errorlevel 1 (
    echo.
    echo [error] 部署失败，请查看上方 CLI 报错信息。
    pause
    exit /b 1
)

echo [3/3] 部署完成。
echo       预览域名需带上 eo_token 参数才能访问，完整地址见上方 JSON 中的 url 字段。
echo.
pause
endlocal
exit /b 0
