# provision-cluster.ps1
# Stands up a local kubeadm cluster (1 control-plane + N workers) via Multipass,
# for Stage 0 PSI-isolation and registry-shaping-rehearsal testing on this machine,
# ahead of lab/university kubeadm access.
#
# Requires: Multipass installed (https://multipass.run), Hyper-V enabled.
# Sibling assets are resolved via $PSScriptRoot so this script works from any cwd
# (e.g. .\deploy\local-vm\provision-cluster.ps1 from the repo root).

$ErrorActionPreference = "Stop"

$WorkerCount    = 2
$UbuntuRelease  = "22.04"
$ControlPlane   = "cp1"
$WorkerPrefix   = "worker"
$PodNetworkCidr = "10.244.0.0/16"   # matches the Calico manifest below

# Local files next to this script (never bare relative paths — caller's cwd varies).
$NodeCommonSh = Join-Path $PSScriptRoot "k8s-node-common.sh"
if (-not (Test-Path -LiteralPath $NodeCommonSh)) {
    throw "Missing node bootstrap script: $NodeCommonSh"
}

# Native multipass/kubeadm write routine notices to stderr (e.g. kubeadm
# "remote version is much newer...falling back to stable-1.30"). Under
# $ErrorActionPreference=Stop, PowerShell promotes that to a terminating
# NativeCommandError even when exit code is 0. Same fix as deploy/aws/up.ps1:
# Start-Process + redirected files + ExitCode only.
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
    # Start-Process -ArgumentList with a string[] does not reliably quote
    # elements that contain spaces; bash -c "mkdir -p ..." becomes bash -c mkdir.
    $argLine = ($ArgumentList | ForEach-Object {
        $a = [string]$_
        if ($a -match '[\s"]') {
            '"' + ($a -replace '\\', '\\' -replace '"', '\"') + '"'
        } else {
            $a
        }
    }) -join ' '
    try {
        $proc = Start-Process -FilePath $FilePath -ArgumentList $argLine `
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

function Invoke-Multipass {
    param(
        [Parameter(Mandatory = $true)][string[]]$ArgumentList,
        [switch]$ThrowOnError,
        [string]$FailMessage = ""
    )
    Invoke-Native -FilePath "multipass" -ArgumentList $ArgumentList `
        -ThrowOnError:$ThrowOnError -FailMessage $FailMessage
}

function Require-Multipass {
    if (-not (Get-Command multipass -ErrorAction SilentlyContinue)) {
        throw "Multipass not found. Install from https://multipass.run and re-run."
    }
}

Require-Multipass

Write-Host "Launching control-plane VM ($ControlPlane)..."
Invoke-Multipass -ArgumentList @(
    "launch", $UbuntuRelease, "--name", $ControlPlane, "--cpus", "2", "--memory", "3G", "--disk", "10G"
) -ThrowOnError -FailMessage "multipass launch $ControlPlane failed" | Out-Null

$workerNames = @()
for ($i = 1; $i -le $WorkerCount; $i++) {
    $name = "$WorkerPrefix$i"
    $workerNames += $name
    Write-Host "Launching worker VM ($name)..."
    # 4 CPU / 4G per worker: enough headroom that a stress-ng run on one worker
    # produces real pressure without starving the VM entirely.
    Invoke-Multipass -ArgumentList @(
        "launch", $UbuntuRelease, "--name", $name, "--cpus", "4", "--memory", "4G", "--disk", "15G"
    ) -ThrowOnError -FailMessage "multipass launch $name failed" | Out-Null
}

$allNodes = @($ControlPlane) + $workerNames

Write-Host "Pushing node prerequisites to all nodes..."
foreach ($node in $allNodes) {
    Invoke-Multipass -ArgumentList @(
        "transfer", $NodeCommonSh, "${node}:/tmp/k8s-node-common.sh"
    ) -ThrowOnError -FailMessage "multipass transfer to $node failed" | Out-Null
    # apt/gpg may chatter on stderr; exit code is the signal.
    Invoke-Multipass -ArgumentList @(
        "exec", $node, "--", "sudo", "bash", "/tmp/k8s-node-common.sh"
    ) -ThrowOnError -FailMessage "k8s-node-common.sh failed on $node" | Out-Null
}

