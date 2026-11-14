@echo off
chcp 65001 >nul
cd /d %~dp0
mcnode.exe --setup
pause
