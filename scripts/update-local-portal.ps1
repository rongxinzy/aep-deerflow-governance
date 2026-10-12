[CmdletBinding()]
param(
    [ValidateSet('plan', 'build', 'prepare', 'deploy', 'rollback')]
    [string]$Mode = 'plan',
    [Parameter(Mandatory = $true)]
    [string]$RepositoryPath,
    [Parameter(Mandatory = $true)]
    [ValidatePattern('^[0-9a-f]{40}$')]
    [string]$ExpectedCommit,
    [Parameter(Mandatory = $true)]
    [string]$RunRoot,
    [switch]$ConfirmDatabaseBackup
)

$ErrorActionPreference = 'Stop'

function ConvertTo-WslPath {
    param([Parameter(Mandatory = $true)][string]$WindowsPath)
    $absolutePath = [System.IO.Path]::GetFullPath($WindowsPath)
    if ($absolutePath -notmatch '^([A-Za-z]):\\(.+)$') {
        throw "Path must be on a mounted Windows drive: $absolutePath"
    }
    $drive = $Matches[1].ToLowerInvariant()
    $relative = $Matches[2].Replace('\', '/')
    return "/mnt/$drive/$relative"
}

if ($Mode -eq 'deploy' -and -not $ConfirmDatabaseBackup) {
    throw 'deploy requires -ConfirmDatabaseBackup after a verified Portal DB backup.'
}

$helperWin = Join-Path $PSScriptRoot 'update_local_portal.py'
if (-not (Test-Path -LiteralPath $helperWin -PathType Leaf)) {
    throw 'The local Portal update helper is missing.'
}

$helperWsl = ConvertTo-WslPath $helperWin
$repoWsl = ConvertTo-WslPath $RepositoryPath
$runRootWsl = ConvertTo-WslPath $RunRoot

if ($Mode -eq 'build') {
    & wsl.exe -d Ubuntu -u root -- python3 $helperWsl 'build-preflight' `
        '--repository-path' $repoWsl '--expected-commit' $ExpectedCommit '--run-root' $runRootWsl
    if ($LASTEXITCODE -ne 0) {
        throw 'Portal source/plan preflight failed; Go was not invoked.'
    }

    $goCommand = Get-Command 'go.exe' -ErrorAction Stop
    $portalPath = Join-Path ([System.IO.Path]::GetFullPath($RepositoryPath)) 'portal'
    $runRootPath = [System.IO.Path]::GetFullPath($RunRoot)
    $testBinary = Join-Path $runRootPath 'portal-tests-linux-amd64'
    $portalBinary = Join-Path $runRootPath 'de-portal'
    $oldEnv = @{}
    foreach ($name in @('GOOS', 'GOARCH', 'CGO_ENABLED')) {
        $oldEnv[$name] = [Environment]::GetEnvironmentVariable($name, 'Process')
    }
    try {
        $env:GOOS = 'linux'
        $env:GOARCH = 'amd64'
        $env:CGO_ENABLED = '0'
        Push-Location -LiteralPath $portalPath
        try {
            $packages = & $goCommand.Source list ./...
            if ($LASTEXITCODE -ne 0 -or @($packages).Count -ne 1) {
                throw 'Expected exactly one Portal Go package; refusing an incomplete test/build.'
            }
            & $goCommand.Source test -count=1 -c -o $testBinary .
            if ($LASTEXITCODE -ne 0) { throw 'Linux Portal test binary compilation failed.' }
            & $goCommand.Source vet ./...
            if ($LASTEXITCODE -ne 0) { throw 'Linux Portal go vet failed.' }
            & $goCommand.Source build -trimpath -o $portalBinary .
            if ($LASTEXITCODE -ne 0) { throw 'Linux/amd64 Portal build failed.' }
        } finally {
            Pop-Location
        }

        $testWsl = ConvertTo-WslPath $testBinary
        & wsl.exe -d Ubuntu -u root -- python3 $helperWsl 'run-tests' `
            '--repository-path' $repoWsl '--expected-commit' $ExpectedCommit '--run-root' $runRootWsl `
            '--test-binary' $testWsl
        if ($LASTEXITCODE -ne 0) { throw 'Linux Portal Go tests failed in WSL.' }
    } finally {
        foreach ($name in @('GOOS', 'GOARCH', 'CGO_ENABLED')) {
            [Environment]::SetEnvironmentVariable($name, $oldEnv[$name], 'Process')
        }
        Remove-Item -LiteralPath $testBinary -Force -ErrorAction SilentlyContinue
    }
}

$arguments = @(
    '-d', 'Ubuntu', '-u', 'root', '--', 'python3', $helperWsl, $Mode,
    '--repository-path', $repoWsl,
    '--expected-commit', $ExpectedCommit,
    '--run-root', $runRootWsl
)
if ($Mode -eq 'deploy') {
    $arguments += '--confirm-db-backup'
}

& wsl.exe @arguments
if ($LASTEXITCODE -ne 0) {
    throw "Portal update helper failed in mode '$Mode' (exit $LASTEXITCODE)."
}
