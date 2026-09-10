# Start the full local demo: 3 mock upstreams + relayd + Flutter client.
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
  $s = Invoke-RestMethod -Headers @{Authorization='Bearer sk-local-change-me'} http://127.0.0.1:8081/admin/summary
  Write-Host "relayd up: relay=:8080 admin=:8081 total=$($s.total)"
} catch {
  Write-Host "relayd not responding: $_"; exit 1
}

$client = "$root\frontend\build\windows\x64\runner\Release\relayd_client.exe"
if (Test-Path $client) {
  Start-Process -FilePath $client -WorkingDirectory (Split-Path $client)
  Write-Host 'client started (Settings token: sk-local-change-me)'
}
