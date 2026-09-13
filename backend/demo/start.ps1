# Start the local demo: mock providers + upstream + relay.
# 前置：cd backend && go build -o upstream.exe ./cmd/upstream && go build -o relay.exe ./cmd/relay && go build -o relaymock.exe ./cmd/relaymock
$ErrorActionPreference = 'Stop'
$root = Split-Path $PSScriptRoot -Parent # backend/
Set-Location $root

Get-Process relaymock, upstream, relay -ErrorAction SilentlyContinue | Stop-Process -Force
Start-Sleep -Seconds 1

Start-Process -FilePath "$root\relaymock.exe" -ArgumentList '-config','demo\relaymock.yaml' `
  -RedirectStandardOutput 'demo\relaymock.log' -RedirectStandardError 'demo\relaymock.err.log' -WindowStyle Hidden
Start-Process -FilePath "$root\upstream.exe" -ArgumentList '-config','demo\upstream.yaml' `
  -RedirectStandardOutput 'upstream.log' -RedirectStandardError 'upstream.err.log' -WindowStyle Hidden

$upstreamReady = $false
for ($i = 0; $i -lt 50; $i++) {
  try {
    $null = Invoke-WebRequest 'http://127.0.0.1:18100/internal/v1/health' -Headers @{ Authorization = 'Bearer local-service-key' }
    $upstreamReady = $true
    break
  } catch { Start-Sleep -Milliseconds 100 }
}
if (-not $upstreamReady) { throw 'upstream did not become ready' }

function Register-DemoAccount([string]$name, [string]$protocol, [int]$port, [string]$apiKey, [string]$model) {
  $body = @{
    name     = $name
    type     = 'api-key'
    protocol = $protocol
    base_url = "http://127.0.0.1:$port"
    api_key  = $apiKey
    models   = @{ $model = $model }
  } | ConvertTo-Json
  try {
    Invoke-RestMethod -Method Post -Uri 'http://127.0.0.1:18100/admin/accounts' `
      -Headers @{ 'X-Admin-Key' = 'change-upstream-admin-key' } -ContentType 'application/json' -Body $body | Out-Null
  } catch { }
}
Register-DemoAccount 'st-a' 'openai-chat' 9001 'sk-mock-a' 'gpt-4o'
Register-DemoAccount 'cl-01' 'anthropic' 9003 'sk-mock-c' 'claude-sonnet-4'

Start-Process -FilePath "$root\relay.exe" -ArgumentList '-config','demo\relay.yaml' `
  -RedirectStandardOutput 'relay.log' -RedirectStandardError 'relay.err.log' -WindowStyle Hidden
for ($i = 0; $i -lt 50; $i++) {
  try {
    $null = Invoke-WebRequest 'http://127.0.0.1:18099/health'
    Write-Host 'ModelSurge up: http://127.0.0.1:18099 (api key: change-client-key)'
    exit 0
  } catch { Start-Sleep -Milliseconds 100 }
}
Write-Host 'relay did not become ready'
exit 1
