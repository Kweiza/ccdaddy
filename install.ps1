<#
.SYNOPSIS
    ccdad installer for Windows.

.DESCRIPTION
    irm https://raw.githubusercontent.com/Kweiza/ccdaddy/main/install.ps1 | iex

    Targets Windows PowerShell 5.1, which is what a stock Windows has. Options
    come from the environment because `irm | iex` cannot pass arguments:

      CCDAD_INSTALL_DIR   where the binary goes (default:
                          $env:LOCALAPPDATA\Programs\ccdad, deliberately not
                          roaming - a roaming profile would sync an 8 MB
                          binary to every machine the user signs into)
      CCDAD_VERSION       a released tag to pin, e.g. v1.2.3 (default: the
                          latest non-prerelease)
      CCDAD_BASE_URL      download origin, for mirrors and for the tests

    This file is kept ASCII-only on purpose: 5.1 decodes a piped script by
    guessing at the encoding, and a stray non-ASCII byte can turn the whole
    thing into mojibake before the first line runs.

.PARAMETER NoRun
    Define the functions without installing anything. Only the test harness
    passes this, by dot-sourcing the file; `iex` never passes arguments, so the
    installer path is unaffected.
#>
param([switch]$NoRun)

$ErrorActionPreference = 'Stop'

# Invoke-WebRequest's progress bar costs more than the download does on 5.1:
# it repaints per chunk and turns an 8 MB transfer into minutes.
$ProgressPreference = 'SilentlyContinue'