Write-Host "Initializing control-plane..."
$infoResult = Invoke-Multipass -ArgumentList @("info", $ControlPlane, "--format", "json") `
    -ThrowOnError -FailMessage "multipass info $ControlPlane failed"
$cpInfo = $infoResult.StdOut | ConvertFrom-Json
$cpIp   = $cpInfo.info.$ControlPlane.ipv4[0]

# kubeadm init: version-check warning is stderr + exit 0 — must not use 2>&1 under Stop.
$initResult = Invoke-Multipass -ArgumentList @(
    "exec", $ControlPlane, "--",
    "sudo", "kubeadm", "init",
    "--apiserver-advertise-address=$cpIp",
    "--pod-network-cidr=$PodNetworkCidr"
) -ThrowOnError -FailMessage "kubeadm init failed"
$initOutput = ($initResult.StdOut + "`n" + $initResult.StdErr)
Write-Host $initOutput

$tokenMatch = [regex]::Match($initOutput, "--token\s+(\S+)")
$hashMatch  = [regex]::Match($initOutput, "--discovery-token-ca-cert-hash\s+(\S+)")

if (-not $tokenMatch.Success -or -not $hashMatch.Success) {
    throw "Could not parse join token/hash from kubeadm init output. Check the output above and join workers manually."
}

$joinCmd = "kubeadm join ${cpIp}:6443 --token $($tokenMatch.Groups[1].Value) --discovery-token-ca-cert-hash $($hashMatch.Groups[1].Value)"

Write-Host "Setting up kubectl access on the control-plane node..."
# Single-quoted: $HOME and $(id -u)/$(id -g) must expand on cp1's bash, not PowerShell.
# Drop cp -i (no TTY under multipass exec). Quote paths so mkdir always gets an operand.
$kubeconfigSetup = 'mkdir -p "$HOME/.kube" && sudo cp /etc/kubernetes/admin.conf "$HOME/.kube/config" && sudo chown "$(id -u):$(id -g)" "$HOME/.kube/config" && test -n "$HOME" && test -f "$HOME/.kube/config" && echo "kubeconfig_ok path=$HOME/.kube/config"'
$kubeSetupResult = Invoke-Multipass -ArgumentList @(
    "exec", $ControlPlane, "--", "bash", "-c", $kubeconfigSetup
) -ThrowOnError -FailMessage "kubectl kubeconfig setup on $ControlPlane failed"
Write-Host $kubeSetupResult.StdOut.TrimEnd()

Write-Host "Installing Calico CNI..."
# Single-quoted: no PowerShell expansion; spaces preserved via Invoke-Native quoting.
$calicoCmd = 'curl -fsSL https://raw.githubusercontent.com/projectcalico/calico/v3.28.0/manifests/calico.yaml | sed ''s#192.168.0.0/16#10.244.0.0/16#'' | kubectl apply -f -'
Invoke-Multipass -ArgumentList @(
    "exec", $ControlPlane, "--", "bash", "-c", $calicoCmd
) -ThrowOnError -FailMessage "Calico apply failed" | Out-Null

Write-Host "Joining worker nodes..."
foreach ($node in $workerNames) {
    Write-Host "Joining $node..."
    # $joinCmd is intentionally PowerShell-built (token/IP); pass as one bash -c argument.
    Invoke-Multipass -ArgumentList @(
        "exec", $node, "--", "sudo", "bash", "-c", $joinCmd
    ) -ThrowOnError -FailMessage "kubeadm join failed on $node" | Out-Null
}

Write-Host ""
Write-Host "Cluster provisioning complete. Verify with:"
Write-Host "  multipass exec $ControlPlane -- kubectl get nodes -o wide"
Write-Host ""
Write-Host "Allow ~30-60s after this finishes for Calico pods to schedule before nodes show Ready."
