# install-centrol.ps1 - installs the centrol CLI and the centrol-verify
# reference verifier on Windows. No telemetry, no account, no network
# access beyond fetching this release's binaries and its SHA256SUMS file.
#
# Installs to $env:LOCALAPPDATA\Programs\centrol (override with
# $env:CENTROL_INSTALL_DIR). That is a per-user directory, so no
# administrator rights are needed. Both binaries are checked against
# SHA256SUMS before either is installed. The script never edits PATH; it
# tells you if the directory is not on it.
#
#   irm https://raw.githubusercontent.com/baitelmal/centrol/main/install-centrol.ps1 | iex

function Install-Centrol {
    $ErrorActionPreference = 'Stop'
    $ProgressPreference = 'SilentlyContinue'   # Windows PowerShell 5.1 downloads crawl with the progress bar on

    # Windows PowerShell 5.1 may default to TLS versions GitHub refuses.
    try {
        [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12
    } catch { }

    $repo = 'baitelmal/centrol'
    $baseUrl = "https://github.com/$repo/releases/latest/download"

    # A 32-bit PowerShell on 64-bit Windows reports x86 in PROCESSOR_ARCHITECTURE;
    # PROCESSOR_ARCHITEW6432 then holds the real one.
    $machine = if ($env:PROCESSOR_ARCHITEW6432) { $env:PROCESSOR_ARCHITEW6432 } else { $env:PROCESSOR_ARCHITECTURE }
    switch ($machine) {
        'AMD64' { $arch = 'amd64' }
        'ARM64' { $arch = 'arm64' }
        default { throw "centrol: unsupported architecture: $machine" }
    }

    if ($env:CENTROL_INSTALL_DIR) {
        $installDir = $env:CENTROL_INSTALL_DIR
    } else {
        $installDir = Join-Path $env:LOCALAPPDATA 'Programs\centrol'
    }

    $tmpDir = Join-Path ([IO.Path]::GetTempPath()) ('centrol-install-' + [Guid]::NewGuid().ToString('N'))
    New-Item -ItemType Directory -Path $tmpDir | Out-Null

    try {
        $sumsPath = Join-Path $tmpDir 'SHA256SUMS'
        Invoke-WebRequest -UseBasicParsing -Uri "$baseUrl/SHA256SUMS" -OutFile $sumsPath

        # SHA256SUMS lines are "<hash>  <name>" (a leading * on the name marks binary mode).
        $expected = @{}
        foreach ($line in Get-Content -LiteralPath $sumsPath) {
            $parts = $line.Trim() -split '\s+', 2
            if ($parts.Count -eq 2) { $expected[$parts[1].TrimStart('*')] = $parts[0].ToLower() }
        }

        # Download and verify everything first; install only if all of it checks out.
        $names = @('centrol', 'centrol-verify')
        foreach ($name in $names) {
            $asset = "$name-windows-$arch.exe"
            Write-Host "centrol: downloading $asset..."
            Invoke-WebRequest -UseBasicParsing -Uri "$baseUrl/$asset" -OutFile (Join-Path $tmpDir $asset)

            if (-not $expected.ContainsKey($asset)) {
                throw "centrol: $asset not listed in SHA256SUMS - refusing to install an unverifiable binary"
            }
            $actual = (Get-FileHash -Algorithm SHA256 -LiteralPath (Join-Path $tmpDir $asset)).Hash.ToLower()
            if ($actual -ne $expected[$asset]) {
                throw ("centrol: checksum mismatch for $asset`n  expected: $($expected[$asset])`n  actual:   $actual`n" +
                       "centrol: refusing to install a binary that doesn't match its published checksum")
            }
            Write-Host "centrol: checksum OK ($asset)"
        }

        New-Item -ItemType Directory -Force -Path $installDir | Out-Null
        foreach ($name in $names) {
            $asset = "$name-windows-$arch.exe"
            $dest = Join-Path $installDir "$name.exe"
            try {
                Move-Item -Force -LiteralPath (Join-Path $tmpDir $asset) -Destination $dest
            } catch {
                throw "centrol: could not write $dest (is it running? close it and re-run): $($_.Exception.Message)"
            }
            Write-Host "centrol: installed to $dest"
        }

        $onPath = ($env:PATH -split ';') | Where-Object { $_.TrimEnd('\') -ieq $installDir.TrimEnd('\') }
        if (-not $onPath) {
            Write-Host "centrol: $installDir is not on your PATH. To add it for your user, run:"
            Write-Host "  [Environment]::SetEnvironmentVariable('Path', [Environment]::GetEnvironmentVariable('Path','User') + ';$installDir', 'User')"
            Write-Host "then open a new terminal."
        }
    } finally {
        Remove-Item -Recurse -Force -LiteralPath $tmpDir -ErrorAction SilentlyContinue
    }
}

Install-Centrol
