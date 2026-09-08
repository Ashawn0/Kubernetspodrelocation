# Implementation log

Messier trail than the Stage 0 pass/fail artifacts under `experiments/results/stage0/`. Those stay clean and authoritative. This file records what broke, what we tried, what it meant, and what we changed — written so it can feed Discussion, limitations, or an implementation-notes appendix later, not as private scratch.

**Convention:** numbered entries, each with what happened, root cause (once known), fix, and lesson/implication when worth keeping. Append whenever Stage 0 (or later campaign work) breaks, gets fixed, or produces a result whose reasoning matters more than the clean outcome. Each entry should stand alone for someone drafting paper text from it.

---

### 1. CRLF line endings silently broke `k8s-node-common.sh` on first run

**What happened.** On first Multipass bring-up, `k8s-node-common.sh` failed immediately on the Ubuntu VMs. Bash died at line 5, right after `set -euo pipefail`, before containerd or kubeadm installed. No useful package-install error — the script never got that far.

**Root cause.** The file on disk had Windows CRLF line endings. `multipass transfer` copied it byte-for-byte into the guest. Bash then treated the `\r` as part of the line, so `set -euo pipefail\r` (and subsequent lines) failed. This is Windows/git line-ending handling, not a logic bug in the script.

**Fix.** Strip CRLF before transfer so the guest sees LF-only shell. Repo-side prevention: `.gitattributes` now forces `*.sh text eol=lf` so a fresh checkout does not reintroduce CRLF on the next machine.

**Lesson.** Any script that must run under Linux bash but is authored or checked out on Windows needs an explicit LF policy. Silent early death at `set -euo pipefail` is a strong signal for `\r`, not for missing packages.

---

### 2. Relative path failed under `[System.IO.File]::ReadAllText`

**What happened.** PowerShell reported “file not found” for a script that was plainly present in the directory the prompt appeared to be using.

**Root cause.** .NET’s process working directory did not match the location shown by the PowerShell prompt — likely an artifact of a conda-activated shell, where prompt `cwd` and the CLR’s idea of cwd can diverge.

**Fix.** Resolve an absolute path with `Join-Path (Get-Location) <file>` (or equivalent) before any .NET file I/O such as `[System.IO.File]::ReadAllText`.

**Lesson.** Do not trust relative paths in mixed PowerShell / .NET scripting. Resolve absolute first; the failure mode looks like a missing file when the file is fine.

---

### 3. Backgrounding `stress-ng` inside a single `multipass exec` produced a silent null result

**What happened.** A remote command of the form `nohup stress-ng ... &` inside one `multipass exec` appeared to succeed, but CPU PSI on the stressed worker stayed at exactly `0.00` before, during, and after the supposed load — no signal at all.

**Root cause (most likely).** The `multipass exec` session tore down as soon as the parent remote command returned, killing the backgrounded child despite `nohup`. The stressor never actually ran for the sampling window.

**Fix.** Run the stressor in the foreground on the remote side, and wrap that blocking `multipass exec` in a separate PowerShell `Start-Job` (or equivalent) so the host keeps a live connection for the full stress duration instead of detaching remotely.

**Lesson.** “Backgrounded inside Multipass” is not the same as “running for the experiment.” For PSI isolation, the load process must outlive the sampling window under an explicitly held session.

---

### 4. First CPU PSI attempt (`--cpu 4` on a 4-vCPU node) was flat for a different reason

**What happened.** After the backgrounding issue was fixed, `stress-ng --cpu 4` on a 4-vCPU worker still produced essentially no CPU PSI “some” movement — another flat result, but not because the process was dead.

**Root cause.** PSI’s `some` metric measures time stalled waiting for a resource, not raw utilization. Four CPU workers on four cores can each receive a core with nothing to wait for: 100% busy, ~0% stalled. Matching demand to capacity 1:1 does not exercise the quantity Stage 0 is validating.

**Fix.** Oversubscribe 2:1 (`--cpu 8` on 4 vCPUs). That produced a clean textbook curve: `avg10` rose then fell with the load; `avg60` and `avg300` lagged by their respective time constants; idle `worker2` never moved. Recorded as a confirmed pass in `experiments/results/stage0/cpu-psi-isolation-localvm.md`.

