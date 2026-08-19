# ollama-mesh Windows installer
# Downloads the latest release binary from GitHub for your architecture.
#
# Control plane:
#   irm https://raw.githubusercontent.com/Anirudhx7/ollama-mesh/main/install.ps1 | iex
#
# Node Agent (run from an elevated/Administrator PowerShell), default path -
# the mesh admin UI's "Node Agent" panel gives you this exact command with a
# short-lived, single-use enrollment code (the binary exchanges it for the
# real token by calling back to MESH, so the real permanent bearer token
# never appears in this command / your PowerShell history - P50):
#   $env:ROLE="agent"; $env:MESH="<mesh admin base URL>"; $env:ENROLL="<code from the mesh admin UI>"
#   irm https://raw.githubusercontent.com/Anirudhx7/ollama-mesh/main/install.ps1 | iex
#
# Legacy/manual path - the real permanent token directly, no exchange, no MESH needed:
#   $env:ROLE="agent"; $env:TOKEN="<token from the mesh admin UI>"
#   irm https://raw.githubusercontent.com/Anirudhx7/ollama-mesh/main/install.ps1 | iex
#
# One of TOKEN or ENROLL+MESH is required for ROLE=agent - there is no
# existing-installation upgrade path (no prior Ollama Mesh deployments exist
# to preserve).
#
# ROLE=agent downloads the dedicated ollama-mesh-agent.exe (a separate
# artifact from the control-plane ollama-mesh.exe - a GPU host running this
# role never has a control-plane-capable executable on disk) and
# registers+starts it as a native Windows Service via the binary's own
# "service install" subcommand (internal/nodeagent/service) - this script's
# job for that role is just "download the right binary, then hand off to
# it," the same split install.sh uses for Linux/macOS.
#
# This script does not yet port install.sh's network-discovery wizard for
# the control-plane role on Windows - that's a separate, larger piece of
# work. For ROLE=mesh (the default), it downloads the binary and tells you
# how to run it; for ROLE=agent, it fully installs and starts the service.

$ErrorActionPreference = "Stop"

# Windows closes the console the instant this script's process exits -
# whether that's a double-clicked .ps1/.lnk or a one-shot `powershell
# -Command "...; irm ... | iex"` launch (e.g. the enroll one-liner from the
# mesh admin UI). Either way the final success/error message flashes and
# vanishes before anyone can read it. `exit N` inside the try block below
# still runs this finally block (PowerShell unwinds try/finally on exit), so
# wrapping the whole script is enough to always pause before the window
# closes - but only when someone is actually watching it happen.
$PauseBeforeExit = [Environment]::UserInteractive -and ($Host.Name -eq "ConsoleHost") -and (-not $env:CI)
function Wait-ForExit {
    if ($PauseBeforeExit) {
        Write-Host ""
        Write-Host "Press any key to exit..." -ForegroundColor DarkGray
        $null = $Host.UI.RawUI.ReadKey("NoEcho,IncludeKeyDown")
    }
}

