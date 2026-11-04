# Installs moor as a Windows service on a CI runner, deploys an app, restarts the service, upgrades it and goes back,
# checking that the app kept running and who the server runs as. The runner is thrown away afterwards.
$ErrorActionPreference = 'Stop'
$failed = 0
function Check([string]$what, [bool]$ok, [string]$detail = '') {
    if ($ok) { Write-Host "ok    $what" } else { Write-Host "FAIL  $what $detail"; $script:failed++ }
}
function Must {
    $tool, $arguments = $args
    & $tool @arguments
    if ($LASTEXITCODE -ne 0) { throw "$tool $arguments exited with $LASTEXITCODE" }
}
function Serve([string]$hostName) {
    $body = & curl.exe -sS -m 10 -H "Host: $hostName" http://127.0.0.1:8080/ 2>&1
    return ($body | Out-String).Trim()
}
function ServerVersion {
    $token = Get-Content (Join-Path $data 'keys\admin-token') -Raw
    $body = & curl.exe -sS -m 10 -H "Authorization: Bearer $($token.Trim())" -H 'Host: moorage.localhost' http://127.0.0.1:8080/api/v1/server
    return ($body | ConvertFrom-Json).version
}
function AppPid { return ((& $moor ps --app svc --json) | ConvertFrom-Json)[0].pid }

$work = Join-Path $env:RUNNER_TEMP 'moorage-service'
New-Item -ItemType Directory -Force $work | Out-Null
$dir = Join-Path $env:ProgramFiles 'MoorageCheck'
New-Item -ItemType Directory -Force $dir | Out-Null
$moor = Join-Path $dir 'moor.exe'
Must go build -o $moor ./cmd/moor

Must $moor server init --preset local
$config = Join-Path $env:ProgramData 'Moorage\server.toml'
Check 'it is configured for the whole computer' (Test-Path $config)
$data = (Select-String -Path $config -Pattern '^data_dir\s*=\s*[''"](.*)[''"]').Matches[0].Groups[1].Value -replace '\\\\', '\'
Must $moor server install
Must $moor server status

$service = Get-CimInstance Win32_Service -Filter "Name='Moorage'"
Check 'the service runs as an account of its own' ($service.StartName -eq 'NT SERVICE\Moorage') $service.StartName
Check 'it starts with the computer' ($service.StartMode -eq 'Auto') $service.StartMode
$acl = (Get-Acl (Join-Path $data 'keys\master.key')).Access | ForEach-Object { $_.IdentityReference.Value }
Check 'nobody else can read the master key' (-not ($acl | Where-Object { $_ -match 'Users|Everyone' })) ($acl -join ', ')

# An app, as an ordinary program.
$app = Join-Path $work 'app'
New-Item -ItemType Directory -Force $app | Out-Null
@'
package main

import (
	"fmt"
	"net/http"
	"os"
)

func main() {
	http.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "from the app ", os.Getenv("MOORAGE_APP")) })
	http.ListenAndServe("127.0.0.1:"+os.Getenv("PORT"), nil)
}
'@ | Set-Content (Join-Path $app 'main.go')
@'
[app]
runtime = "binary"

[processes.web]
command = "./server.exe"
health = { path = "/" }

[resources]
memory = "256MB"
cpu = 1.0
pids = 20
'@ | Set-Content (Join-Path $app 'moorage.toml')
Push-Location $app
Must go mod init svc
Must go build -o server.exe .
Pop-Location
Remove-Item (Join-Path $app 'main.go'), (Join-Path $app 'go.mod')
Must $moor deploy $app --app svc
Check 'the app serves' ((Serve 'svc.localhost') -match 'from the app') (Serve 'svc.localhost')
$appPid = AppPid
$process = Get-CimInstance Win32_Process -Filter "ProcessId=$appPid"
Check 'the app is running' ($null -ne $process)
$data_written = & $moor run --app svc -- 'echo written > "$MOORAGE_DATA_DIR\from-a-command.txt"'
Check 'a command runs as the app' (Test-Path (Join-Path $data 'data\svc\production\from-a-command.txt'))

Write-Host '--- restarting'
Must $moor server restart
Must $moor server status
Check 'the app kept running through a restart' ((AppPid) -eq $appPid) "$(AppPid) and not $appPid"
Check 'and goes on serving' ((Serve 'svc.localhost') -match 'from the app') (Serve 'svc.localhost')

Write-Host '--- upgrading'
$startedAs = ServerVersion
$arch = if ($env:PROCESSOR_ARCHITECTURE -eq 'ARM64') { 'arm64' } else { 'amd64' }
$asset = "moor-windows-$arch.exe"
$good = Join-Path $work 'good'
New-Item -ItemType Directory -Force $good | Out-Null
Must go build -ldflags '-X github.com/robert-mcdermott/moorage/internal/buildinfo.Version=99.0.0' -o (Join-Path $good $asset) ./cmd/moor
$sum = (Get-FileHash (Join-Path $good $asset) -Algorithm SHA256).Hash.ToLower()
"$sum  $asset" | Set-Content (Join-Path $good 'SHA256SUMS')

$altered = Join-Path $work 'altered'
New-Item -ItemType Directory -Force $altered | Out-Null
Copy-Item (Join-Path $good '*') $altered
Add-Content (Join-Path $altered $asset) 'and something more'
& $moor server upgrade --from $altered
Check 'a release that has been changed is refused' ($LASTEXITCODE -ne 0)
Check 'and changes nothing' ((ServerVersion) -eq $startedAs)

Must $moor server upgrade --from $good
Must $moor server status
Check 'the upgrade took' ((ServerVersion) -eq '99.0.0') (ServerVersion)
Check 'the previous binary was kept' (Test-Path (Join-Path $dir 'moor.previous.exe'))
Check 'the app was not restarted' ((AppPid) -eq $appPid) "$(AppPid) and not $appPid"
Check 'and goes on serving' ((Serve 'svc.localhost') -match 'from the app') (Serve 'svc.localhost')

Write-Host '--- going back'
Must $moor server upgrade --rollback
Must $moor server status
Check 'going back took' ((ServerVersion) -eq $startedAs) (ServerVersion)
Check 'the app was not restarted' ((AppPid) -eq $appPid) "$(AppPid) and not $appPid"

Write-Host '--- the hosts file'
Must $moor server resolver install
$hostsFile = Join-Path $env:SystemRoot 'System32\drivers\etc\hosts'
Check 'the hosts file has the app' ((Get-Content $hostsFile -Raw) -match 'svc\.localhost')
Must $moor server resolver remove
Check 'and it is taken out again' (-not ((Get-Content $hostsFile -Raw) -match 'svc\.localhost'))

Write-Host '--- uninstalling'
Must $moor server uninstall
Check 'the service has gone' ($null -eq (Get-Service Moorage -ErrorAction SilentlyContinue))
$still = & curl.exe -sS -m 2 "http://127.0.0.1:8080/" 2>&1
if ($failed -gt 0) { Write-Host "$failed failed"; exit 1 }
Write-Host 'every check passed'
exit 0
