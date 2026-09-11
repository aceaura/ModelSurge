# Start the local demo: mock upstreams + relayd.
$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent $PSScriptRoot
Set-Location $root

Get-Process relaymock, relayd -ErrorAction SilentlyContinue | Stop-Process -Force
Start-Sleep -Seconds 1

Start-Process -FilePath "$root\relaymock.exe" -ArgumentList '-config','demo\relaymock.yaml' `
  -RedirectStandardOutput 'demo\relaymock.log' -RedirectStandardError 'demo\relaymock.err.log' -WindowStyle Hidden
Start-Process -FilePath "$root\relayd.exe" -ArgumentList '-config','relayd.yaml' `
  -RedirectStandardOutput 'relayd.log' -RedirectStandardError 'relayd.err.log' -WindowStyle Hidden
Start-Sleep -Seconds 1

try {
  $null = Invoke-WebRequest http://127.0.0.1:8080/health
  Write-Host "relayd up: http://127.0.0.1:8080 (api key: sk-local-change-me)"
} catch {
  Write-Host "relayd not responding: $_"; exit 1
}
