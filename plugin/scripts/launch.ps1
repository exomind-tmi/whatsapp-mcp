# whatsapp-mcp launcher for Windows (Windows PowerShell 5.1).
#
# plugin/.mcp.json runs ${CLAUDE_PLUGIN_ROOT}/scripts/launch-whatsapp-mcp: on
# Windows the hosts pick launch-whatsapp-mcp.cmd, which runs this script; on
# Linux the sh launcher launch-whatsapp-mcp next to it does the same.
#
# It runs the whatsapp-mcp binary pinned in ..\release.json from
# <home>\bin\<version>\, downloading it from GitHub Releases on first use and
# checking its SHA-256.
#
# stdout is the MCP channel: nothing but the binary's own output may reach it,
# so every value a command returns is discarded. The file stays ASCII-only:
# PowerShell 5.1 reads BOM-less scripts in the ANSI code page.

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue' # the progress bar makes Invoke-WebRequest crawl

$platform = 'windows-amd64'
$exeName = 'whatsapp-mcp.exe'
$lockWait = 180 # seconds to wait while a parallel launcher downloads
$logFile = $null

function Write-Log {
    [Diagnostics.CodeAnalysis.SuppressMessageAttribute('PSAvoidUsingEmptyCatchBlock', '',
        Justification = 'a busy log never stops the server')]
    param([string]$msg)
    if (-not $logFile) { return }
    $now = (Get-Date).ToUniversalTime().ToString('yyyy-MM-ddTHH:mm:ss.fffZ')
    # Parallel launchers append at the same moment and AppendAllText fails on
    # the one that finds the file open: retry briefly.
    for ($i = 0; $i -lt 50; $i++) {
        try { [IO.File]::AppendAllText($logFile, "$now pid=$PID $msg`n"); return } catch { }
        Start-Sleep -Milliseconds 20
    }
}

function Exit-Launcher([string]$msg) {
    Write-Log "error: $msg"
    [Console]::Error.WriteLine("whatsapp-mcp launcher: $msg")
    exit 1
}

function Test-Binary([string]$path) {
    $f = New-Object IO.FileInfo $path
    return $f.Exists -and $f.Length -gt 0
}

# Test-Busy: whether an error is a file being in use, worth waiting out.
function Test-Busy([Management.Automation.ErrorRecord]$err) {
    $e = $err.Exception
    if ($e.InnerException) { $e = $e.InnerException } # PowerShell wraps .NET call errors in MethodInvocationException
    return $e -is [IO.IOException] -or $e -is [UnauthorizedAccessException]
}

# Take the download lock: an exclusively opened file whose handle child
# processes inherit. Windows releases it when its last holder exits: a killed
# launcher leaves no stale lock, and a curl.exe that outlives the launcher keeps
# the lock until its download ends.
function Lock-File([string]$path) {
    $deadline = (Get-Date).AddSeconds($lockWait)
    while ($true) {
        try { return [IO.File]::Open($path, 'OpenOrCreate', 'ReadWrite', 'None, Inheritable') } catch {
            if (-not (Test-Busy $_)) { throw }
        }
        if ((Get-Date) -gt $deadline) {
            Exit-Launcher "another launcher has been downloading $version for over ${lockWait}s"
        }
        Start-Sleep -Milliseconds 250
    }
}

