@echo off
setlocal
rem Copyright The SetDrift Authors
rem SPDX-License-Identifier: Apache-2.0

rem Runs setdrift helm-postrender under whichever name SetDrift was installed
rem as: setdrift.exe (a release binary) or kubectl-setdrift.exe (a kubectl
rem plugin, e.g. through krew). Arguments are Helm's --post-renderer-args.
rem Search PATH only: cmd would otherwise run a setdrift.exe from the
rem current folder first. setlocal keeps SETDRIFT out of the caller.
set "SETDRIFT="
for %%N in (setdrift.exe kubectl-setdrift.exe) do (
  if not defined SETDRIFT set "SETDRIFT=%%~$PATH:N"
)
if not defined SETDRIFT (
  echo setdrift-chart-repo: neither setdrift nor kubectl-setdrift is on PATH; install SetDrift first 1>&2
  exit /b 9009
)
"%SETDRIFT%" helm-postrender %*
exit /b %errorlevel%
