[CmdletBinding()]
param(
  [string]$InstallDir = (Join-Path $HOME ".local\bin")
)

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

$repoRoot = Split-Path -Parent $PSScriptRoot
$backendDir = Join-Path $repoRoot "backend"
$packagePath = Join-Path $repoRoot "frontend\package.json"
$cacheRoot = if ($env:LOCALAPPDATA) {
  Join-Path $env:LOCALAPPDATA "aoagents\agent-orchestrator\bin"
} else {
  Join-Path $HOME ".cache\aoagents\agent-orchestrator\bin"
}
$buildPath = Join-Path $cacheRoot "ao.exe"
$installPath = Join-Path $InstallDir "ao.exe"

if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
  throw "go is required to build ao"
}
if (-not (Get-Command git -ErrorAction SilentlyContinue)) {
  throw "git is required to stamp the ao source build"
}

$version = (Get-Content -LiteralPath $packagePath -Raw | ConvertFrom-Json).version
$commit = (& git -C $repoRoot rev-parse --short=12 HEAD).Trim()
if ($LASTEXITCODE -ne 0 -or -not $commit) {
  throw "could not determine the current Git commit"
}
$buildDate = [DateTime]::UtcNow.ToString("yyyy-MM-ddTHH:mm:ssZ")
$cliPackage = "github.com/aoagents/agent-orchestrator/backend/internal/cli"
$ldflags = @(
  "-X $cliPackage.Version=$version-source"
  "-X $cliPackage.Commit=$commit"
  "-X $cliPackage.Date=$buildDate"
) -join " "

New-Item -ItemType Directory -Path $cacheRoot -Force | Out-Null
Push-Location $backendDir
try {
  & go build -trimpath -ldflags $ldflags -o $buildPath ./cmd/ao
  if ($LASTEXITCODE -ne 0) {
    throw "go build failed with exit code $LASTEXITCODE"
  }
} finally {
  Pop-Location
}

New-Item -ItemType Directory -Path $InstallDir -Force | Out-Null
Copy-Item -LiteralPath $buildPath -Destination $installPath -Force

$reportedVersion = (& $installPath version).Trim()
if ($LASTEXITCODE -ne 0) {
  throw "installed ao failed its version check with exit code $LASTEXITCODE"
}

Write-Output "Built $buildPath"
Write-Output "Installed $installPath"
Write-Output $reportedVersion
