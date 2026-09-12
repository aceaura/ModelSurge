# Start the local demo: mock upstreams + relayd.
# 前置：cd backend && go build -o relayd.exe ./cmd/relayd && go build -o relaymock.exe ./cmd/relaymock
$ErrorActionPreference = 'Stop'
$root = Split-Path $PSScriptRoot -Parent # backend/
Set-Location $root

Get-Process relaymock, relayd -ErrorAction SilentlyContinue | Stop-Process -Force
Start-Sleep -Seconds 1

Start-Process -FilePath "$root\relaymock.exe" -ArgumentList '-config','demo\relaymock.yaml' `
  -RedirectStandardOutput 'demo\relaymock.log' -RedirectStandardError 'demo\relaymock.err.log' -WindowStyle Hidden
Start-Process -FilePath "$root\relayd.exe" -ArgumentList '-config','relayd.yaml' `
  -RedirectStandardOutput 'relayd.log' -RedirectStandardError 'relayd.err.log' -WindowStyle Hidden
Start-Sleep -Seconds 1

# mock 账号注册（SQLite-only：yaml 不配上游；已存在 409 忽略）
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
    Invoke-RestMethod -Method Post -Uri 'http://127.0.0.1:8080/admin/accounts' `
      -Headers @{ 'X-Admin-Key' = 'sk-admin-change-me' } -ContentType 'application/json' -Body $body | Out-Null
  } catch { }
}
Register-DemoAccount 'st-a' 'openai-chat' 9001 'sk-mock-a' 'gpt-4o'
Register-DemoAccount 'cl-01' 'anthropic' 9003 'sk-mock-c' 'claude-sonnet-4'

try {
  $null = Invoke-WebRequest http://127.0.0.1:8080/health
  Write-Host "relayd up: http://127.0.0.1:8080 (api key: sk-local-change-me)"
} catch {
  Write-Host "relayd not responding: $_"; exit 1
}
