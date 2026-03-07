@echo off
chcp 65001 >nul
echo [1/3] 正在构建 Vue 前端...
cd /d "%~dp0..\proxyui"
call npm run build

echo [2/3] 正在复制前端产物至 Go 目录...
cd /d "%~dp0"
rmdir /s /q proxysocket\dist 2>nul
xcopy /e /i /y ..\proxyui\dist proxysocket\dist

echo [3/3] 开始跨平台静态编译...
set CGO_ENABLED=0

echo. & echo 正在编译 Windows 版本: proxy_man_win.exe ...
set GOOS=windows
set GOARCH=amd64
go build -ldflags="-w -s" -o proxy_man_win.exe main.go

echo. & echo 正在编译 Linux 版本: proxy_man_linux ...
set GOOS=linux
set GOARCH=amd64
go build -ldflags="-w -s" -o proxy_man_linux main.go

echo. & echo 编译完成！
pause
