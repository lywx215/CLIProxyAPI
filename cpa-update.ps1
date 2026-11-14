[CmdletBinding()]
param(
    [Parameter(Mandatory = $true, Position = 0)]
    [ValidateSet('init', 'plan', 'update', 'status', 'resume', 'pause', 'report')]
    [string]$Action,
    [string[]]$Targets,
    [string]$Commit,
    [string]$Plan,
    [string]$Run,
    [string]$ExcludeModelTest,
    [string]$Inventory,
    [string]$TargetOptions,
    [string]$StateDirectory,
    [string]$Python = $env:CPA_OPS_PYTHON
)

$ErrorActionPreference = 'Stop'
if (-not $Python) {
    $pythonCommand = Get-Command python -CommandType Application -ErrorAction SilentlyContinue | Select-Object -First 1
    if (-not $pythonCommand) { throw 'Python 3.11+ is required. Specify -Python or CPA_OPS_PYTHON.' }
    $Python = $pythonCommand.Source
}
$cliArguments = @('-B', '-m', 'scripts.ops.cli')
if ($StateDirectory) { $cliArguments += @('--state-dir', $StateDirectory) }
$cliArguments += $Action
if ($Targets) { $cliArguments += @('--targets', ($Targets -join ',')) }
if ($Commit) { $cliArguments += @('--commit', $Commit) }
if ($Plan) { $cliArguments += @('--plan', $Plan) }
if ($Run) { $cliArguments += @('--run', $Run) }
if ($ExcludeModelTest) { $cliArguments += @('--exclude-model-test', $ExcludeModelTest) }
if ($Inventory) { $cliArguments += @('--inventory', $Inventory) }
if ($TargetOptions) { $cliArguments += @('--target-options', $TargetOptions) }

Push-Location -LiteralPath $PSScriptRoot
try {
    & $Python @cliArguments
    $resultCode = $LASTEXITCODE
} finally {
    Pop-Location
}
exit $resultCode
