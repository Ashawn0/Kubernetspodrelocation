# up.ps1 — one-command AWS cluster bring-up (terraform apply + bootstrap).
# Prerequisites: aws CLI (reloc-disrupt-admin), terraform, key pair reloc-disrupt-key.
# Usage:  .\up.ps1
# Cost:   run .\down.ps1 when done ($85 alert = stop signal).

param(
    [string]$KeyPath = "",
    [string]$Region = "ap-northeast-2",
    [string]$OperatorCidr = "",
    [switch]$SkipBootstrap
)

$ErrorActionPreference = "Stop"
$Root = $PSScriptRoot
$TfDir = Join-Path $Root "terraform"
$Scripts = Join-Path $Root "scripts"
$KubeDir = Join-Path $Root ".kube"
$OutJson = Join-Path $Root "terraform-outputs.json"
$Tfvars = Join-Path $TfDir "terraform.tfvars"

if (-not $KeyPath) {
    $KeyPath = Join-Path $env:USERPROFILE ".ssh\reloc-disrupt-key.pem"
}
if (-not (Test-Path $KeyPath)) {
    throw "SSH private key not found at $KeyPath. Pass -KeyPath or place reloc-disrupt-key.pem under ~/.ssh/"
}

function Require-Cmd($name) {
    if (-not (Get-Command $name -ErrorAction SilentlyContinue)) {
        throw "$name not found on PATH"
    }
}

Require-Cmd aws
Require-Cmd terraform
Require-Cmd ssh
Require-Cmd scp

# --- operator CIDR: required, never 0.0.0.0/0 ---
if (-not $OperatorCidr) {
    try {
        $pub = (Invoke-RestMethod -Uri "https://checkip.amazonaws.com" -TimeoutSec 10).Trim()
        if ($pub -match '^\d+\.\d+\.\d+\.\d+$') {
            $OperatorCidr = "$pub/32"
            Write-Host "Auto-detected operator_cidr=$OperatorCidr (from checkip.amazonaws.com)"
        }
    } catch {
        Write-Host "Could not auto-detect public IP: $_"
    }
}
if (-not $OperatorCidr) {
    throw "Pass -OperatorCidr x.x.x.x/32 (your public IP). Refusing open 0.0.0.0/0 for SSH/6443."
}
if ($OperatorCidr -eq "0.0.0.0/0") {
    throw "operator_cidr must not be 0.0.0.0/0 (kube-apiserver must not be world-open)."
}

@"
region         = "$Region"
key_name       = "reloc-disrupt-key"
operator_cidr  = "$OperatorCidr"
"@ | Set-Content -Path $Tfvars -Encoding utf8
Write-Host "Wrote $Tfvars with operator_cidr=$OperatorCidr (gitignored)"

$env:AWS_DEFAULT_REGION = $Region
Write-Host "=== terraform apply ($Region) SG 22+6443 locked to $OperatorCidr ==="
Push-Location $TfDir
try {
    terraform init -input=false
    terraform apply -auto-approve -input=false
    terraform output -json | Set-Content -Path $OutJson -Encoding utf8
}
finally {
    Pop-Location
}

$out = Get-Content $OutJson -Raw | ConvertFrom-Json
$cpPub = $out.cp_public_ip.value
$workerPubs = @($out.worker_public_ips.value)
$workerPrivs = @($out.worker_private_ips.value)
$regIps = @($out.worker_registry_private_ips.value)

Write-Host "CP public=$cpPub"
Write-Host "Workers public=$($workerPubs -join ',') registry=$($regIps -join ',')"

