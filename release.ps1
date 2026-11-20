<#
.SYNOPSIS
    Builds the gophant release packages (one archive per target platform) and a SHA256SUMS.txt.

.DESCRIPTION
    For every target the script
      1. cross-compiles ./cmd/gophant with CGO disabled (the binary has no C dependency),
      2. stages the binary together with configs/gophant.example.yaml, README.md and docs/*.md,
      3. packs the stage: a .zip for windows, a .tar.gz for everything else. In the .tar.gz the
         binary is stored executable (mode 0755), so it can be run right after "tar -xzf".
    Finally it writes SHA256SUMS.txt next to the archives (check it with "sha256sum -c").

    Unless -SkipTests is given, go vet and go test run first (unit tests only, no Docker needed),
    and go mod verify checks the downloaded dependencies.

    Nothing is published or uploaded: the packages are only written to the output directory.

.PARAMETER Version
    Version that is built into the binary ("gophant --version") and used in the file names.
    Default: "git describe --tags --always --dirty", or "dev" outside a git checkout.
    Tag the commit (git tag v0.1.0) or pass -Version to get a clean release name.

.PARAMETER Targets
    Platforms as os/arch, separated by commas or given as an array.
    Default: linux/amd64 and windows/amd64.
    Supported: linux/amd64, linux/arm64, windows/amd64, windows/arm64, darwin/amd64, darwin/arm64.

.PARAMETER OutDir
    Output directory, relative to the repository root unless it is absolute. Default: release.
    The packages are written to <OutDir>/<Version>/ and a previous build of the same version is replaced.

.PARAMETER SkipTests
    Skip go vet and go test.

.EXAMPLE
    powershell -NoProfile -ExecutionPolicy Bypass -File .\release.ps1 -Version v0.1.0

.EXAMPLE
    .\release.ps1 -Version v0.1.0 -Targets linux/amd64,linux/arm64 -SkipTests
#>
[CmdletBinding()]
param(
    [string]$Version,
    [string[]]$Targets = @('linux/amd64', 'windows/amd64'),
    [string]$OutDir = 'release',
    [switch]$SkipTests
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$RepoRoot = $PSScriptRoot
$Supported = @('linux/amd64', 'linux/arm64', 'windows/amd64', 'windows/arm64', 'darwin/amd64', 'darwin/arm64')

function Write-Step([string]$Message) {
    Write-Host "==> $Message" -ForegroundColor Cyan
}

# A native command that fails does not stop the script by itself, its exit code has to be checked.
function Assert-ExitCode([string]$What) {
    if ($LASTEXITCODE -ne 0) {
        throw "$What failed with exit code $LASTEXITCODE"
    }
}

# Returns the trimmed output of a git command, or $null when git is missing or the command fails.
function Get-GitOutput([string[]]$GitArgs) {
    if (-not (Get-Command git -ErrorAction SilentlyContinue)) { return $null }
    $previous = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    try {
        $output = & git @GitArgs 2>$null
        if ($LASTEXITCODE -ne 0) { return $null }
        if ($null -eq $output) { return '' }
        return (($output | ForEach-Object { [string]$_ }) -join "`n").Trim()
    }
    finally {
        $ErrorActionPreference = $previous
    }
}

# ---- tar.gz writer ----------------------------------------------------------------------------------
# Compress-Archive and tar.exe on Windows cannot store the Unix executable bit, so the .tar.gz is
# written here: a plain ustar archive, owner root, binary 0755, everything else 0644, gzip-compressed.

function Set-TarField([byte[]]$Header, [int]$Offset, [int]$Length, [string]$Text) {
    $bytes = [System.Text.Encoding]::ASCII.GetBytes($Text)
    if ($bytes.Length -gt $Length) { throw "tar field is too long: $Text" }
    [Array]::Copy($bytes, 0, $Header, $Offset, $bytes.Length)
}

# Writes a number as zero-padded octal text; the last byte of the field stays NUL.
function Set-TarOctal([byte[]]$Header, [int]$Offset, [int]$Length, [long]$Value) {
    $digits = [Convert]::ToString($Value, 8).PadLeft($Length - 1, '0')
    if ($digits.Length -ge $Length) { throw "tar number is too large: $Value" }
    Set-TarField $Header $Offset ($Length - 1) $digits
}

function New-TarHeader([string]$Name, [string]$Type, [int]$Mode, [long]$Size, [DateTime]$ModifiedUtc) {
    if ([System.Text.Encoding]::ASCII.GetByteCount($Name) -gt 100) { throw "tar entry name is too long: $Name" }
    $epoch = New-Object DateTime 1970, 1, 1, 0, 0, 0, ([DateTimeKind]::Utc)
    $header = New-Object byte[] 512
    Set-TarField $header 0 100 $Name
    Set-TarOctal $header 100 8 $Mode
    Set-TarOctal $header 108 8 0
    Set-TarOctal $header 116 8 0
    Set-TarOctal $header 124 12 $Size
    Set-TarOctal $header 136 12 ([long][Math]::Floor(($ModifiedUtc - $epoch).TotalSeconds))
    for ($i = 148; $i -lt 156; $i++) { $header[$i] = 32 }   # the checksum is computed over spaces
    Set-TarField $header 156 1 $Type
    Set-TarField $header 257 5 'ustar'                       # byte 262 stays NUL: magic is "ustar\0"
    Set-TarField $header 263 2 '00'
    Set-TarField $header 265 32 'root'
    Set-TarField $header 297 32 'root'
    $sum = 0
    foreach ($b in $header) { $sum += $b }
    Set-TarField $header 148 6 ([Convert]::ToString($sum, 8).PadLeft(6, '0'))
    $header[154] = 0
    $header[155] = 32
    return , $header
}

# ---- zip writer -------------------------------------------------------------------------------------
# ZipFile.CreateFromDirectory writes backslashes as path separators in Windows PowerShell 5.1, but the
# zip format requires forward slashes, and other systems would create files with a backslash in the name.

# Packs the contents of $SourceDir below the top-level folder $RootName.
function New-ZipArchive([string]$SourceDir, [string]$RootName, [string]$ArchivePath) {
    $base = (Resolve-Path -LiteralPath $SourceDir).Path.TrimEnd('\', '/')
    $file = [System.IO.File]::Create($ArchivePath)
    try {
        $zip = New-Object System.IO.Compression.ZipArchive($file, [System.IO.Compression.ZipArchiveMode]::Create)
        try {
            foreach ($item in (Get-ChildItem -LiteralPath $SourceDir -Recurse -File | Sort-Object FullName)) {
                $relative = $item.FullName.Substring($base.Length + 1).Replace('\', '/')
                $entry = $zip.CreateEntry("$RootName/$relative", [System.IO.Compression.CompressionLevel]::Optimal)
                $entry.LastWriteTime = [DateTimeOffset]$item.LastWriteTime
                $source = [System.IO.File]::OpenRead($item.FullName)
                try {
                    $sink = $entry.Open()
                    try { $source.CopyTo($sink) } finally { $sink.Dispose() }
                }
                finally {
                    $source.Dispose()
                }
            }
        }
        finally {
            $zip.Dispose()
        }
    }
    finally {
        $file.Dispose()
    }
}

function Write-TarDirectory([System.IO.Stream]$Stream, [string]$Name, [DateTime]$ModifiedUtc) {
    $header = New-TarHeader -Name ($Name.TrimEnd('/') + '/') -Type '5' -Mode 493 -Size 0 -ModifiedUtc $ModifiedUtc
    $Stream.Write($header, 0, 512)
}

function Write-TarFile([System.IO.Stream]$Stream, [string]$Name, [string]$Path, [int]$Mode) {
    $data = [System.IO.File]::ReadAllBytes($Path)
    $modified = (Get-Item -LiteralPath $Path).LastWriteTimeUtc
    $header = New-TarHeader -Name $Name -Type '0' -Mode $Mode -Size $data.Length -ModifiedUtc $modified
    $Stream.Write($header, 0, 512)
    $Stream.Write($data, 0, $data.Length)
    $padding = (512 - ($data.Length % 512)) % 512
    if ($padding -gt 0) {
        $Stream.Write((New-Object byte[] $padding), 0, $padding)
    }
}

# Packs the contents of $SourceDir below the top-level folder $RootName.
function New-TarGz([string]$SourceDir, [string]$RootName, [string]$ArchivePath, [string]$ExecutableName) {
    $base = (Resolve-Path -LiteralPath $SourceDir).Path.TrimEnd('\', '/')
    $file = [System.IO.File]::Create($ArchivePath)
    try {
        $gzip = New-Object System.IO.Compression.GZipStream($file, [System.IO.Compression.CompressionLevel]::Optimal)
        try {
            Write-TarDirectory $gzip $RootName (Get-Item -LiteralPath $SourceDir).LastWriteTimeUtc
            foreach ($dir in (Get-ChildItem -LiteralPath $SourceDir -Recurse -Directory | Sort-Object FullName)) {
                $relative = $dir.FullName.Substring($base.Length + 1).Replace('\', '/')
                Write-TarDirectory $gzip "$RootName/$relative" $dir.LastWriteTimeUtc
            }
            foreach ($item in (Get-ChildItem -LiteralPath $SourceDir -Recurse -File | Sort-Object FullName)) {
                $relative = $item.FullName.Substring($base.Length + 1).Replace('\', '/')
                $mode = 420   # 0644
                if ($item.Name -eq $ExecutableName) { $mode = 493 }   # 0755
                Write-TarFile $gzip "$RootName/$relative" $item.FullName $mode
            }
            $gzip.Write((New-Object byte[] 1024), 0, 1024)   # end of archive: two empty blocks
        }
        finally {
            $gzip.Dispose()
        }
    }
    finally {
        $file.Dispose()
    }
}

# ---- main -------------------------------------------------------------------------------------------

Push-Location $RepoRoot
$savedEnvironment = @{}
foreach ($name in 'CGO_ENABLED', 'GOOS', 'GOARCH') {
    $savedEnvironment[$name] = [Environment]::GetEnvironmentVariable($name, 'Process')
}
$stageRoot = $null

try {
    if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
        throw 'go was not found in PATH'
    }
    Add-Type -AssemblyName System.IO.Compression

    if (-not $Version) {
        $Version = Get-GitOutput @('describe', '--tags', '--always', '--dirty')
        if (-not $Version) { $Version = 'dev' }
    }
    if ($Version -notmatch '^[A-Za-z0-9][A-Za-z0-9._+-]*$') {
        throw "invalid version '$Version': use letters, digits and the characters . _ + - only"
    }

    $targetList = @($Targets | ForEach-Object { $_ -split ',' } | ForEach-Object { $_.Trim() } | Where-Object { $_ })
    if ($targetList.Count -eq 0) { throw 'no target given' }
    foreach ($target in $targetList) {
        if ($Supported -notcontains $target) {
            throw "unsupported target '$target': use one of $($Supported -join ', ')"
        }
    }

    if (Get-GitOutput @('status', '--porcelain')) {
        Write-Warning 'The working tree has uncommitted changes: the package does not match a commit.'
    }
    $commit = Get-GitOutput @('rev-parse', 'HEAD')
    if (-not $commit) { $commit = 'unknown' }

    if (-not $SkipTests) {
        Write-Step 'go vet ./...'
        & go vet ./...
        Assert-ExitCode 'go vet'
        Write-Step 'go test ./...'
        & go test -count=1 ./...
        Assert-ExitCode 'go test'
    }
    Write-Step 'go mod verify'
    & go mod verify
    Assert-ExitCode 'go mod verify'

    $outRoot = $OutDir
    if (-not [System.IO.Path]::IsPathRooted($outRoot)) { $outRoot = Join-Path $RepoRoot $outRoot }
    $versionDir = Join-Path $outRoot $Version
    if (Test-Path -LiteralPath $versionDir) {
        Write-Step "removing the previous build of $Version"
        Remove-Item -LiteralPath $versionDir -Recurse -Force
    }
    $stageRoot = Join-Path $versionDir '.stage'
    New-Item -ItemType Directory -Path $stageRoot -Force | Out-Null

    $goVersion = ((& go version) | Out-String).Trim()
    $builtAt = [DateTime]::UtcNow.ToString("yyyy-MM-dd'T'HH:mm:ss'Z'")

    foreach ($target in $targetList) {
        $goos, $goarch = $target -split '/'
        $packageName = "gophant_${Version}_${goos}_${goarch}"
        $stage = Join-Path $stageRoot $packageName
        $binary = 'gophant'
        if ($goos -eq 'windows') { $binary = 'gophant.exe' }

        Write-Step "building $packageName"
        New-Item -ItemType Directory -Path (Join-Path $stage 'configs'), (Join-Path $stage 'docs') -Force | Out-Null
        $env:CGO_ENABLED = '0'
        $env:GOOS = $goos
        $env:GOARCH = $goarch
        & go build -trimpath -ldflags "-s -w -X main.version=$Version" -o (Join-Path $stage $binary) ./cmd/gophant
        Assert-ExitCode "go build $target"

        Copy-Item -LiteralPath (Join-Path $RepoRoot 'configs/gophant.example.yaml') -Destination (Join-Path $stage 'configs')
        Copy-Item -LiteralPath (Join-Path $RepoRoot 'README.md') -Destination $stage
        Get-ChildItem -LiteralPath (Join-Path $RepoRoot 'docs') -Filter '*.md' -File |
            Copy-Item -Destination (Join-Path $stage 'docs')
        $info = @(
            "gophant $Version",
            "commit: $commit",
            "built:  $builtAt",
            "go:     $goVersion",
            "target: $target"
        ) -join "`n"
        [System.IO.File]::WriteAllText((Join-Path $stage 'BUILDINFO.txt'), $info + "`n", (New-Object System.Text.UTF8Encoding($false)))

        if ($goos -eq 'windows') {
            New-ZipArchive -SourceDir $stage -RootName $packageName -ArchivePath (Join-Path $versionDir "$packageName.zip")
        }
        else {
            New-TarGz -SourceDir $stage -RootName $packageName -ArchivePath (Join-Path $versionDir "$packageName.tar.gz") -ExecutableName $binary
        }
    }

    Write-Step 'writing SHA256SUMS.txt'
    $lines = foreach ($archiveFile in (Get-ChildItem -LiteralPath $versionDir -File | Sort-Object Name)) {
        $hash = (Get-FileHash -Algorithm SHA256 -LiteralPath $archiveFile.FullName).Hash.ToLowerInvariant()
        "$hash  $($archiveFile.Name)"
    }
    [System.IO.File]::WriteAllText((Join-Path $versionDir 'SHA256SUMS.txt'), (($lines -join "`n") + "`n"), (New-Object System.Text.UTF8Encoding($false)))

    Write-Host ''
    Write-Host "Release $Version is ready in $versionDir" -ForegroundColor Green
    foreach ($archiveFile in (Get-ChildItem -LiteralPath $versionDir -File | Sort-Object Name)) {
        if ($archiveFile.Length -ge 1MB) { $size = '{0:N1} MB' -f ($archiveFile.Length / 1MB) } else { $size = '{0:N1} KB' -f ($archiveFile.Length / 1KB) }
        Write-Host ('  {0,-48} {1,10}' -f $archiveFile.Name, $size)
    }
}
finally {
    foreach ($name in $savedEnvironment.Keys) {
        [Environment]::SetEnvironmentVariable($name, $savedEnvironment[$name], 'Process')
    }
    if ($stageRoot -and (Test-Path -LiteralPath $stageRoot)) {
        Remove-Item -LiteralPath $stageRoot -Recurse -Force -ErrorAction SilentlyContinue
    }
    Pop-Location
}
