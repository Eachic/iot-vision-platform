param(
  [string]$BaseUrl = "http://127.0.0.1:5173",
  [string]$Username = "admin",
  [string]$Password = "admin123456",
  [string]$Duration = "60s",
  [int]$Concurrency = 50,
  [string]$Hey = "hey",
  [switch]$NoWarmup
)

$ErrorActionPreference = "Stop"

function Join-Url {
  param(
    [string]$Base,
    [string]$Path
  )
  return $Base.TrimEnd("/") + "/" + $Path.TrimStart("/")
}

function Invoke-Hey {
  param(
    [string]$Name,
    [string]$Url,
    [string]$Token
  )
  Write-Host ""
  Write-Host "============================================================"
  Write-Host "Query pressure: $Name"
  Write-Host "URL: $Url"
  Write-Host "Duration: $Duration  Concurrency: $Concurrency"
  Write-Host "============================================================"
  & $Hey -z $Duration -c $Concurrency -H "Authorization: Bearer $Token" $Url
}

try {
  & $Hey -n 1 -c 1 (Join-Url $BaseUrl "/api/health") | Out-Null
} catch {
  Write-Error "hey is not available or gateway is unreachable. Install hey and confirm $BaseUrl is running."
  exit 1
}

$loginUrl = Join-Url $BaseUrl "/api/auth/login"
$loginBody = @{
  username = $Username
  password = $Password
} | ConvertTo-Json -Compress

Write-Host "Login: $loginUrl"
$login = Invoke-RestMethod -Method Post -Uri $loginUrl -ContentType "application/json" -Body $loginBody
$token = [string]$login.token
if ([string]::IsNullOrWhiteSpace($token)) {
  Write-Error "Login response did not include token."
  exit 1
}

$headers = @{ Authorization = "Bearer $token" }
$targets = @(
  @{ Name = "stats Redis cache"; Url = Join-Url $BaseUrl "/api/stats" },
  @{ Name = "tasks status Redis cache"; Url = Join-Url $BaseUrl "/api/tasks/status" },
  @{ Name = "devices Redis cache"; Url = Join-Url $BaseUrl "/api/devices" },
  @{ Name = "images MySQL list"; Url = Join-Url $BaseUrl "/api/images?page=1&page_size=60&sort_by=created_at&sort_order=desc" }
)

if (-not $NoWarmup) {
  Write-Host "Warm up query cache..."
  foreach ($target in $targets) {
    try {
      Invoke-RestMethod -Method Get -Uri $target.Url -Headers $headers | Out-Null
    } catch {
      Write-Warning "Warmup failed for $($target.Name): $($_.Exception.Message)"
    }
  }
}

foreach ($target in $targets) {
  Invoke-Hey -Name $target.Name -Url $target.Url -Token $token
}

Write-Host ""
Write-Host "Done. Compare Requests/sec, Average, 95%, 99%, and non-2xx responses across endpoints."
