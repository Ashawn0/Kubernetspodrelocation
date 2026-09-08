# Bring up registry + build ground-truth layer images.
# Requires: kubectl pointed at the local-VM cluster.
$ErrorActionPreference = "Stop"

$Here = $PSScriptRoot

Write-Host "Applying registry..."
kubectl apply -f (Join-Path $Here "registry.yaml")
kubectl -n reloc-registry rollout status deploy/registry --timeout=180s

Write-Host "Deleting prior build job (if any)..."
kubectl -n reloc-registry delete job build-layer-images --ignore-not-found

Write-Host "Starting build-layer-images Job..."
kubectl apply -f (Join-Path $Here "build-layer-images-job.yaml")
kubectl -n reloc-registry wait --for=condition=complete job/build-layer-images --timeout=600s

Write-Host "Build logs:"
kubectl -n reloc-registry logs job/build-layer-images

Write-Host "Ground truth ConfigMap:"
kubectl -n reloc-registry get configmap layer-ground-truth -o jsonpath="{.data.layer-ground-truth\.json}"
Write-Host ""
Write-Host ""
Write-Host "Next: go run ./cmd/stage0/imageprobe -suite layer-share"
Write-Host "Optional: configure insecure registry on nodes for kubelet pulls (ctr --plain-http used by probe does not require it)."
