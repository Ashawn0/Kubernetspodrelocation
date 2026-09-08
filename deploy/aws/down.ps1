# down.ps1 — one-command cost stop. Prefer this at the $85 alert.
# Usage:
#   .\down.ps1              # terraform destroy -auto-approve (full teardown)
#   .\down.ps1 -StopOnly    # ec2 stop only (EBS/ENI still bill; not preferred)

param(
    [switch]$StopOnly,
    [string]$Region = "ap-northeast-2"
)

$ErrorActionPreference = "Stop"
$Root = $PSScriptRoot
$TfDir = Join-Path $Root "terraform"
$OutJson = Join-Path $Root "terraform-outputs.json"

$env:AWS_DEFAULT_REGION = $Region

if ($StopOnly) {
    if (-not (Test-Path $OutJson)) {
        throw "terraform-outputs.json missing; cannot StopOnly without prior up.ps1"
    }
    $out = Get-Content $OutJson -Raw | ConvertFrom-Json
    $ids = @($out.cp_instance_id.value) + @($out.worker_instance_ids.value)
    Write-Host "Stopping instances: $($ids -join ', ')"
    aws ec2 stop-instances --instance-ids $ids --region $Region | Out-Null
    Write-Host "Stopped. EBS + Elastic IPs (if any) still accrue. Prefer .\down.ps1 without -StopOnly."
    exit 0
}

if (-not (Get-Command terraform -ErrorAction SilentlyContinue)) {
    throw "terraform not found on PATH"
}

Write-Host "=== terraform destroy -auto-approve ($Region) ==="
Write-Host "This removes CP, workers, ENIs, VPC. Confirm budget stop."
Push-Location $TfDir
try {
    terraform init -input=false
    terraform destroy -auto-approve -input=false
}
finally {
    Pop-Location
}

$kube = Join-Path $Root ".kube\aws.conf"
if (Test-Path $kube) {
    Remove-Item $kube -Force
}
Write-Host "Destroyed. Billing for these instances should stop shortly."