# Native ssh/scp write "Permanently added ..." to stderr; under $ErrorActionPreference=Stop
# PowerShell can promote that to NativeCommandError. Always use Start-Process + ExitCode.
function Invoke-Native {
    param(
        [Parameter(Mandatory = $true)][string]$FilePath,
        [Parameter(Mandatory = $true)][string[]]$ArgumentList,
        [switch]$ThrowOnError,
        [string]$FailMessage = ""
    )
    $stamp = [guid]::NewGuid().ToString("N").Substring(0, 12)
    $outFile = Join-Path $env:TEMP "reloc-native-$stamp.out"
    $errFile = Join-Path $env:TEMP "reloc-native-$stamp.err"
    try {
        $proc = Start-Process -FilePath $FilePath -ArgumentList $ArgumentList `
            -NoNewWindow -Wait -PassThru `
            -RedirectStandardOutput $outFile -RedirectStandardError $errFile
        $stdout = ""
        $stderr = ""
        if (Test-Path $outFile) { $stdout = Get-Content $outFile -Raw -ErrorAction SilentlyContinue }
        if (Test-Path $errFile) { $stderr = Get-Content $errFile -Raw -ErrorAction SilentlyContinue }
        if ($null -eq $stdout) { $stdout = "" }
        if ($null -eq $stderr) { $stderr = "" }
        if ($ThrowOnError -and $proc.ExitCode -ne 0) {
            $msg = $FailMessage
            if (-not $msg) { $msg = "$FilePath failed (exit $($proc.ExitCode))" }
            throw "$msg`n--- stdout ---`n$stdout`n--- stderr ---`n$stderr"
        }
        return [pscustomobject]@{
            ExitCode = $proc.ExitCode
            StdOut   = $stdout
            StdErr   = $stderr
        }
    }
    finally {
        Remove-Item -Force $outFile, $errFile -ErrorAction SilentlyContinue
    }
}

function Ssh-Args([string]$ip, [string]$remoteCmd, [int]$ConnectTimeout = 0) {
    $a = @(
        "-o", "StrictHostKeyChecking=no",
        "-o", "UserKnownHostsFile=/dev/null",
        "-o", "BatchMode=yes",
        "-i", $KeyPath
    )
    if ($ConnectTimeout -gt 0) {
        $a += @("-o", "ConnectTimeout=$ConnectTimeout")
    }
    $a += @("ubuntu@$ip", $remoteCmd)
    return , $a
}

function Wait-Ssh($ip) {
    for ($i = 0; $i -lt 60; $i++) {
        $r = Invoke-Native -FilePath "ssh" -ArgumentList (Ssh-Args $ip "echo ok" -ConnectTimeout 5)
        if ($r.ExitCode -eq 0) { return }
        Start-Sleep -Seconds 5
    }
    throw "SSH timeout waiting for $ip"
}