try {

$Repo = "Anirudhx7/ollama-mesh"
$Role = if ($env:ROLE) { $env:ROLE } else { "mesh" }
$BinName = if ($Role -eq "agent") { "ollama-mesh-agent.exe" } else { "ollama-mesh.exe" }
$Token = $env:TOKEN
$Enroll = $env:ENROLL
$Mesh = $env:MESH
$Port = if ($env:PORT) { [int]$env:PORT } else { 9200 }
$InstallDir = if ($env:INSTALL_DIR) { $env:INSTALL_DIR } else { Join-Path $env:ProgramFiles "ollama-mesh" }

if (-not [Environment]::Is64BitOperatingSystem) {
    Write-Error "Unsupported architecture: 32-bit Windows is not supported."
    exit 1
}

# goreleaser (.goreleaser.yaml) excludes windows/arm64 today - only
# windows/amd64 binaries are published. Detect ARM64 and fail clearly rather
# than downloading a binary that doesn't exist.
$isArm64 = ($env:PROCESSOR_ARCHITECTURE -eq "ARM64") -or ($env:PROCESSOR_ARCHITEW6432 -eq "ARM64")
if ($isArm64) {
    Write-Error "windows/arm64 binaries are not published yet. Track releases: https://github.com/$Repo/releases"
    exit 1
}
$Arch = "amd64"

$BinaryAsset = if ($Role -eq "agent") { "ollama-mesh-agent-windows-$Arch.exe" } else { "ollama-mesh-windows-$Arch.exe" }
$Url = "https://github.com/$Repo/releases/latest/download/$BinaryAsset"
$BinPath = Join-Path $InstallDir $BinName
$ServiceName = "ollama-mesh-agent"

New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null

if ($Role -eq "agent") {
    # Windows locks a running .exe's file - downloading over $BinPath below
    # would fail outright if an already-installed agent service is still
    # holding it open (e.g. re-running this script to pick up a new
    # release). Linux/systemd can replace a running binary's file freely, so
    # this has no equivalent there. Stop the existing service first and wait
    # for it to actually exit before the download runs; ignore all errors
    # here (sc.exe/query failing just means no prior install to stop).
    $null = & sc.exe query $ServiceName 2>$null
    if ($LASTEXITCODE -eq 0) {
        & sc.exe stop $ServiceName 2>$null | Out-Null
        $deadline = (Get-Date).AddSeconds(10)
        while ((Get-Date) -lt $deadline) {
            $state = & sc.exe query $ServiceName 2>$null
            if ($LASTEXITCODE -ne 0 -or -not ($state -match "RUNNING")) { break }
            Start-Sleep -Milliseconds 250
        }
    }
}

Write-Host "Downloading ollama-mesh for windows/$Arch..."
Write-Host "  $Url"
try {
    Invoke-WebRequest -Uri $Url -OutFile $BinPath -UseBasicParsing
} catch {
    Write-Error "Failed to download $Url : $_"
    Write-Error "Check releases manually: https://github.com/$Repo/releases/latest"
    exit 1
}

$VersionOutput = & $BinPath -version 2>$null
$NewVersion = ($VersionOutput -split '\s+')[-1]
Write-Host "Installed ollama-mesh $NewVersion to $BinPath"

# Unlike Linux (/usr/local/bin is already on PATH) and macOS, there's no
# Windows equivalent default-PATH directory under Program Files - without
# this, every command this script (and "agent service status"/"uninstall"
# it tells the operator to run afterward) prints as a bare "ollama-mesh ..."
# would fail with "not recognized" in any shell, exactly the class of bug
# this closes. Machine scope needs elevation; fall back to User scope
# (still PATH-effective for this account, no admin required) otherwise -
# both scopes are additive with the built-in PATH, never overwritten.
$currentPrincipalForPath = New-Object Security.Principal.WindowsPrincipal([Security.Principal.WindowsIdentity]::GetCurrent())
$IsElevated = $currentPrincipalForPath.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
$PathScope = if ($IsElevated) { "Machine" } else { "User" }
$ExistingPath = [Environment]::GetEnvironmentVariable("Path", $PathScope)
if (-not $ExistingPath) { $ExistingPath = "" }
$PathEntries = $ExistingPath -split ";" | Where-Object { $_ -ne "" }
if (-not ($PathEntries -contains $InstallDir)) {
    # Join only non-empty entries - if $ExistingPath was blank (common: a
    # fresh account has no User-scope Path at all), a naive
    # "$ExistingPath;$InstallDir" leaves a leading empty segment, which
    # cmd.exe/PowerShell resolve as "search the current directory first" -
    # a PATH-injection footgun, and exactly the common case this branch hits.
    $NewPath = (@($PathEntries) + $InstallDir) -join ";"
    try {
        [Environment]::SetEnvironmentVariable("Path", $NewPath, $PathScope)
        Write-Host "Added $InstallDir to your $PathScope PATH - open a NEW terminal window for the 'ollama-mesh' command to be recognized there."
    } catch {
        Write-Host "Could not add $InstallDir to PATH automatically ($_) - add it manually, or always run '$BinPath' by full path."
    }
    # Broadcast WM_SETTINGCHANGE so already-open Explorer/shells notice the
    # change - best-effort only (a locked-down/AV-restricted host may not be
    # able to compile the inline P/Invoke); the registry write above already
    # succeeded regardless, so a failure here must not be reported as a PATH
    # update failure - the "open a new terminal" note above already covers
    # the case where no broadcast reaches an already-open window.
    try {
        Add-Type -Namespace Win32 -Name NativeMethods -MemberDefinition @"
[DllImport("user32.dll", SetLastError = true, CharSet = CharSet.Auto)]
public static extern IntPtr SendMessageTimeout(IntPtr hWnd, uint Msg, UIntPtr wParam, string lParam, uint fuFlags, uint uTimeout, out UIntPtr lpdwResult);
"@ -ErrorAction Stop
        $result = [UIntPtr]::Zero
        [Win32.NativeMethods]::SendMessageTimeout([IntPtr]0xffff, 0x1a, [UIntPtr]::Zero, "Environment", 2, 5000, [ref]$result) | Out-Null
    } catch {
        # Silent - purely an optimization for already-open windows, not the PATH update itself.
    }
    $env:Path = $env:Path.TrimEnd(";") + ";" + $InstallDir
}

if ($Role -eq "agent") {
    if (-not $Token -and -not $Enroll) {
        Write-Error "ROLE=agent requires TOKEN=<token> or ENROLL=<code> MESH=<url>."
        Write-Error "Generate one from the mesh admin UI: GPU Nodes -> (a node) -> Node Agent -> Enable Agent."
        exit 1
    }
    if ($Enroll -and -not $Token -and -not $Mesh) {
        Write-Error "ENROLL=<code> requires MESH=<url> (the mesh admin dashboard's address)."
        exit 1
    }

    if (-not $IsElevated) {
        Write-Error "Installing the Node Agent service requires an elevated (Run as Administrator) PowerShell session."
        exit 1
    }

    Write-Host ""
    Write-Host "Installing ollama-mesh Node Agent as a Windows service (port $Port)..."
    if ($Token) {
        # Deliberately not passing --token=$Token here - that would put the
        # real bearer token in this process's argv (visible via Task
        # Manager/`Get-Process -IncludeUserName`/WMI for the life of the
        # install). $env:TOKEN is already set in this process's environment
        # and is inherited by the child process automatically; the binary's
        # own "service install" subcommand already falls back to the
        # TOKEN env var when --token isn't given.
        & $BinPath service install --port=$Port
    } else {
        & $BinPath service install --port=$Port --enroll=$Enroll --mesh=$Mesh
    }
    if ($LASTEXITCODE -ne 0) {
        Write-Error "Node Agent service install failed (exit code $LASTEXITCODE)."
        exit 1
    }

    Write-Host ""
    Write-Host "Node Agent installed and running - the mesh will start polling it on its next poll cycle."
    Write-Host "  Status:    ollama-mesh-agent service status"
    Write-Host "  Uninstall: ollama-mesh-agent service uninstall"
    exit 0
}

Write-Host ""
Write-Host "ollama-mesh successfully installed to $BinPath"
Write-Host "Run: & '$BinPath'"
Write-Host "Docs: https://github.com/$Repo"

} finally {
    Wait-ForExit
}
