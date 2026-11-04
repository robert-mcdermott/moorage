# Runs install.ps1 for real on a CI runner, as a user would, against the published
# releases: piped into Invoke-Expression, onto the user PATH, and upgrading under a
# running server. Not for your own machine: it changes the user PATH.
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'

if ($env:CI -ne 'true') {
    throw "This changes the user PATH, and is meant for CI's throwaway runners"
}

$script = Join-Path (Split-Path $PSScriptRoot -Parent | Split-Path -Parent) 'install.ps1'
$work = Join-Path ([IO.Path]::GetTempPath()) ('moor-install-check-' + [guid]::NewGuid())
New-Item -ItemType Directory -Path $work | Out-Null
$port = 18096
$script:failures = 0
$server = $null

function Check([string]$What, [bool]$Passed) {
    if ($Passed) {
        Write-Host "ok    $What"
    } else {
        Write-Host "FAIL  $What"
        $script:failures++
    }
}

function Clear-Settings {
    foreach ($name in 'MOOR_VERSION', 'MOOR_INSTALL_DIR', 'MOOR_NO_PATH_UPDATE') {
        Remove-Item "env:$name" -ErrorAction SilentlyContinue
    }
}

# As `irm … | iex` does: the script's text, read into memory, never a script file.
function Install-Moor {
    Get-Content -Raw -LiteralPath $script | Invoke-Expression
}

function Get-Health {
    (Invoke-WebRequest -UseBasicParsing "http://127.0.0.1:$port/.moorage/health").Content
}

function Wait-Server {
    foreach ($i in 1..100) {
        try { Get-Health | Out-Null; return } catch { Start-Sleep -Milliseconds 100 }
    }
    throw 'The server did not start'
}

function Start-Server([string]$Moor) {
    Start-Process -FilePath $Moor -ArgumentList 'server', 'run', '--config', $config -PassThru -NoNewWindow `
        -RedirectStandardOutput (Join-Path $work 'server.out') -RedirectStandardError (Join-Path $work ("server." + [guid]::NewGuid() + ".err"))
}

$latest = (Invoke-WebRequest -UseBasicParsing 'https://github.com/robert-mcdermott/moorage/releases/latest').BaseResponse
$latestUrl = if ($latest.PSObject.Properties['ResponseUri']) { $latest.ResponseUri.AbsoluteUri } else { $latest.RequestMessage.RequestUri.AbsoluteUri }
$latest = ($latestUrl -split '/')[-1].TrimStart('v')
Write-Host "PowerShell $($PSVersionTable.PSVersion) on $([Environment]::OSVersion.VersionString)"
Write-Host "the latest release is $latest"
$expectedAsset = 'amd64'
if ([Environment]::GetEnvironmentVariable('PROCESSOR_ARCHITECTURE', 'Machine') -eq 'ARM64') { $expectedAsset = 'arm64' }

try {
    Write-Host '--- piped into Invoke-Expression, with nothing set'
    Clear-Settings
    Install-Moor
    $moor = Join-Path $env:LOCALAPPDATA 'Programs\Moorage\moor.exe'
    Check 'it is in Programs\Moorage' (Test-Path -LiteralPath $moor)
    $version = & $moor version
    Check 'it is the latest release' ($version -like "moor $latest *")
    Check "it is the file for this CPU ($expectedAsset)" ($version -like "*windows/$expectedAsset*")

    $userPath = [Microsoft.Win32.Registry]::CurrentUser.OpenSubKey('Environment').GetValue('Path', '', [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames)
    Check 'it is on the user PATH' ($userPath -like '*Programs\Moorage*')
    Check 'this session can find it' ($null -ne (Get-Command moor -ErrorAction SilentlyContinue))
    $inNew = & cmd /c "set `"PATH=$userPath;$env:SystemRoot\System32`" && moor version"
    Check 'a new terminal can find it' ($inNew -like "moor $latest *")
    $kind = [Microsoft.Win32.Registry]::CurrentUser.OpenSubKey('Environment').GetValueKind('Path')
    Check 'the PATH keeps its kind' ($kind -eq [Microsoft.Win32.RegistryValueKind]::ExpandString -or $kind -eq [Microsoft.Win32.RegistryValueKind]::String)

    $before = $userPath
    Install-Moor
    $after = [Microsoft.Win32.Registry]::CurrentUser.OpenSubKey('Environment').GetValue('Path', '', [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames)
    Check 'a second install adds nothing to the PATH' ($before -eq $after)

    Write-Host '--- a given release, then the latest, under a running server'
    $pinned = Join-Path $work 'pinned'
    $env:MOOR_INSTALL_DIR = $pinned
    $env:MOOR_NO_PATH_UPDATE = '1'
    $env:MOOR_VERSION = 'v0.2.0'
    Install-Moor
    $pinnedMoor = Join-Path $pinned 'moor.exe'
    Check 'it installs the release it is asked for' ((& $pinnedMoor version) -like 'moor 0.2.0 *')

    $config = Join-Path $work 'server.toml'
    $env:MOORAGE_CONFIG = $config
    & $pinnedMoor server init --preset local --config $config --data-dir (Join-Path $work 'data') | Out-Null
    (Get-Content -Raw $config).Replace('127.0.0.1:8080', "127.0.0.1:$port") -replace '(?m)^ssh .*$', 'ssh = ""' | Set-Content -NoNewline $config

    $server = Start-Server $pinnedMoor
    Wait-Server
    Check 'a server runs from it' ((Get-Health) -like '*"version":"0.2.0"*')

    Remove-Item env:MOOR_VERSION
    if ($latest -ne '0.2.0') {
        Install-Moor
        Check 'it replaces the running file' ((& $pinnedMoor version) -like "moor $latest *")
        Check 'the server goes on, with the old version' ((Get-Health) -like '*"version":"0.2.0"*')
        Check 'the old file waits aside' (@(Get-ChildItem $pinned -Filter 'moor.previous.*.exe').Count -eq 1)
        Stop-Process -Id $server.Id -Force
        $server.WaitForExit()
        $server = $null
        Install-Moor
        Check 'the next install clears it away' (@(Get-ChildItem $pinned -Filter 'moor.previous.*.exe').Count -eq 0)
    } else {
        # The first version of the installer: until a release follows 0.2.0, what
        # it can show is moving the running file aside for a copy of itself.
        Install-Moor
        Check 'it replaces the running file' ((& $pinnedMoor version) -like 'moor 0.2.0 *')
        Check 'the server goes on' ((Get-Health) -like '*"version":"0.2.0"*')
        Stop-Process -Id $server.Id -Force
        $server.WaitForExit()
        $server = $null
    }
    Remove-Item env:MOORAGE_CONFIG

    Write-Host '--- nothing replaces it, if what it downloads is wrong'
    $env:MOOR_VERSION = 'v0.0.1'
    $failed = $false
    try { Install-Moor } catch { $failed = $true }
    Check 'a release that does not exist fails' $failed
    Check 'and what was there stays' ((& $pinnedMoor version) -like "moor $latest *")
    Clear-Settings
} finally {
    if ($server) {
        Stop-Process -Id $server.Id -Force -ErrorAction SilentlyContinue
    }
}

if ($script:failures -ne 0) {
    throw "$($script:failures) checks failed"
}
Write-Host "install.ps1 works, in PowerShell $($PSVersionTable.PSVersion)"
