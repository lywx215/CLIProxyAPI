@echo off
if "%~1"=="" (
    python "%~dp0scripts\vps\deploy.py" deploy --password
) else (
    python "%~dp0scripts\vps\deploy.py" %*
)
exit /b %ERRORLEVEL%
