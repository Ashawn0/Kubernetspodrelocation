# fetch-kubeconfig.ps1 — pull admin.conf from CP after up (or re-fetch).
param(
    [string]$KeyPath = "",
    [string]$Region = "ap-northeast-2"
)
$ErrorActionPreference = "Stop"
$Root = (Resolve-Path (Join-Path $PSScriptRoot "..")).Path
$OutJson = Join-Path $Root "terraform-outputs.json"
if (-not $KeyPath) { $KeyPath = Join-Path $env:USERPROFILE ".ssh\reloc-disrupt-key.pem" }
if (-not (Test-Path $OutJson)) { throw "run up.ps1 first (missing terraform-outputs.json)" }
$out = Get-Content $OutJson -Raw | ConvertFrom-Json
$cpPub = $out.cp_public_ip.value
$KubeDir = Join-Path $Root ".kube"
New-Item -ItemType Directory -Force -Path $KubeDir | Out-Null
$localKube = Join-Path $KubeDir "aws.conf"

$stamp = [guid]::NewGuid().ToString("N").Substring(0, 12)
$outFile = Join-Path $env:TEMP "reloc-scp-$stamp.out"
$errFile = Join-Path $env:TEMP "reloc-scp-$stamp.err"
try {
    $proc = Start-Process -FilePath "scp" -ArgumentList @(
        "-o", "StrictHostKeyChecking=no",
        "-o", "UserKnownHostsFile=/dev/null",
        "-o", "BatchMode=yes",
        "-i", $KeyPath,
        "ubuntu@${cpPub}:/home/ubuntu/.kube/config",
        $localKube
    ) -NoNewWindow -Wait -PassThru -RedirectStandardOutput $outFile -RedirectStandardError $errFile
    if ($proc.ExitCode -ne 0) {
        $err = Get-Content $errFile -Raw -ErrorAction SilentlyContinue
        throw "scp failed (exit $($proc.ExitCode)): $err"
    }
}
finally {
    Remove-Item -Force $outFile, $errFile -ErrorAction SilentlyContinue
}

$kc = Get-Content $localKube -Raw
$kc = $kc -replace "server: https://.*:6443", "server: https://${cpPub}:6443"
Set-Content -Path $localKube -Value $kc -NoNewline
Write-Host "Wrote $localKube"