function Invoke-Remote($ip, $cmd) {
    $delays = @(5, 10, 15)
    $maxAttempts = 1 + $delays.Count
    $last = $null
    for ($attempt = 1; $attempt -le $maxAttempts; $attempt++) {
        $last = Invoke-Native -FilePath "ssh" -ArgumentList (Ssh-Args $ip $cmd)
        if ($last.ExitCode -eq 0) {
            if ($last.StdOut.Trim()) { Write-Host $last.StdOut.TrimEnd() }
            return $last.StdOut
        }
        if ($attempt -lt $maxAttempts) {
            $delay = $delays[$attempt - 1]
            Write-Host ("SSH retry {0}/{1} to {2} after exit {3}; sleeping {4}s..." -f `
                $attempt, ($maxAttempts - 1), $ip, $last.ExitCode, $delay)
            if ($last.StdErr.Trim()) { Write-Host ("  stderr: " + ($last.StdErr.Trim() -replace "`r?`n", " | ")) }
            Start-Sleep -Seconds $delay
        }
    }
    throw ("remote failed on {0} after {1} attempts : {2}`n--- stdout ---`n{3}`n--- stderr ---`n{4}" -f `
        $ip, $maxAttempts, $cmd, $last.StdOut, $last.StdErr)
}

function Send-File($ip, $local, $remote) {
    $delays = @(5, 10, 15)
    $maxAttempts = 1 + $delays.Count
    $scpArgs = @(
        "-o", "StrictHostKeyChecking=no",
        "-o", "UserKnownHostsFile=/dev/null",
        "-o", "BatchMode=yes",
        "-i", $KeyPath,
        $local,
        "ubuntu@${ip}:${remote}"
    )
    $last = $null
    for ($attempt = 1; $attempt -le $maxAttempts; $attempt++) {
        $last = Invoke-Native -FilePath "scp" -ArgumentList $scpArgs
        if ($last.ExitCode -eq 0) { return }
        if ($attempt -lt $maxAttempts) {
            $delay = $delays[$attempt - 1]
            Write-Host ("SCP retry {0}/{1} {2} -> {3}:{4} after exit {5}; sleeping {6}s..." -f `
                $attempt, ($maxAttempts - 1), $local, $ip, $remote, $last.ExitCode, $delay)
            if ($last.StdErr.Trim()) { Write-Host ("  stderr: " + ($last.StdErr.Trim() -replace "`r?`n", " | ")) }
            Start-Sleep -Seconds $delay
        }
    }
    throw ("scp failed after {0} attempts {1} -> {2}:{3}`n--- stdout ---`n{4}`n--- stderr ---`n{5}" -f `
        $maxAttempts, $local, $ip, $remote, $last.StdOut, $last.StdErr)
}

function Receive-File($ip, $remote, $local) {
    $null = Invoke-Native -FilePath "scp" -ArgumentList @(
        "-o", "StrictHostKeyChecking=no",
        "-o", "UserKnownHostsFile=/dev/null",
        "-o", "BatchMode=yes",
        "-i", $KeyPath,
        "ubuntu@${ip}:${remote}",
        $local
    ) -ThrowOnError -FailMessage "scp failed ${ip}:${remote} -> $local"
}

Write-Host "=== waiting for SSH ==="
Wait-Ssh $cpPub
foreach ($ip in $workerPubs) { Wait-Ssh $ip }

$bashScripts = @(
    "k8s-node-common.sh",
    "aws-node-prep.sh",
    "bootstrap-cluster.sh",
    "shape-registry-eni.sh"
)
$pathsEnv = Join-Path $Root "config\paths.env"
$toNormalize = @()
foreach ($s in $bashScripts) { $toNormalize += (Join-Path $Scripts $s) }
$toNormalize += $pathsEnv
foreach ($p in $toNormalize) {
    $raw = [System.IO.File]::ReadAllText($p) -replace "`r`n", "`n" -replace "`r", "`n"
    $utf8 = New-Object System.Text.UTF8Encoding $false
    [System.IO.File]::WriteAllText($p, $raw, $utf8)
}

Write-Host "=== push scripts + run k8s-node-common on all nodes ==="
$allPub = @($cpPub) + $workerPubs
foreach ($ip in $allPub) {
    foreach ($s in $bashScripts) {
        Send-File $ip (Join-Path $Scripts $s) "/tmp/$s"
    }
    Send-File $ip $pathsEnv "/tmp/paths.env"
    Invoke-Remote $ip "sudo bash -c 'set -a; . /tmp/paths.env; set +a; bash /tmp/k8s-node-common.sh'"
}

Write-Host "=== AWS node prep (NVMe + secondary ENI on workers) ==="
Invoke-Remote $cpPub "sudo bash -c 'set -a; . /tmp/paths.env; set +a; bash /tmp/aws-node-prep.sh control-plane'"
foreach ($ip in $workerPubs) {
    Invoke-Remote $ip "sudo bash -c 'set -a; . /tmp/paths.env; set +a; bash /tmp/aws-node-prep.sh worker'"
}

if ($SkipBootstrap) {
    Write-Host "SkipBootstrap set; nodes prepped but kubeadm not run."
    Write-Host "Outputs: $OutJson"
    exit 0
}

# Private key stays on the operator machine only — never copied to the CP.
# Join: capture token from CP stdout, run kubeadm join on each worker via Invoke-Remote.

Write-Host "=== kubeadm init + Calico on CP (extra-san=$cpPub) ==="
$podCidr = "10.244.0.0/16"
# Pass CP *public* IP as EXTRA_SAN ($2). Advertise address stays the private IP inside the script.
$boot = Invoke-Native -FilePath "ssh" -ArgumentList (Ssh-Args $cpPub "bash /tmp/bootstrap-cluster.sh $podCidr $cpPub")
$bootOut = $boot.StdOut + "`n" + $boot.StdErr
Write-Host $bootOut
if ($boot.ExitCode -ne 0) {
    throw "bootstrap-cluster.sh failed on CP (exit $($boot.ExitCode)); JOIN markers not used"
}

$joinCmd = $null
if ($bootOut -match '(?s)JOIN_BEGIN\s*\r?\n(.+?)\r?\nJOIN_END') {
    $joinCmd = $Matches[1].Trim()
}
if (-not $joinCmd -or $joinCmd -notmatch '^kubeadm join\s') {
    throw "JOIN_BEGIN/JOIN_END missing or invalid (kubeadm init/Calico likely failed before token print). Refusing to join workers."
}
Write-Host "=== join command captured (running on workers from this machine; key never leaves here) ==="
Write-Host $joinCmd

foreach ($ip in $workerPubs) {
    Write-Host "Joining worker $ip ..."
    # Quote carefully: join cmd has spaces; run under bash -c on the worker.
    $remote = "sudo bash -c " + "'" + ($joinCmd -replace "'", "'\''") + "'"
    Invoke-Remote $ip $remote
}

Write-Host "=== waiting for nodes Ready (fail if not all Ready) ==="
$allReady = $false
for ($i = 0; $i -lt 60; $i++) {
    $r = Invoke-Native -FilePath "ssh" -ArgumentList (Ssh-Args $cpPub "kubectl get nodes --no-headers")
    if ($r.ExitCode -eq 0 -and $r.StdOut) {
        $lines = @($r.StdOut -split "`n" | Where-Object { $_.Trim() -ne "" })
        $ready = @($lines | Where-Object { $_ -match '\sReady\s' }).Count
        Write-Host ("  ready={0}/{1}" -f $ready, $lines.Count)
        if ($lines.Count -ge 3 -and $ready -eq $lines.Count) {
            $allReady = $true
            break
        }
    }
    Start-Sleep -Seconds 5
}
if (-not $allReady) {
    throw "nodes did not all reach Ready within timeout (Calico/join incomplete). Not declaring Cluster up."
}

Write-Host "=== fetch kubeconfig ==="
New-Item -ItemType Directory -Force -Path $KubeDir | Out-Null
$localKube = Join-Path $KubeDir "aws.conf"
Receive-File $cpPub "/home/ubuntu/.kube/config" $localKube
$kc = Get-Content $localKube -Raw
$kc = $kc -replace "server: https://.*:6443", "server: https://${cpPub}:6443"
Set-Content -Path $localKube -Value $kc -NoNewline

# Persist probe hints next to kubeconfig
@"
KUBECONFIG=$localKube
RELOC_IO_STRESS_PATH=/mnt/reloc-nvme
RELOC_PRIMARY_IFACE=ens5
RELOC_REGISTRY_IFACE=ens6
WORKER_REGISTRY_IPS=$($regIps -join ',')
"@ | Set-Content (Join-Path $KubeDir "env.txt")

Write-Host ""
Write-Host "Cluster up (all nodes Ready)."
Write-Host "  KUBECONFIG=$localKube"
Write-Host "  kubectl --kubeconfig $localKube get nodes -o wide"
Write-Host ""
Write-Host "IO PSI:  go run ./cmd/stage0/psiprobe -skip-io-isolation=false -io-path /mnt/reloc-nvme"
Write-Host "Net:     go run ./cmd/stage0/netprobe"
Write-Host "STOP COST:  .\down.ps1"