**Lesson (paper-relevant).** Any PSI-based covariate needs genuine oversubscription in its validation protocol. Matching demand to capacity 1:1 proves nothing about stall-based features; utilization and pressure are not interchangeable.

---

### 5. Memory PSI needed the same lesson applied differently — and even then stayed shallow

**What happened.** An initial percentage-based allocation (`--vm-bytes 75%`) produced little memory stall: demand stayed under the kernel’s already-reported “available” figure (which includes reclaimable cache). The kernel evicted clean page cache; stall barely registered. Explicit sizing past “available” (not “total”) then produced a real, isolated signal: `worker1` responded; idle `worker2` stayed at exactly 0 across two runs at increasing size. Both runs showed the same shallow shape — nearly all stall in the first ~8 seconds, then flat — unlike CPU’s sustained curve. Escalating from 1700M×2 to 1800M×2 only moved peak `some` total from 12,030 µs to 16,057 µs. Testing stopped at two clean confirmations rather than a third push toward OOM for diminishing information. Pass artifact: `experiments/results/stage0/memory-psi-isolation-localvm.md`.

**Root cause.** Memory “pressure” under this protocol is dominated by fast cache reclaim once you cross available, not by sustained contention analogous to CPU oversubscription. Isolation holds; magnitude and time-shape do not match the CPU result.

**Fix (for validation).** Size past **available**, not total or a naive percentage of total; keep an idle control node; treat two reproducible isolation passes as enough without chasing OOM.

**Lesson (paper-relevant).** A memory-pressure covariate may carry a **qualitatively different signal shape** than a CPU-pressure covariate. If both land in the final feature set, flag that difference — do not assume they behave the same way merely because both are PSI-derived.

---

### 6. Recurring false alarm: stderr rendered as `NativeCommandError`

**What happened.** Across the session, both `kubeadm`’s remote-version-check line and `stress-ng`’s own info logging appeared in PowerShell as red `NativeCommandError` blocks. Each time looked like a hard failure until checked carefully.

**Root cause.** Those tools write informational messages to stderr. PowerShell’s default host rendering paints native stderr as errors even when the process exit code is success. Neither case was an actual failure.

**Fix.** None required in the tools. Treat red stderr from known-good `kubeadm` / `stress-ng` invocations as a rendering quirk unless the exit code is non-zero or the cluster/load actually misbehaves.

**Lesson.** Do not re-diagnose this from scratch next time it shows up. Confirm exit status and real effects (nodes Ready, PSI moved, etc.) before chasing a “NativeCommandError” that is only stderr.

---

### 7. Only `uidprobe` depends on the open TTFS client-behavior decision

**What happened.** Early Stage 0 hold language blocked all four probes until T0/T1 client behavior was settled. That was broader than the dependency graph required.

**Root cause.** TTFS client behavior (`Connection: close` vs keep-alive) defines what `uidprobe` measures. `schedprobe`, `psiprobe`, and `imageprobe` never touch that clock.

**Fix.** Implemented `cmd/stage0/{schedprobe,psiprobe,imageprobe}`; continue holding only `uidprobe`.

**Lesson.** Gate probes on their actual dependencies, not on “Stage 0 unfinished” as a blanket.

---

### 8. Layer-sharing claim needs a real registry pull path

**What happened.** `imageprobe` plumbing against `pause:3.9` passed warm/CRI accounting and `uncached≠total` on a present image, which looked like a contrast check but did not exercise shared layers.

**Root cause.** Uncached bytes are defined as what containerd would still **fetch**. Layer-sharing divergence (after `app-a` is present, `uncached(app-b)≈size(L2)≠total(app-b)`) requires a registry pull path and purpose-built L0/L1/L2 images.

**Fix.** In-cluster `registry:2` (`deploy/registry/`), buildah Job producing deterministic `base`/`app-a`/`app-b`, ConfigMap ground-truth compressed blob sizes, and `imageprobe -suite layer-share`.

**Lesson.** Do not close measurement-spec claims on plumbing proxies; the paper justification needs the ground-truth sequence.

---

### 9. HostExec used `sh` (dash); layer-share scripts need bash `pipefail`