# 5.1 negotiates SSL3/TLS1.0 by default on an unpatched host, so the FIRST
# thing that happens is a "Could not create SSL/TLS secure channel" from
# raw.githubusercontent.com - before a single line of this script has run, when
# it is being fetched. Setting it here fixes the downloads this script itself
# makes; the bootstrap one-liner needs the same line ahead of it on such a
# host, which the README documents.
try {
    [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
} catch {
    # .NET Core ignores this property; nothing to do and nothing to report.
}

$script:CcdadRepoSlug = 'Kweiza/ccdaddy'

function Test-CcdadOnWindows {
    # $IsWindows exists only in PowerShell 6+. Windows PowerShell 5.1 leaves it
    # undefined and only runs on Windows, so undefined means Windows. Read it
    # through Get-Variable so this is also correct under Set-StrictMode.
    $v = Get-Variable -Name IsWindows -ErrorAction SilentlyContinue
    if ($null -eq $v) { return $true }
    return [bool]$v.Value
}

function Get-CcdadAssetName {
    <#
    .SYNOPSIS
        Resolve the release asset for this machine's architecture.
    #>
    param(
        [string]$Architew6432,
        [string]$Architecture
    )

    # PROCESSOR_ARCHITEW6432 has to be read FIRST. Under WOW64 - a 32-bit
    # PowerShell on 64-bit Windows, which is what several launchers still give
    # you - PROCESSOR_ARCHITECTURE reports x86, and trusting it sends the
    # installer looking for an asset that does not exist.
    $arch = $Architew6432
    if ([string]::IsNullOrWhiteSpace($arch)) { $arch = $Architecture }

    switch -Regex ($arch) {
        '^(?i)AMD64$' { return 'ccdad-windows-amd64.exe' }
        '^(?i)ARM64$' { return 'ccdad-windows-arm64.exe' }
        default {
            throw "unsupported architecture: '$arch' (ccdad ships amd64 and arm64)"
        }
    }
}

function Get-CcdadDownloadBase {
    <#
    .SYNOPSIS
        The directory URL the asset and sums file are fetched from.
    #>
    param(
        [string]$BaseUrl,
        [string]$Version
    )

    if ([string]::IsNullOrWhiteSpace($Version)) {
        # Deliberately not api.github.com/repos/.../releases/latest: sixty
        # unauthenticated requests an hour turns an install behind a corporate
        # NAT or inside CI into a mystery failure. This costs no API call, at
        # the price of a redirect - hence the shape checks further down.
        return "$BaseUrl/latest/download"
    }
    $tag = $Version.Trim()
    if ($tag -notmatch '^v') { $tag = "v$tag" }
    return "$BaseUrl/download/$tag"
}

function Test-CcdadSumsShape {
    <#
    .SYNOPSIS
        Does this look like a checksum file at all?
    .DESCRIPTION
        The latest-release URL is a redirect, and a proxy that answers it with
        its own HTML page produces a "the asset is not listed" abort that sends
        the reader looking for a missing asset. Saying which of the two
        happened is worth its own branch.
    #>
    param([string[]]$Lines)

    foreach ($line in $Lines) {
        if ($line -cmatch '^[0-9a-f]{64}  ') { return $true }
    }
    return $false
}

function Get-CcdadExpectedHash {
    <#
    .SYNOPSIS
        The recorded hash for one asset, or $null if it is not listed.
    #>
    param(
        [string[]]$Lines,
        [string]$Asset
    )

    # Anchored at BOTH ends, case-sensitively lowercase, and TWO spaces, which
    # is what sha256sum and `shasum -a 256` emit. One space matches nothing; a
    # missing trailing anchor lets ccdad-windows-amd64.exe be satisfied by a
    # longer neighbour; a missing leading one lets any prefixed line through.
    $pattern = '^([0-9a-f]{64})  ' + [regex]::Escape($Asset) + '$'
    foreach ($line in $Lines) {
        $m = [regex]::Match($line, $pattern)
        if ($m.Success) { return $m.Groups[1].Value }
    }
    return $null
}

function Get-CcdadUpdatedPath {
    <#
    .SYNOPSIS
        The user PATH with the install directory appended, or $null if it is
        already there.
    .DESCRIPTION
        Kept separate from the registry access so the decision - which is where
        duplicate entries come from - can be tested off Windows.
    #>
    param(
        [string]$Current,
        [string]$Directory
    )

    # @(...) is load-bearing. A pipeline that yields one element collapses to
    # a scalar string, and $scalar + $Directory is string CONCATENATION - so a
    # PATH with a single entry would come back with the install directory glued
    # onto it and no separator.
    $entries = @()
    if (-not [string]::IsNullOrEmpty($Current)) {
        $entries = @($Current.Split(';') | Where-Object { $_ -ne '' })
    }
    # Spelled out rather than left to -eq, which is case-insensitive by
    # default: this comparison decides whether a PATH entry gets duplicated,
    # and it should say which comparison it means. A trailing separator is not
    # part of the name Windows stores either.
    # BOTH spellings, because the value is read unexpanded (see
    # Add-CcdadToUserPath) while $Directory is fully expanded. Comparing only
    # the raw text means a Path holding `%LOCALAPPDATA%\Programs\ccdad` never
    # matches the expanded install directory, so every run appends another
    # copy -- which is the opposite of what this function exists for. ccdad's
    # own Go implementation (internal/cli/setuppath.go, matchesEntry) applies
    # the same rule, and `ccdad uninstall` removes on it: a duplicate this
    # missed would also be a duplicate uninstall could not find.
    foreach ($entry in $entries) {
        foreach ($form in @($entry, [Environment]::ExpandEnvironmentVariables($entry))) {
            if ([string]::Equals($form.TrimEnd('\'), $Directory.TrimEnd('\'), [StringComparison]::OrdinalIgnoreCase)) { return $null }
        }
    }
    if ($entries.Count -eq 0) { return $Directory }
    return (($entries + $Directory) -join ';')
}

function Add-CcdadToUserPath {
    <#
    .SYNOPSIS
        Append the install directory to the user PATH, in the registry.
    #>
    param([string]$Directory)

    if (-not (Test-CcdadOnWindows)) { return $false }

    # Two landmines, both of which have broken shipped installers.
    #
    # 1. Never read $env:Path and write it back to User scope. That variable is
    #    Machine+User concatenated, so writing it to User permanently
    #    duplicates the entire machine PATH into the user's - and the next
    #    installer to do the same doubles it again.
    # 2. [Environment]::SetEnvironmentVariable writes REG_SZ. If the existing
    #    value was REG_EXPAND_SZ, that silently destroys %VAR% expansion for
    #    every entry in it. Open HKCU\Environment, keep the value kind, and
    #    read with DoNotExpandEnvironmentNames so the %VAR% entries are written
    #    back as themselves rather than as today's expansion.
    $key = [Microsoft.Win32.Registry]::CurrentUser.OpenSubKey('Environment', $true)
    if ($null -eq $key) { return $false }
    try {
        $kind = [Microsoft.Win32.RegistryValueKind]::ExpandString
        $current = ''
        if ($key.GetValueNames() -contains 'Path') {
            $kind = $key.GetValueKind('Path')
            $current = [string]$key.GetValue('Path', '', [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames)
        }
        $updated = Get-CcdadUpdatedPath -Current $current -Directory $Directory
        if ($null -eq $updated) { return $false }
        $key.SetValue('Path', $updated, $kind)
    } finally {
        $key.Close()
    }

    # Write down what was added, so `ccdad uninstall` can take back this entry
    # and only this entry. A registry PATH component carries no evidence of who
    # added it, and the install directory is routinely one the USER put on PATH
    # (a zip install into their own tools directory, or %USERPROFILE%\go\bin
    # from `go install`) -- removing it on ownership guessed from the binary's
    # location breaks every other program in it. ccdad's Go implementation
    # writes the same value from setup-path; the two must agree, because either
    # may be the one that registered the entry uninstall later removes.
    try {
        $record = [Microsoft.Win32.Registry]::CurrentUser.CreateSubKey('Software\ccdad')
        try { $record.SetValue('PathEntry', $Directory, [Microsoft.Win32.RegistryValueKind]::String) }
        finally { $record.Close() }
    } catch {
        Write-Warning "PATH was updated but ccdad could not record it, so 'ccdad uninstall' will leave the entry in place. ($_)"
    }

    # Without this, only processes started after the next sign-out see it.
    try {
        if (-not ('CcdadNative.Win32' -as [type])) {
            Add-Type -Namespace CcdadNative -Name Win32 -MemberDefinition @'
[DllImport("user32.dll", SetLastError = true, CharSet = CharSet.Auto)]
public static extern IntPtr SendMessageTimeout(IntPtr hWnd, uint Msg, UIntPtr wParam, string lParam, uint fuFlags, uint uTimeout, out UIntPtr lpdwResult);
'@
        }
        $result = [UIntPtr]::Zero
        [void][CcdadNative.Win32]::SendMessageTimeout([IntPtr]0xffff, 0x1A, [UIntPtr]::Zero, 'Environment', 0x2, 5000, [ref]$result)
    } catch {
        Write-Warning "PATH was updated but the broadcast failed; open a new terminal. ($_)"
    }
    return $true
}

function Get-CcdadFile {
    param([string]$Uri, [string]$OutFile)
    Invoke-WebRequest -Uri $Uri -OutFile $OutFile -UseBasicParsing
}

function Invoke-CcdadInstall {
    $baseUrl = $env:CCDAD_BASE_URL
    if ([string]::IsNullOrWhiteSpace($baseUrl)) {
        $baseUrl = "https://github.com/$script:CcdadRepoSlug/releases"
    }
    $installDir = $env:CCDAD_INSTALL_DIR
    if ([string]::IsNullOrWhiteSpace($installDir)) {
        if ([string]::IsNullOrWhiteSpace($env:LOCALAPPDATA)) {
            throw 'LOCALAPPDATA is not set; point CCDAD_INSTALL_DIR at an install directory'
        }
        $installDir = Join-Path $env:LOCALAPPDATA 'Programs\ccdad'
    }

    $asset = Get-CcdadAssetName -Architew6432 $env:PROCESSOR_ARCHITEW6432 -Architecture $env:PROCESSOR_ARCHITECTURE
    $download = Get-CcdadDownloadBase -BaseUrl $baseUrl -Version $env:CCDAD_VERSION

    New-Item -ItemType Directory -Force -Path $installDir | Out-Null

    # Staged inside the install directory so the final move is a rename on the
    # same volume. %TEMP% is routinely a different volume, and a cross-volume
    # move is a copy, which cannot land on a file another process is running.
    $staging = Join-Path $installDir (".ccdad-install." + [System.IO.Path]::GetRandomFileName())
    New-Item -ItemType Directory -Force -Path $staging | Out-Null
    try {
        $sumsPath = Join-Path $staging 'sha256sums.txt'
        try {
            Get-CcdadFile -Uri "$download/sha256sums.txt" -OutFile $sumsPath
        } catch {
            throw "cannot download $download/sha256sums.txt - refusing to install unverified ($($_.Exception.Message))"
        }
        $lines = @(Get-Content -LiteralPath $sumsPath)
        if (-not (Test-CcdadSumsShape -Lines $lines)) {
            throw "$download/sha256sums.txt is not a checksum file - a proxy or an error page?"
        }
        $expected = Get-CcdadExpectedHash -Lines $lines -Asset $asset
        if ($null -eq $expected) {
            throw "$asset is not listed in $download/sha256sums.txt - refusing to install unverified"
        }

        Write-Host "downloading $asset"
        $assetPath = Join-Path $staging $asset
        try {
            Get-CcdadFile -Uri "$download/$asset" -OutFile $assetPath
        } catch {
            throw "cannot download $download/$asset ($($_.Exception.Message))"
        }

        $size = (Get-Item -LiteralPath $assetPath).Length
        if ($size -lt 1000000) {
            throw "$asset downloaded as $size bytes, which is not a ccdad binary - a proxy or an error page?"
        }

        # Get-FileHash answers in uppercase and the sums file is lowercase, so
        # one of them has to be folded. -cne rather than -ne on purpose: -ne is
        # case-insensitive, which would make this pass whether the fold
        # happened or not, and an implicit case fold is not something a
        # verification step should be resting on.
        $actual = (Get-FileHash -Algorithm SHA256 -LiteralPath $assetPath).Hash.ToLowerInvariant()
        if ($actual -cne $expected) {
            throw "checksum mismatch for ${asset}: expected $expected, got $actual"
        }

        # Mark-of-the-Web survives the move, and SmartScreen acts on it the
        # first time the binary runs.
        if (Test-CcdadOnWindows) {
            try { Unblock-File -LiteralPath $assetPath } catch { }
        }

        $target = Join-Path $installDir 'ccdad.exe'
        if (Test-Path -LiteralPath $target) {
            # The daemon is self-managed and holds a singleton lock, so leaving
            # it running means the OLD code keeps running indefinitely. This
            # invokes the OLD binary, which may predate the daemon command
            # group and answer `unknown command "daemon"` with exit 2, so its
            # exit code cannot be allowed to abort the upgrade.
            try {
                & $target daemon stop 2>$null | Out-Null
            } catch { }
            # A running .exe cannot be overwritten, but it CAN be renamed.
            $aside = Join-Path $installDir (".ccdad-old." + [System.IO.Path]::GetRandomFileName() + ".exe")
            Move-Item -LiteralPath $target -Destination $aside -Force
            try {
                Remove-Item -LiteralPath $aside -Force
            } catch {
                # Still running. It is out of the way, which is what mattered.
            }
        }
        Move-Item -LiteralPath $assetPath -Destination $target -Force

        $version = 'ccdad'
        try { $version = (& $target --version 2>$null | Select-Object -First 1) } catch { }
        Write-Host "installed $version to $target"

        if (Add-CcdadToUserPath -Directory $installDir) {
            Write-Host "added $installDir to your user PATH; open a new terminal to pick it up"
        } else {
            # $false means the entry was already there, or the registry could
            # not be opened. Only the second case needs the user, and pointing
            # at `ccdad setup-path` covers it without this script having to tell
            # the two apart: that command does the same write, reports which it
            # was, and exits 3 when there was nothing to do.
            Write-Host "if '$installDir' is not on your PATH in a new terminal, run:"
            Write-Host "    & '$target' setup-path"
        }
        Write-Host "To remove ccdad later run 'ccdad uninstall', not a delete: there is a daemon"
        Write-Host "to stop, a token directory to clear and possibly an MCP entry to unwire."
    } finally {
        Remove-Item -LiteralPath $staging -Recurse -Force -ErrorAction SilentlyContinue
    }
}

if (-not $NoRun) {
    Invoke-CcdadInstall
}
