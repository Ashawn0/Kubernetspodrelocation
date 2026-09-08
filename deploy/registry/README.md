# In-cluster registry (Stage 0 / reusable)

Minimal `registry:2` Deployment + NodePort Service for:

- Section 2.5 layer-sharing ground-truth pulls (`imageprobe -suite layer-share`)
- Later section 2.6 registry-shaping rehearsal (independence still not closeable on this host)

## Apply

```powershell
kubectl apply -f deploy/registry/registry.yaml
kubectl -n reloc-registry rollout status deploy/registry --timeout=120s
```

Service DNS (in-cluster): `registry.reloc-registry.svc.cluster.local:5000`  
NodePort from a worker IP: `<worker-ip>:30500`

HTTP only. Configure containerd on every node before kubelet/ctr pulls (see `configure-insecure-registry.sh`).

## Build and push ground-truth images

```powershell
# After registry is Ready and insecure config is applied:
kubectl apply -f deploy/registry/build-layer-images-job.yaml
kubectl -n reloc-registry wait --for=condition=complete job/build-layer-images --timeout=600s
kubectl -n reloc-registry logs job/build-layer-images
```

That Job writes layer blob sizes into a ConfigMap consumed by `imageprobe`.

Build notes: `buildah bud` uses `--layers` (required — without it multi-COPY images collapse to one layer) and `--timestamp=0` (stable digests). Payloads are deterministic SHA-256(counter) streams so compressed sizes stay large and stable.
