param(
  [string]$Distro = "Ubuntu-22.04",
  [Parameter(Mandatory=$true)][string]$RepoPath,
  [Parameter(Mandatory=$true)][string]$CampaignId,
  [Parameter(Mandatory=$true)][ValidatePattern('^[0-9a-f]{40}$')][string]$FrozenCommit,
  [Parameter(Mandatory=$true)][string]$PrivateConfigDir,
  [Parameter(Mandatory=$true)][string]$DatasetBindingsDir,
  [ValidateSet(3)][int]$Deployments = 3
)
$ErrorActionPreference = "Stop"
function ConvertTo-BashLiteral([string]$Value) {
  $quote = [string][char]39
  $replacement = $quote + [char]34 + $quote + [char]34 + $quote
  return $quote + $Value.Replace($quote, $replacement) + $quote
}
$manifest = [ordered]@{
  schema_version = 1; campaign_id = $CampaignId; frozen_commit = $FrozenCommit; windows_host_sha256 = ""
  windows = [ordered]@{
    edition = (Get-ComputerInfo).WindowsProductName; version = (Get-ComputerInfo).WindowsVersion
    build = (Get-ComputerInfo).OsBuildNumber; cpu = (Get-CimInstance Win32_Processor | Select-Object -ExpandProperty Name)
    logical_processors = (Get-CimInstance Win32_ComputerSystem).NumberOfLogicalProcessors
    physical_ram_bytes = (Get-CimInstance Win32_ComputerSystem).TotalPhysicalMemory
    power_plan = (powercfg /getactivescheme | Out-String).Trim(); wsl_version = (wsl --version | Out-String)
    wsl_status = (wsl --status | Out-String); docker_desktop = ((Get-Item "$Env:ProgramFiles\Docker\Docker\Docker Desktop.exe" -ErrorAction SilentlyContinue).VersionInfo.FileVersion)
  }; deployments = @()
}
$wslConfig = Join-Path $Env:USERPROFILE ".wslconfig"
if (Test-Path $wslConfig) { $manifest.windows.wslconfig_sha256 = (Get-FileHash $wslConfig -Algorithm SHA256).Hash.ToLowerInvariant() }
$windowsHostPath = "taskgate-$CampaignId-windows-host.json"
$windowsHostJson = $manifest.windows | ConvertTo-Json -Depth 8
[IO.File]::WriteAllText($windowsHostPath, $windowsHostJson + [Environment]::NewLine, [Text.UTF8Encoding]::new($false))
$windowsHostSHA256 = (Get-FileHash $windowsHostPath -Algorithm SHA256).Hash.ToLowerInvariant()
$windowsHostBase64 = [Convert]::ToBase64String([IO.File]::ReadAllBytes($windowsHostPath))
$manifest.windows_host_sha256 = $windowsHostSHA256
for ($i=1; $i -le $Deployments; $i++) {
  wsl.exe --shutdown
  $deploymentId = "deployment-{0:d2}" -f $i
  $bindings = "$DatasetBindingsDir/$deploymentId.json"
  $command = "cd $(ConvertTo-BashLiteral $RepoPath) && TASKGATE_EXPERIMENT_CLASS=publication TASKGATE_SUBMISSION_COMMIT=$(ConvertTo-BashLiteral $FrozenCommit) TASKGATE_CAMPAIGN_ID=$(ConvertTo-BashLiteral $CampaignId) TASKGATE_DEPLOYMENT_ID=$(ConvertTo-BashLiteral $deploymentId) TASKGATE_PRIVATE_CONFIG_DIR=$(ConvertTo-BashLiteral $PrivateConfigDir) TASKGATE_DATASET_BINDINGS=$(ConvertTo-BashLiteral $bindings) TASKGATE_WINDOWS_ENVIRONMENT_SHA256=$(ConvertTo-BashLiteral $windowsHostSHA256) TASKGATE_WINDOWS_ENVIRONMENT_BASE64=$(ConvertTo-BashLiteral $windowsHostBase64) TASKGATE_FRESH_DEPLOYMENT=1 evaluation/final-v5-wsl2/scripts/run-deployment.sh"
  wsl.exe -d $Distro -- bash -lc $command
  $status = $LASTEXITCODE
  $manifest.deployments += [ordered]@{ deployment_id=$deploymentId; exit_status=$status }
  wsl.exe --shutdown
  if ($status -ne 0) { break }
}
$json = $manifest | ConvertTo-Json -Depth 8
$path = "taskgate-$CampaignId-windows-environment.json"
[IO.File]::WriteAllText($path, $json + [Environment]::NewLine, [Text.UTF8Encoding]::new($false))
(Get-FileHash $path -Algorithm SHA256).Hash.ToLowerInvariant() | Set-Content "$path.sha256" -NoNewline
