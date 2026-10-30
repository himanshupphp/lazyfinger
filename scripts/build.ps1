# Builds lazy for every platform and packages it into dist\.
$ErrorActionPreference = "Stop"
Set-Location (Join-Path $PSScriptRoot "..")

$Version = if ($env:VERSION) { $env:VERSION } else { "dev" }
$Targets = "darwin/arm64","darwin/amd64","linux/amd64","linux/arm64","windows/amd64","windows/arm64"

Remove-Item -Recurse -Force dist -ErrorAction SilentlyContinue
New-Item -ItemType Directory dist | Out-Null
go mod tidy
if ($LASTEXITCODE -ne 0) { throw "go mod tidy failed" }

foreach ($t in $Targets) {
  $os, $arch = $t.Split("/")
  $name = "lazy-$Version-$os-$arch"
  $ext = if ($os -eq "windows") { ".exe" } else { "" }
  Write-Host "-> building $name"
  New-Item -ItemType Directory "dist/$name" | Out-Null
  $env:CGO_ENABLED = "0"; $env:GOOS = $os; $env:GOARCH = $arch
  go build -trimpath -ldflags "-s -w -X main.version=$Version" -o "dist/$name/lazy$ext" ./cmd/lazy
  if ($LASTEXITCODE -ne 0) { throw "build failed for $t" }
  Copy-Item README.md, LICENSE "dist/$name/"
  Compress-Archive -Path "dist/$name" -DestinationPath "dist/$name.zip"
  Remove-Item -Recurse -Force "dist/$name"
}
Remove-Item Env:GOOS, Env:GOARCH, Env:CGO_ENABLED

Write-Host "-> packaging source"
$src = Get-ChildItem -Force | Where-Object { $_.Name -notin @("dist", ".git", ".lazyfinger") }
Compress-Archive -Path $src.FullName -DestinationPath "dist/lazy-$Version-source.zip"

Write-Host "Done:"
Get-ChildItem dist | ForEach-Object { $_.Name }