# curl.exe (Windows 10 1803+) resumes a partial $out (-C -) but ignores the
# system proxy, so it is used only for direct connections. Behind a proxy,
# without curl.exe, or when curl fails, Invoke-WebRequest downloads from
# scratch, passing the user's credentials to the proxy. A stalled transfer
# does not hold the lock forever: curl fails under 1 KB/s for 30 s;
# Invoke-WebRequest after 60 s without a response, or after .NET's 5-minute
# read timeout once the body has started.
function Save-Url([string]$url, [string]$out) {
    $direct = [Net.WebRequest]::GetSystemWebProxy().IsBypassed((New-Object Uri $url))
    $curl = Get-Command curl.exe -CommandType Application -ErrorAction SilentlyContinue | Select-Object -First 1
    if ($direct -and $curl) {
        $ErrorActionPreference = 'Continue' # curl's stderr must not turn into an exception
        $null = & $curl.Source -fsSL -C - --connect-timeout 10 --speed-limit 1024 --speed-time 30 -o $out $url
        $ErrorActionPreference = 'Stop'
        if ($LASTEXITCODE -eq 0) { return }
        Write-Log "curl.exe exited with $LASTEXITCODE, retrying with Invoke-WebRequest"
    }
    Remove-Item -LiteralPath $out -Force -ErrorAction SilentlyContinue # no resume here
    [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12
    [Net.WebRequest]::DefaultWebProxy.Credentials = [Net.CredentialCache]::DefaultNetworkCredentials
    try { $null = Invoke-WebRequest -Uri $url -OutFile $out -UseBasicParsing -TimeoutSec 60 }
    catch { throw "download failed: ${url}: $($_.Exception.Message)" }
}

# Download, verify, then rename into place: the target never holds a partial
# or unverified file. Runs under the lock, and the partial download is kept per
# version, so when a host kills a launcher mid-download (e.g. on its MCP start
# timeout) the next launcher resumes it instead of starting over.
function Install-Binary($asset) {
    $part = Join-Path $binDir "$version.part"
    Write-Log "downloading $($asset.url)"
    try { Save-Url $asset.url $part } catch {
        if (-not (Test-Binary $part)) { Remove-Item -LiteralPath $part -Force -ErrorAction SilentlyContinue } # keep only something to resume
        throw
    }
    $got = (Get-FileHash -Algorithm SHA256 -LiteralPath $part).Hash
    if ($got -ne $asset.sha256) {
        Remove-Item -LiteralPath $part -Force -ErrorAction SilentlyContinue # never resume a corrupt file
        Exit-Launcher "SHA-256 mismatch for $($asset.url): expected $($asset.sha256), got $got"
    }
    try {
        $null = [IO.Directory]::CreateDirectory((Split-Path $exe))
        # A fresh exe is what antivirus and the indexer open first: retry the
        # rename for a while, as the Go shim does (home.Retry).
        $deadline = (Get-Date).AddSeconds(3)
        while ($true) {
            try {
                if (Test-Path -LiteralPath $exe) { [IO.File]::Delete($exe) } # empty leftover
                [IO.File]::Move($part, $exe)
                break
            } catch {
                if (-not (Test-Busy $_) -or (Get-Date) -gt $deadline) {
                    if (Test-Binary $exe) { break } # installed without the lock, e.g. by scripts/dev-install.sh
                    throw
                }
            }
            Start-Sleep -Milliseconds 100
        }
        Write-Log "installed $exe"
    } catch {
        Remove-Item -LiteralPath $part -Force -ErrorAction SilentlyContinue
        throw
    }
}

function Get-Asset {
    $asset = $release.assets.$platform
    if (-not $asset -or -not $asset.url -or $asset.sha256 -notmatch '^[0-9a-fA-F]{64}$') {
        Exit-Launcher "release.json ($version) has no $platform build; is this plugin released yet?"
    }
    return $asset
}

try {
    if ($env:WHATSAPP_MCP_HOME) {
        $stateDir = $env:WHATSAPP_MCP_HOME
        if ($stateDir -notmatch '^([A-Za-z]:[\\/]|\\\\)') {
            Exit-Launcher "WHATSAPP_MCP_HOME must be an absolute path, got '$stateDir'"
        }
    } else {
        $stateDir = Join-Path $env:USERPROFILE '.mcp\exomind-tmi\whatsapp-mcp'
    }
    $logDir = Join-Path $stateDir 'logs'
    $null = [IO.Directory]::CreateDirectory($logDir)
    $logFile = Join-Path $logDir 'launcher.log'
    $log = New-Object IO.FileInfo $logFile
    if ($log.Exists -and $log.Length -gt 1MB) {
        Move-Item -LiteralPath $logFile -Destination "$logFile.1" -Force -ErrorAction SilentlyContinue
    }

    $releaseFile = Join-Path $PSScriptRoot '..\release.json'
    try { $release = [IO.File]::ReadAllText($releaseFile) | ConvertFrom-Json }
    catch { Exit-Launcher "cannot read ${releaseFile}: $($_.Exception.Message)" }
    $version = [string]$release.version
    # Semver only: the version becomes a path segment.
    if ($version -notmatch '^v\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$') {
        Exit-Launcher "release.json has no valid version: '$version'"
    }
    $binDir = Join-Path $stateDir 'bin'
    $exe = Join-Path $binDir "$version\$exeName"

    Write-Log "start $version"
    if (-not (Test-Binary $exe)) {
        $asset = Get-Asset
        $null = [IO.Directory]::CreateDirectory($binDir)
        $lock = Lock-File (Join-Path $binDir "$version.download.lock")
        try {
            # Re-check: a parallel launcher may have installed it while we waited.
            if (-not (Test-Binary $exe)) { Install-Binary $asset }
        } finally {
            $lock.Dispose()
        }
    }
} catch {
    Exit-Launcher $_.Exception.Message
}

$ErrorActionPreference = 'Continue' # the server's stderr must not turn into an exception
try { & $exe stdio } catch { Exit-Launcher "cannot start ${exe}: $($_.Exception.Message)" }
exit $LASTEXITCODE
