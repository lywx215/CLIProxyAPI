# Deploy the current working tree to an existing Linux Docker host over SSH.
[CmdletBinding()]
param(
    [Parameter(Position = 0)]
    [ValidateSet('deploy', 'update', 'restart', 'logs', 'status', 'probe', 'package')]
    [string]$Action = 'deploy',
    [string]$Server,
    [string]$User,
    [int]$SshPort,
    [string]$Directory,
    [string]$Project,
    [string]$Domain,
    [int]$Port,
    [string]$IdentityFile,
    [string]$Profile,
    [string]$Output,
    [int]$Lines = 100,
    [switch]$Follow,
    [switch]$PasswordAuth,
    [switch]$PasswordStdin,
    [switch]$AcceptNewHostKey,
    [switch]$UseSudo
)

$ErrorActionPreference = 'Stop'
$taskArguments = @((Join-Path $PSScriptRoot 'scripts/vps/deploy.py'), $Action)
$taskOptions = @{
    Server = '--host'; User = '--user'; SshPort = '--ssh-port'
    Directory = '--directory'; Project = '--project'; Port = '--port'
    Domain = '--domain'
    IdentityFile = '--identity'; Profile = '--profile'; Output = '--output'
}
foreach ($taskName in $taskOptions.Keys) {
    if ($PSBoundParameters.ContainsKey($taskName)) {
        $taskArguments += @($taskOptions[$taskName], [string]$PSBoundParameters[$taskName])
    }
}
$taskArguments += @('--lines', [string]$Lines)
if ($Follow) { $taskArguments += '--follow' }
if ($PasswordAuth) { $taskArguments += '--password' }
if ($PasswordStdin) { $taskArguments += '--password-stdin' }
if ($AcceptNewHostKey) { $taskArguments += '--accept-new-host-key' }
if ($UseSudo) { $taskArguments += '--sudo' }

& python @taskArguments
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
