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
# ROLE=agent registers+starts the Node Agent as a native Windows Service via
# the binary's own "ollama-mesh agent service install" subcommand
# (internal/nodeagent/service) - this script's job for that role is just
# "download the right binary, then hand off to it," the same split
# install.sh uses for Linux/macOS.
#
# This script does not yet port install.sh's network-discovery wizard for
# the control-plane role on Windows - that's a separate, larger piece of
# work. For ROLE=mesh (the default), it downloads the binary and tells you
# how to run it; for ROLE=agent, it fully installs and starts the service.

$ErrorActionPreference = "Stop"

$Repo = "Anirudhx7/ollama-mesh"
$BinName = "ollama-mesh.exe"
$Role = if ($env:ROLE) { $env:ROLE } else { "mesh" }
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

$BinaryAsset = "ollama-mesh-windows-$Arch.exe"
$Url = "https://github.com/$Repo/releases/latest/download/$BinaryAsset"
$BinPath = Join-Path $InstallDir $BinName

New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null

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

    $currentPrincipal = New-Object Security.Principal.WindowsPrincipal([Security.Principal.WindowsIdentity]::GetCurrent())
    if (-not $currentPrincipal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
        Write-Error "Installing the Node Agent service requires an elevated (Run as Administrator) PowerShell session."
        exit 1
    }

    Write-Host ""
    Write-Host "Installing ollama-mesh Node Agent as a Windows service (port $Port)..."
    if ($Token) {
        & $BinPath agent service install --port=$Port --token=$Token
    } else {
        & $BinPath agent service install --port=$Port --enroll=$Enroll --mesh=$Mesh
    }
    if ($LASTEXITCODE -ne 0) {
        Write-Error "Node Agent service install failed (exit code $LASTEXITCODE)."
        exit 1
    }

    Write-Host ""
    Write-Host "Node Agent installed and running on port $Port - the mesh will start polling it on its next poll cycle."
    Write-Host "  Status:    ollama-mesh agent service status"
    Write-Host "  Uninstall: ollama-mesh agent service uninstall"
    exit 0
}

Write-Host ""
Write-Host "ollama-mesh successfully installed to $BinPath"
Write-Host "Run: & '$BinPath'"
Write-Host "Docs: https://github.com/$Repo"
