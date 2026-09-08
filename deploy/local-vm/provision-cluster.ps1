# provision-cluster.ps1
# Stands up a local kubeadm cluster (1 control-plane + N workers) via Multipass,
# for Stage 0 PSI-isolation and registry-shaping-rehearsal testing on this machine,
# ahead of lab/university kubeadm access.
#
# Requires: Multipass installed (https://multipass.run), Hyper-V enabled.
# Run k8s-node-common.sh and this script from the same directory.

$ErrorActionPreference = "Stop"

$WorkerCount    = 2
$UbuntuRelease  = "22.04"
$ControlPlane   = "cp1"
$WorkerPrefix   = "worker"
$PodNetworkCidr = "10.244.0.0/16"   # matches the Calico manifest below

function Require-Multipass {
    if (-not (Get-Command multipass -ErrorAction SilentlyContinue)) {
        throw "Multipass not found. Install from https://multipass.run and re-run."
    }
}

Require-Multipass

Write-Host "Launching control-plane VM ($ControlPlane)..."
multipass launch $UbuntuRelease --name $ControlPlane --cpus 2 --memory 3G --disk 10G

$workerNames = @()
for ($i = 1; $i -le $WorkerCount; $i++) {
    $name = "$WorkerPrefix$i"
    $workerNames += $name
    Write-Host "Launching worker VM ($name)..."
    # 4 CPU / 4G per worker: enough headroom that a stress-ng run on one worker
    # produces real pressure without starving the VM entirely.
    multipass launch $UbuntuRelease --name $name --cpus 4 --memory 4G --disk 15G
}

$allNodes = @($ControlPlane) + $workerNames

Write-Host "Pushing node prerequisites to all nodes..."
foreach ($node in $allNodes) {
    multipass transfer k8s-node-common.sh "${node}:/tmp/k8s-node-common.sh"
    multipass exec $node -- sudo bash /tmp/k8s-node-common.sh
}

Write-Host "Initializing control-plane..."
$cpInfo = multipass info $ControlPlane --format json | ConvertFrom-Json
$cpIp   = $cpInfo.info.$ControlPlane.ipv4[0]

$initOutput = multipass exec $ControlPlane -- sudo kubeadm init `
    --apiserver-advertise-address=$cpIp `
    --pod-network-cidr=$PodNetworkCidr 2>&1 | Out-String

Write-Host $initOutput

$tokenMatch = [regex]::Match($initOutput, "--token\s+(\S+)")
$hashMatch  = [regex]::Match($initOutput, "--discovery-token-ca-cert-hash\s+(\S+)")

if (-not $tokenMatch.Success -or -not $hashMatch.Success) {
    throw "Could not parse join token/hash from kubeadm init output. Check the output above and join workers manually."
}

$joinCmd = "kubeadm join ${cpIp}:6443 --token $($tokenMatch.Groups[1].Value) --discovery-token-ca-cert-hash $($hashMatch.Groups[1].Value)"

Write-Host "Setting up kubectl access on the control-plane node..."
multipass exec $ControlPlane -- bash -c "mkdir -p `$HOME/.kube && sudo cp -i /etc/kubernetes/admin.conf `$HOME/.kube/config && sudo chown `$(id -u):`$(id -g) `$HOME/.kube/config"

Write-Host "Installing Calico CNI..."
multipass exec $ControlPlane -- bash -c "curl -fsSL https://raw.githubusercontent.com/projectcalico/calico/v3.28.0/manifests/calico.yaml | sed 's#192.168.0.0/16#10.244.0.0/16#' | kubectl apply -f -"

Write-Host "Joining worker nodes..."
foreach ($node in $workerNames) {
    Write-Host "Joining $node..."
    multipass exec $node -- sudo bash -c "$joinCmd"
}

Write-Host ""
Write-Host "Cluster provisioning complete. Verify with:"
Write-Host "  multipass exec $ControlPlane -- kubectl get nodes -o wide"
Write-Host ""
Write-Host "Allow ~30-60s after this finishes for Calico pods to schedule before nodes show Ready."