**What happened.** `imageprobe -suite layer-share` failed almost every check with `sh: 2: set: Illegal option -o pipefail`. Clear/remove checks that did not use `pipefail` still passed.

**Root cause.** `k8s.HostExec` invoked the host command via `nsenter … -- sh -c`. On Ubuntu, `/bin/sh` is dash, which rejects `set -o pipefail`. Layer-share and several other host scripts use bashisms (`set -euo pipefail`); PSI collection happened to avoid that line.

**Fix.** HostExec now uses `bash -c`, matching `k8s-node-common.sh`, the registry build Job, and memory host-stress. One convention: host-side scripts are bash.

**Lesson.** Ubuntu `/bin/sh` is dash; never assume `pipefail` works under `sh -c` on kubeadm nodes.

---

### 10. `ctr content info` does not exist — false "uncached" for every digest

**What happened.** After bash fix, layer-share got through pull/warm of `app-a`, but `app_b_shared_L0` reported `uncached=L0+L2` exactly. Diagnostics showed `cached=false` on L0 and L2 with ctr message "No help topic for 'info'".

**Root cause.** The probe called `ctr content info <digest>`, which is not a real subcommand (`ls`, `get`, `fetch`, … only). Every presence check failed before touching the store; misses were shell errors, not real absences.

**Fix.** Presence checks now use `ctr -n k8s.io content ls -q` (one digest per line) and exact set membership on the full `sha256:…` token — no column parsing, no substring grep.

**Lesson.** Probe the real CLI surface (`ctr content --help`) before assuming docker-like subcommands exist.

---

### 11. Layer-share suite closed on local-VM (four bugs on the way)

**What happened.** `imageprobe -suite layer-share` now passes all nine checks. Artifact: `experiments/results/stage0/layer-sharing-divergence-localvm.md`. This closes uncached-bytes construct validity on local-VM for the paper’s layer-sharing justification, distinct from the earlier `pause:3.9` plumbing pass.

**Trail (bugs hit before the clean result):**

1. **buildah without `--layers`** — multi-`COPY` images flattened to one layer; `app-a`/`app-b` failed the two-layer assert. Fixed with `--layers` (+ `--timestamp=0` for stable L0 digests).
2. **Weak LCG PRNG** — low-bit periodicity compressed multi-MiB payloads down to ~13KiB blobs, undermining “known size” ground truth. Fixed with deterministic SHA-256(counter) streams.
3. **HostExec via `sh`/dash** — `set -o pipefail` illegal; suite died before real accounting. Fixed by `bash -c` in HostExec (entry 9).
4. **Nonexistent `ctr content info`** — every digest looked “missing”; `uncached(app-b)` falsely equaled L0+L2. Fixed with `content ls -q` exact membership (entry 10).

**Local-VM Stage 0 status after this:** scheduler path, CPU/memory PSI, uncached bytes (including layer-sharing), and UID-pinned TTFS are confirmed. Still open on this machine: IO PSI and registry-vs-pod-network independence (shared disk/NIC).

---

### 12. TTFS client behavior frozen; uidprobe confirmed

**Decision.** Headline: HTTP/1.1 `Connection: close` (no keep-alive). Keep-alive is a sensitivity cell only — conntrack can pin traffic to a terminating pod independent of EndpointSlice / target-node state. Serving paths: report **both** ClusterIP TTFS and direct pod-IP TTFS so Discussion can separate readiness from service-path lag.

**Fix.** Spec updated; `cmd/stage0/uidprobe` + `internal/disrupt` + `probe/uidserver` implemented (Stage 0 default image: inline Python on `python:3.12-alpine` with Downward API `POD_UID`). First run lost ClusterIP samples because cancelling the load context killed HostExec early; load now runs to completion on a detached context. Artifact: `experiments/results/stage0/uid-pinned-ttfs-localvm.md`.

**Follow-up (decomposition magnitudes).** Construct-run ClusterIP 2134 ms < pod-IP 2446 ms looked mechanistically backwards. Eight fair dual-poller repeats (`-suite decomp-repeat`): 5 near-ties (|Δ|≤5 ms), 3 with ClusterIP slower by ~465–475 ms, **0** with pod-IP meaningfully slower. Verdict: noise; single-trial pair is illustrative only. Pinning/overlap numbers untouched.
