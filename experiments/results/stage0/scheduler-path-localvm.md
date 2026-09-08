# Stage 0 artifact: scheduler binding path (local-VM)

**Result:** PASS (construct-valid on Multipass kubeadm for claim 2.2).

**Environment:** local-VM path (`deploy/local-vm/`), Multipass kubeadm (1 control-plane + workers), separate guest kernels.

**Setup:** `cmd/stage0/schedprobe` against that cluster. Forced placement via unique per-worker labels (`reloc-disrupt.stage0/node=<hostname>`). Positive cases use `nodeSelector` / required `nodeAffinity` only (never `spec.nodeName` for the condition under test). Negative control sets `spec.nodeName` deliberately. Impossible selector uses a label value no node has. Evidence for the scheduler binding path is a `Scheduled` event from `default-scheduler` plus landing on the intended node (Binding subresource POST is what DefaultBinder performs; without API audit, that event is the binding-path signal).

| Check | Result |
|---|---|
| nodeSelector_scheduled_and_bound | PASS |
| nodeAffinity_scheduled_and_bound | PASS |
| nodeName_negative_no_scheduler_event | PASS |
| impossible_selector_pending | PASS |

```
PASS nodeSelector_scheduled_and_bound
PASS nodeAffinity_scheduled_and_bound
PASS nodeName_negative_no_scheduler_event
PASS impossible_selector_pending
```

## Interpretation

- **nodeSelector / nodeAffinity PASS:** forced placement goes through the real kube-scheduler Filter → Score → Bind path. A `Scheduled` event from `default-scheduler` appears and the pod lands on the labeled target node. This is the campaign placement mechanism, not a kubelet shortcut.
- **nodeName negative PASS:** the same workload with `spec.nodeName` set reaches the intended node **without** a scheduler `Scheduled` event — confirming the bypass the research design forbids for real trials, and that the probe can tell the two paths apart.
- **impossible selector PASS:** a selector no node satisfies stays `Pending` with empty `nodeName`, showing Filter actually runs and rejects rather than silently binding somewhere.

Claim 2.2 is closed on the local-VM environment. Lab/university kubeadm remains smoke-only for this instrument.
