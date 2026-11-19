# Copyright The SetDrift Authors
# SPDX-License-Identifier: Apache-2.0

# Installs the setdrift CLI from the latest GitHub release (or $env:SETDRIFT_VERSION).
# In PowerShell on Windows:
#
#   irm https://raw.githubusercontent.com/setdrift/setdrift-engine/main/scripts/install.ps1 | iex
#
# It downloads the archive for this OS and CPU, checks it against the
# release's checksums.txt, installs setdrift to $env:SETDRIFT_INSTALL_DIR,
# else %LOCALAPPDATA%\Programs\setdrift (~/.local/bin outside Windows), and
# adds that directory to your user PATH. Works in Windows PowerShell 5.1 and
# PowerShell 7 on Windows, macOS and Linux.

# `irm | iex` runs a script in the caller's session: the script block keeps
# its variables and preferences out of it. Only PATH changes outlive it.
# Errors are caught and printed, not thrown: a throw out of iex can crash
# a non-interactive PowerShell instead of showing the message.
& {
try {
$ErrorActionPreference = 'Stop'
# Windows PowerShell 5.1 defaults to TLS 1.0, which GitHub refuses.
[Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12
# Invoke-WebRequest's progress bar makes downloads many times slower in 5.1.
$ProgressPreference = 'SilentlyContinue'

$repo = 'setdrift/setdrift-engine'

function Fail([string]$msg) { throw "setdrift install: $msg" }

# $IsWindows exists from PowerShell 6; Windows PowerShell 5.1 is Windows.
$onWindows = ($PSVersionTable.PSEdition -eq 'Desktop') -or $IsWindows
if ($onWindows) { $os = 'windows' }
elseif ($IsMacOS) { $os = 'darwin' }
elseif ($IsLinux) { $os = 'linux' }
else { Fail "unsupported OS: download a release from https://github.com/$repo/releases" }

if ($onWindows) {
  # PROCESSOR_ARCHITEW6432 is the real CPU when 32-bit PowerShell runs on 64-bit Windows.
  $cpu = if ($env:PROCESSOR_ARCHITEW6432) { $env:PROCESSOR_ARCHITEW6432 } else { $env:PROCESSOR_ARCHITECTURE }
} else {
  $cpu = (uname -m)
}
switch -regex ($cpu) {
  '^(AMD64|x86_64|amd64)$' { $arch = 'amd64' }
  '^(ARM64|arm64|aarch64)$' { $arch = 'arm64' }
  default { Fail "unsupported CPU ${cpu}: download a release from https://github.com/$repo/releases" }
}

$version = $env:SETDRIFT_VERSION
if (-not $version) {
  # The latest release's tag, from the API (no redirect handling needed in 5.1).
  try {
    $version = (Invoke-RestMethod -UseBasicParsing "https://api.github.com/repos/$repo/releases/latest").tag_name
  } catch {
    Fail "no release found at https://github.com/$repo/releases ($($_.Exception.Message))"
  }
}
if (-not $version.StartsWith('v')) { $version = "v$version" }

$ext = if ($os -eq 'windows') { 'zip' } else { 'tar.gz' }
$archive = "setdrift_$($version.Substring(1))_${os}_${arch}.$ext"
# SETDRIFT_DOWNLOAD_URL: a mirror of the release assets (and for testing).
$base = if ($env:SETDRIFT_DOWNLOAD_URL) { $env:SETDRIFT_DOWNLOAD_URL } else { "https://github.com/$repo/releases/download/$version" }

$tmp = Join-Path ([IO.Path]::GetTempPath()) ("setdrift-" + [Guid]::NewGuid())
New-Item -ItemType Directory -Path $tmp | Out-Null
try {
  Write-Host "Downloading setdrift $version for $os/$arch"
  # WebClient, not Invoke-WebRequest -OutFile: a failed -OutFile download can
  # crash PowerShell 7.4 from a background thread, past any catch.
  $web = New-Object Net.WebClient
  try {
    $web.DownloadFile("$base/$archive", (Join-Path $tmp $archive))
    $web.DownloadFile("$base/checksums.txt", (Join-Path $tmp 'checksums.txt'))
  } catch {
    $e = $_.Exception
    while ($e.InnerException) { $e = $e.InnerException }
    Fail "download failed from ${base}: $($e.Message)"
  } finally {
    $web.Dispose()
  }

  $want = $null
  foreach ($line in Get-Content (Join-Path $tmp 'checksums.txt')) {
    $fields = $line -split '\s+'
    if ($fields.Count -ge 2 -and $fields[1] -eq $archive) { $want = $fields[0].ToLower() }
  }
  if (-not $want) { Fail "$archive is not in checksums.txt" }
  $got = (Get-FileHash -Algorithm SHA256 (Join-Path $tmp $archive)).Hash.ToLower()
  if ($got -ne $want) { Fail "checksum mismatch for ${archive}: got $got, want $want" }

  $exe = if ($os -eq 'windows') { 'setdrift.exe' } else { 'setdrift' }
  if ($ext -eq 'zip') {
    Expand-Archive -Path (Join-Path $tmp $archive) -DestinationPath (Join-Path $tmp 'x')
  } else {
    New-Item -ItemType Directory -Path (Join-Path $tmp 'x') | Out-Null
    tar -xzf (Join-Path $tmp $archive) -C (Join-Path $tmp 'x') setdrift
    if ($LASTEXITCODE -ne 0) { Fail "could not extract $archive" }
  }

  $dir = $env:SETDRIFT_INSTALL_DIR
  if (-not $dir) {
    $dir = if ($onWindows) { Join-Path $env:LOCALAPPDATA 'Programs\setdrift' } else { Join-Path $HOME '.local/bin' }
  }
  New-Item -ItemType Directory -Force -Path $dir | Out-Null
  $dest = Join-Path $dir $exe
  Copy-Item -Force (Join-Path (Join-Path $tmp 'x') $exe) $dest
  if (-not $onWindows) { chmod 0755 $dest }

  Write-Host "Installed $(& $dest version) to $dest"

  $sep = [IO.Path]::PathSeparator
  if (-not (($env:PATH -split [regex]::Escape($sep)) -contains $dir)) {
    if ($onWindows) {
      # The user's PATH, for new terminals; and this session's.
      $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
      if (-not (($userPath -split ';') -contains $dir)) {
        [Environment]::SetEnvironmentVariable('Path', ($(if ($userPath) { "$userPath;" } else { '' }) + $dir), 'User')
      }
      $env:PATH = "$env:PATH;$dir"
      Write-Host "Added $dir to your user PATH (open a new terminal for other windows)."
    } else {
      Write-Host "Add $dir to your PATH, e.g.: export PATH=`"${dir}:`$PATH`""
    }
  }
  Write-Host 'Next: setdrift export   (reads your current kubeconfig context; changes nothing)'
} finally {
  Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
}
} catch {
  $msg = $_.Exception.Message
  if (-not $msg.StartsWith('setdrift install:')) { $msg = "setdrift install: $msg" }
  Write-Host $msg -ForegroundColor Red
  # For scripts that check: `irm … | iex; if ($LASTEXITCODE) { … }`.
  $global:LASTEXITCODE = 1
}
}
