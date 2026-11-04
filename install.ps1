#Requires -Version 5.1

<#
.SYNOPSIS
    Installs moor, the Moorage server and command line, on Windows.

.DESCRIPTION
    Downloads the release binary for this computer's CPU and SHA256SUMS, checks
    the SHA-256 checksum and the binary's version, and only then puts it in place
    as moor.exe. The user PATH is updated unless -NoPathUpdate is given. It needs
    no administrator rights, and does not start a server.

    Run through Invoke-Expression, the script never touches disk as a script
    file, so the PowerShell execution policy does not apply. Files downloaded
    with Invoke-WebRequest carry no mark of the web, so SmartScreen does not
    ask about the installed executable.

.EXAMPLE
    irm https://raw.githubusercontent.com/robert-mcdermott/moorage/main/install.ps1 | iex

.EXAMPLE
    & ([scriptblock]::Create((irm https://raw.githubusercontent.com/robert-mcdermott/moorage/main/install.ps1))) -Version v0.2.0

.EXAMPLE
    powershell -ExecutionPolicy Bypass -File .\install.ps1 -InstallDir "$HOME\bin"
#>

[CmdletBinding()]
param(
    [string]$Version = $env:MOOR_VERSION,
    [string]$InstallDir = $env:MOOR_INSTALL_DIR,
    [string]$Repository = $env:MOOR_REPOSITORY,
    [string]$Architecture = $env:MOOR_ARCH,
    [switch]$NoPathUpdate
)

function Get-MoorArchitecture {
    <#
        Reports the native Windows CPU architecture as 'amd64' or 'arm64'.

        The machine-scoped PROCESSOR_ARCHITECTURE value comes first because it is
        from the registry and describes the real hardware even when PowerShell
        itself is emulated: Windows PowerShell 5.1 has no native ARM64 build, so on
        Windows 11 ARM64 it runs as emulated x64. Every probe is optional: a host
        that lacks one must not fail the install.
    #>
    [CmdletBinding()]
    param()

    $probes = @(
        { [Environment]::GetEnvironmentVariable('PROCESSOR_ARCHITECTURE', 'Machine') },
        { $env:PROCESSOR_ARCHITEW6432 },
        { [Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString() },
        { $env:PROCESSOR_ARCHITECTURE }
    )

    $seen = @()
    foreach ($probe in $probes) {
        $value = $null
        try { $value = & $probe } catch { $value = $null }
        if (-not $value) { continue }

        $value = ([string]$value).Trim()
        if ($seen -notcontains $value) { $seen += $value }

        switch -Regex ($value) {
            '^(arm64|aarch64)$'          { return 'arm64' }
            '^(amd64|x64|x86_64|em64t)$' { return 'amd64' }
        }
    }

    $observed = if ($seen) { $seen -join ', ' } else { 'nothing' }
    throw "Could not determine the Windows CPU architecture (the probes reported $observed). Run it again with -Architecture amd64 or -Architecture arm64, or set MOOR_ARCH."
}

function Get-MoorAsset {
    param([string]$Architecture)

    if (-not $Architecture) { $Architecture = Get-MoorArchitecture }

    switch -Regex ($Architecture.Trim()) {
        '^(arm64|aarch64)$'          { return 'moor-windows-arm64.exe' }
        '^(amd64|x64|x86_64|em64t)$' { return 'moor-windows-amd64.exe' }
        default { throw "Unsupported Windows architecture: $Architecture (expected amd64 or arm64)" }
    }
}

function Resolve-MoorVersion {
    param([Parameter(Mandatory = $true)][string]$Value)

    if ($Value -eq 'latest') { return $Value }
    if ($Value -match '^[0-9]') { $Value = "v$Value" }
    if ($Value -notmatch '^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z]+([.-][0-9A-Za-z]+)*)?$') {
        throw "Invalid release version: $Value (expected a tag such as v0.2.0)"
    }
    return $Value
}

function Get-MoorExpectedChecksum {
    param(
        [Parameter(Mandatory = $true)][string]$ManifestPath,
        [Parameter(Mandatory = $true)][string]$Asset
    )

    $escaped = [regex]::Escape($Asset)
    $entries = @(
        Get-Content -LiteralPath $ManifestPath | ForEach-Object {
            if ($_ -match "^(?<hash>[0-9A-Fa-f]{64})[ `t]+[*]?$escaped$") {
                $Matches['hash'].ToLowerInvariant()
            }
        }
    )
    if ($entries.Count -ne 1) {
        throw "SHA256SUMS does not contain exactly one entry for $Asset"
    }
    return $entries[0]
}

function Publish-MoorEnvironmentChange {
    <#
        Tells Explorer that the environment changed, so that terminals started
        from the Start menu see the new PATH without signing out. Best effort:
        Add-Type is unavailable in constrained language mode.
    #>
    try {
        if (-not ('Moorage.NativeMethods' -as [type])) {
            Add-Type -Namespace 'Moorage' -Name 'NativeMethods' -MemberDefinition @'
[System.Runtime.InteropServices.DllImport("user32.dll", SetLastError = true, CharSet = System.Runtime.InteropServices.CharSet.Auto)]
public static extern System.IntPtr SendMessageTimeout(System.IntPtr hWnd, uint Msg, System.IntPtr wParam, string lParam, uint fuFlags, uint uTimeout, out System.UIntPtr lpdwResult);
'@
        }
        $result = [UIntPtr]::Zero
        # HWND_BROADCAST, WM_SETTINGCHANGE, SMTO_ABORTIFHUNG, 5 s timeout.
        [void][Moorage.NativeMethods]::SendMessageTimeout([IntPtr]0xffff, 0x1A, [IntPtr]::Zero, 'Environment', 2, 5000, [ref]$result)
    } catch {
        Write-Verbose "Could not broadcast the environment change: $($_.Exception.Message)"
    }
}

function Get-MoorIdentity {
    <#
        Runs an executable's version command and returns its first line, or throws
        an error saying what happened.

        The program's own output is the signal, not $LASTEXITCODE, which a fresh
        session has never set. When Windows never starts the process, because of
        an elevation prompt or an application-control policy, there is no output,
        no error and no exit code at all. A staged name with a word such as
        "install" or "update" in it would cause that prompt, for an unsigned
        program: moor.staged.<guid>.exe has none.
    #>
    param([Parameter(Mandatory = $true)][string]$Path)

    Set-Variable -Name LASTEXITCODE -Scope Global -Value $null -ErrorAction SilentlyContinue

    $output = $null
    try {
        $output = & $Path version 2>&1
    } catch {
        throw "The downloaded binary could not be started ($Path): $($_.Exception.Message)"
    }
    $exitCode = Get-Variable -Name LASTEXITCODE -Scope Global -ValueOnly -ErrorAction SilentlyContinue

    $lines = @($output | ForEach-Object { ([string]$_).Trim() } | Where-Object { $_ })
    $identity = @($lines | Where-Object { $_.StartsWith('moor ', [StringComparison]::Ordinal) })[0]

    if (-not $identity) {
        if ($null -eq $exitCode -and -not $lines) {
            throw "Windows did not run the downloaded binary at $Path (no output and no exit code). Check for a User Account Control prompt or an application-control policy."
        }
        $printed = if ($lines) { $lines -join ' | ' } else { 'nothing' }
        $status = if ($null -eq $exitCode) { 'no exit code' } else { "exit code $exitCode" }
        throw "The downloaded binary did not pass its version check ($status). $Path printed: $printed"
    }
    if ($null -ne $exitCode -and $exitCode -ne 0) {
        throw "The downloaded binary reported '$identity' but exited with code $exitCode"
    }
    return $identity
}

function Add-MoorUserPath {
    <#
        Appends a directory to the current user's PATH and reports whether the
        stored value changed.

        The registry is written directly rather than through
        [Environment]::SetEnvironmentVariable, which always stores REG_SZ: a user
        PATH is normally REG_EXPAND_SZ, and rewriting it as REG_SZ breaks entries
        such as %USERPROFILE%\bin.
    #>
    param([Parameter(Mandatory = $true)][string]$Directory)

    $normalized = $Directory.TrimEnd('\')
    $changed = $false

    $key = [Microsoft.Win32.Registry]::CurrentUser.OpenSubKey('Environment', $true)
    if (-not $key) { throw 'Could not open HKEY_CURRENT_USER\Environment for writing' }
    try {
        $current = [string]$key.GetValue('Path', '', [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames)
        $kind = if ($key.GetValueNames() -contains 'Path') {
            $key.GetValueKind('Path')
        } else {
            [Microsoft.Win32.RegistryValueKind]::ExpandString
        }

        $entries = @($current -split ';' | Where-Object { $_ })
        $present = $false
        foreach ($entry in $entries) {
            if ($entry.TrimEnd('\').Equals($normalized, [StringComparison]::OrdinalIgnoreCase)) {
                $present = $true
                break
            }
        }
        if (-not $present) {
            $key.SetValue('Path', ((@($entries) + $Directory) -join ';'), $kind)
            $changed = $true
        }
    } finally {
        $key.Close()
    }

    if ($changed) { Publish-MoorEnvironmentChange }

    if (-not (($env:Path -split ';') | Where-Object { $_.TrimEnd('\').Equals($normalized, [StringComparison]::OrdinalIgnoreCase) })) {
        $env:Path = "$env:Path;$Directory"
    }
    return $changed
}

function Invoke-MoorInstall {
    [CmdletBinding()]
    param(
        [string]$Version = 'latest',
        [string]$InstallDir,
        [string]$Repository = 'robert-mcdermott/moorage',
        [string]$Architecture,
        [switch]$NoPathUpdate
    )

    # Scoped to this function, so that piping the script into Invoke-Expression
    # does not leave the caller's session in strict mode.
    $ErrorActionPreference = 'Stop'
    Set-StrictMode -Version 2.0
    # Windows PowerShell draws a progress bar for every Invoke-WebRequest buffer,
    # which makes a 20 MB download take minutes instead of seconds.
    $ProgressPreference = 'SilentlyContinue'

    if (-not $env:OS -or $env:OS -ne 'Windows_NT') {
        throw 'install.ps1 is for Windows; on macOS and Linux, use install.sh'
    }
    if (-not $InstallDir) {
        if (-not $env:LOCALAPPDATA) {
            throw 'LOCALAPPDATA is not set; pass -InstallDir'
        }
        $InstallDir = Join-Path $env:LOCALAPPDATA 'Programs\Moorage'
    }
    if ($Repository -notmatch '^[0-9A-Za-z_.-]+/[0-9A-Za-z_.-]+$') {
        throw "Repository must use owner/repository syntax: $Repository"
    }
    if (-not [IO.Path]::IsPathRooted($InstallDir)) {
        throw "The installation directory must be an absolute path: $InstallDir"
    }

    $updatePath = -not $NoPathUpdate
    if ($updatePath -and $env:MOOR_NO_PATH_UPDATE -and $env:MOOR_NO_PATH_UPDATE -notmatch '^(0|false|no)$') {
        $updatePath = $false
    }

    $Version = Resolve-MoorVersion $Version
    $asset = Get-MoorAsset -Architecture $Architecture
    $base = if ($Version -eq 'latest') {
        "https://github.com/$Repository/releases/latest/download"
    } else {
        "https://github.com/$Repository/releases/download/$Version"
    }
    $versionLabel = if ($Version -eq 'latest') { 'the latest release' } else { $Version }

    $temporary = Join-Path ([IO.Path]::GetTempPath()) ('moor-download-' + [guid]::NewGuid())
    $binaryPath = Join-Path $temporary $asset
    $checksumPath = Join-Path $temporary 'SHA256SUMS'
    $destination = Join-Path $InstallDir 'moor.exe'
    $staged = Join-Path $InstallDir ('moor.staged.' + [guid]::NewGuid() + '.exe')
    $previousFile = Join-Path $InstallDir ('moor.previous.' + [guid]::NewGuid() + '.exe')
    $oldProtocol = [Net.ServicePointManager]::SecurityProtocol
    $movedAside = $false

    Write-Host "==> Installing moor from $versionLabel ($asset)"
    New-Item -ItemType Directory -Force -Path $temporary | Out-Null
    try {
        [Net.ServicePointManager]::SecurityProtocol = $oldProtocol -bor [Net.SecurityProtocolType]::Tls12

        Write-Host "==> Downloading $asset"
        Invoke-WebRequest -UseBasicParsing -Uri "$base/$asset" -OutFile $binaryPath
        Write-Host '==> Downloading SHA256SUMS'
        try {
            Invoke-WebRequest -UseBasicParsing -Uri "$base/SHA256SUMS" -OutFile $checksumPath
        } catch {
            throw "Could not download SHA256SUMS: $($_.Exception.Message)"
        }

        $expected = Get-MoorExpectedChecksum -ManifestPath $checksumPath -Asset $asset
        $actual = (Get-FileHash -LiteralPath $binaryPath -Algorithm SHA256).Hash.ToLowerInvariant()
        if ($actual -ne $expected) {
            throw "Checksum verification failed for $asset (expected $expected, got $actual)"
        }
        Write-Host '==> Checksum verified'

        New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
        Copy-Item -LiteralPath $binaryPath -Destination $staged
        $identity = Get-MoorIdentity -Path $staged
        $newVersion = ($identity -split '\s+')[1]
        if ($Version -ne 'latest' -and $newVersion -ne $Version.Substring(1)) {
            throw "The downloaded binary reports an unexpected version: $identity"
        }

        $previous = $null
        if (Test-Path -LiteralPath $destination) {
            try { $previous = ((Get-MoorIdentity -Path $destination) -split '\s+')[1] } catch { $previous = $null }
        }

        # Copies left by an earlier upgrade, if nothing runs them any more.
        Get-ChildItem -LiteralPath $InstallDir -Filter 'moor.previous.*.exe' -ErrorAction SilentlyContinue |
            Remove-Item -Force -ErrorAction SilentlyContinue

        # A running program cannot be replaced on Windows, and a server's supervisors
        # run from this file for as long as its apps do. It can be renamed, though:
        # what is running goes on, and the new file takes its name.
        if (Test-Path -LiteralPath $destination) {
            try {
                Move-Item -LiteralPath $destination -Destination $previousFile
            } catch {
                throw "Could not move $destination aside to replace it: $($_.Exception.Message)"
            }
            $movedAside = $true
        }
        try {
            Move-Item -LiteralPath $staged -Destination $destination
        } catch {
            if ($movedAside) {
                try {
                    Move-Item -LiteralPath $previousFile -Destination $destination
                } catch {
                    throw "The installation failed and the previous binary could not be restored; it is at $previousFile"
                }
            }
            throw "Could not install $destination. ($($_.Exception.Message))"
        }
        # This fails while something still runs from it: the next upgrade tries again.
        Remove-Item -LiteralPath $previousFile -Force -ErrorAction SilentlyContinue

        $pathChanged = $false
        $pathUpdated = $updatePath
        if ($updatePath) {
            # The binary is installed at this point, so a PATH failure must not
            # fail the installation.
            try {
                $pathChanged = Add-MoorUserPath -Directory $InstallDir
            } catch {
                $pathUpdated = $false
                Write-Warning "Could not update the user PATH: $($_.Exception.Message)"
            }
        }

        Write-Host "==> $identity"
        Write-Host "==> Installed at $destination"
        if ($pathUpdated) {
            if ($pathChanged) {
                Write-Host "==> Added $InstallDir to the user PATH"
                Write-Host '    Terminals that are already open need to be restarted to see it.'
            }
        } else {
            Write-Warning "$InstallDir is not on PATH; run $destination directly"
        }
        $run = if ($pathUpdated) { 'moor' } else { "& '$destination'" }

        if ($previous -and $previous -ne $newVersion) {
            Write-Host "==> Upgraded from $previous"
            Write-Host ''
            Write-Host 'A server running from this copy goes on with the old version until you restart it:'
            Write-Host 'press Ctrl+C where it runs, and start it again. Its apps keep running through that.'
            Write-Host ''
            Write-Host "See what changed: https://github.com/$Repository/blob/main/CHANGELOG.md"
        } else {
            Write-Host ''
            Write-Host 'Try it on this computer (apps at http://<app>.localhost:8080):'
            Write-Host "  $run server init --preset local"
            Write-Host "  $run server run"
            Write-Host ''
            Write-Host "Documentation: https://github.com/$Repository/blob/main/docs/install.md"
        }
    } finally {
        [Net.ServicePointManager]::SecurityProtocol = $oldProtocol
        Remove-Item -LiteralPath $staged -Force -ErrorAction SilentlyContinue
        Remove-Item -LiteralPath $temporary -Recurse -Force -ErrorAction SilentlyContinue
    }
}

if ($MyInvocation.InvocationName -ne '.') {
    $arguments = @{}
    if ($Version) { $arguments['Version'] = $Version }
    if ($InstallDir) { $arguments['InstallDir'] = $InstallDir }
    if ($Repository) { $arguments['Repository'] = $Repository }
    if ($Architecture) { $arguments['Architecture'] = $Architecture }
    if ($NoPathUpdate) { $arguments['NoPathUpdate'] = $true }
    Invoke-MoorInstall @arguments
}
