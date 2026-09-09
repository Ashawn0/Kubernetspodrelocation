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

---

### 13. AWS deploy path for IO PSI and registry ENI independence

**What happened.** Multipass cannot close IO PSI (one host disk) or registry-vs-pod-network independence (one NIC). Added `deploy/aws/` mirroring `deploy/local-vm/`: Terraform VPC with primary + registry subnets, `m6i.large` CP + `c6id.xlarge` workers, secondary ENI per worker, NVMe mounted at `/mnt/reloc-nvme`, one-command `up.ps1` / `down.ps1`.

**Deviations from `k8s-node-common.sh`.** Kept that script for kubeadm/containerd; AWS-only work is `aws-node-prep.sh` (NVMe + secondary ENI netplan, `rp_filter=2`, source/dest check off in Terraform). Ubuntu 24.04 Noble AMI (not Multipass 22.04).

**Probe hooks.** `psiprobe -skip-io-isolation=false -io-path /mnt/reloc-nvme`; new `netprobe` shapes `ens6` only and asserts `ens5` stable. Budget: treat $85 alert as stop → `.\down.ps1`.

**Key handling.** `up.ps1` never copies `reloc-disrupt-key.pem` onto the CP. Join token is captured from CP stdout (`JOIN_BEGIN`/`JOIN_END`) and `kubeadm join` runs on each worker via the operator’s existing SSH — same path as `k8s-node-common.sh`.

---

### 14. AWS Stage 0 closeout session (account → probes → gates closed)

Single session trail from first AWS bring-up through closing the two AWS-only Stage 0 gates. Earlier piecemeal notes on logical-vs-physical ENI isolation and memory-PSI zero-signal are folded here rather than kept as separate §14/§15.

#### 1. Free Plan blocked non-free-tier instance types

**What happened.** New AWS account (post July 2025 Free Plan) refused the Terraform instance types needed for Stage 0 (`c6id.xlarge` / `m6i.large`) despite unused vCPU quota headroom.

**Root cause.** Free Plan accounts can block non-free-tier EC2 types outright; that is not a Service Quotas ceiling.

**Fix.** Upgrade Free Plan → Paid Plan in the Billing console. A Service Quotas request was the wrong lever (tried first, then corrected).

#### 2. PowerShell `$ErrorActionPreference = "Stop"` + native ssh/scp stderr

**What happened.** `up.ps1` aborted on ssh/scp even when the remote command succeeded (exit 0).

**Root cause.** Under `Stop`, PowerShell promotes native-command stderr text to a script-terminating error. Redirecting with `2>$null` is documented as unreliable in that mode.

**Fix.** Route all ssh/scp through `Start-Process` with redirected output files and explicit exit-code checks (`Invoke-Native`), not automatic stderr promotion.

#### 3. Transient SSH timeouts on bring-up

**What happened.** Two separate runs hit brief SSH timeouts on different nodes; manual recheck showed the nodes were fine.

**Fix.** `Invoke-Remote` / `Send-File` retry 3× with 5/10/15 s backoff.

#### 4. `gpg --dearmor` hung on re-provision

**What happened.** Re-running `k8s-node-common.sh` on an already-provisioned node stopped on an interactive overwrite prompt with no tty.

**Fix.** `gpg --batch --yes --dearmor`. Same change on the local-VM copy (latent there too).

#### 5. `config/paths.env` CRLF

**What happened.** `.sh` scripts were LF-normalized before transfer; `paths.env` was not, so `source` failed on Linux.

**Fix.** Include `paths.env` in the normalization loop; `.gitattributes` forces LF for `*.sh` and `paths.env`.

#### 6. Memory PSI: readiness bug, then a real zero-signal characteristic

**Two distinct issues.**

**(a) False “no usable signal”.** `WaitPodRunning` returned once the container was Running, before host `stress-ng` was necessarily allocating; the 5 s sample clock started immediately and could miss the whole stall window. Fixed with `WaitHostStressNG` (`pgrep` until stress-ng is live) before starting the sample clock, plus stress-pod log capture on the JSONL record.

**(b) Genuine zero PSI under real stress.** After (a), manual out-of-band `/proc/pressure/memory` reads still showed **unchanged total stall across ~20 s** while stress-ng held ~7.2–7.4 GB resident on a ~7.6 GiB `c6id.xlarge`. Likely cause: ~2.9 GiB reclaimable page cache let the kernel satisfy demand via fast eviction with no task stall. Environment healthy: clean `dmesg`, no OOM kills, no swap (`swapon` empty). Probe now records `swapon` + `dmesg_oom` on every memory-isolation row. This is the strong form of the local-VM shallow-memory-PSI finding (entry 5): signal strength depends on page-cache headroom relative to demand, not only MemAvailable arithmetic. Paper Discussion / feature-reliability material — not a measurement bug. No Stage 0 “fix” for the kernel behavior.

#### 7. netprobe iperf3 killed by HostExec cgroup teardown

**What happened.** After the `ss -ltn` readiness fix, failures still showed raw client `connection refused` (not “server not listening”). On the server node, `/tmp/iperf-s.log` existed but was **0 bytes** — bind had happened, then the process died before flushing a banner.

**Root cause.** HostExec nsenter enters pid/mount/net (not cgroup). `nohup iperf3 -s -1 … &` then return left the server in the wrapper pod’s cgroup; deleting that pod after readiness killed iperf3 before the client HostExec ran. Same failure class as multipass exec killing backgrounded stress (entry 3), different transport. `-1` was secondary; detachment semantics were the bug.

**Fix.** Match the memory-stress pattern: long-lived privileged pod, host `iperf3 -s` in the **foreground** for the test duration (no background-and-return). Listen-poll while that pod stays Running; delete after the client finishes.

#### Registry path claim (logical isolation)

`c6id.xlarge` has **Maximum Network Cards: 1** — primary and secondary ENIs share one physical NIC. Shaping is downward-only (`tbf` 20 Mbit + `netem` 100 ms on the registry iface). Claim: logically isolated via per-iface shaping / `rp_filter`, with raw deltas on the record — not unqualified physical independence. Boolean thresholds remain operational sanity alongside signed `shaping_raw_deltas`.

#### Final result — Stage 0 closed on AWS

Both AWS-only Stage 0 gates closed with measured magnitudes, not booleans alone:

- **`isolation_io`:** pass — `stressed_delta_us` 13 045 678 vs control flat (NVMe path).
- **`netprobe`:** pass — primary iface ≈ +0.00 ms RTT / +0.1% BW (noise; isolated); registry iface +200.04 ms RTT / −100% BW (matches 100 ms netem + 20 Mbit tbf by design).

**Stage 0 is closed.** Next: experimental campaign (LightGBM training, calibration/evaluation replicate split, GapCaptured) — not started.

---

### 16. `.gitattributes` LF rules do not rewrite already-committed CRLF

**What happened.** `*.sh text eol=lf` (and `paths.env`) was already in `.gitattributes`, but `deploy/registry/configure-insecure-registry.sh` still produced the classic bash CRLF diagnostic (dollar-single-quote backslash-r / `command not found` on `\r`) under `multipass exec ... bash` on Ubuntu. Working-tree bytes had CRLF; with `core.autocrlf=true`, `git status` stayed clean because the clean filter hides CR on compare.

**Root cause.** Adding an `eol=lf` attribute does **not** retroactively rewrite blobs or refresh working trees that already carried CRLF (or that an editor re-saved as CRLF after checkout). Attributes apply to new checkins/checkouts; a one-time renormalization (and/or an explicit LF rewrite of the working tree) is required for pre-existing files.

**Fix.** Ran `git add --renormalize .` against the existing rules. Index/HEAD blobs for tracked `*.sh` / `paths.env` were already LF (nothing new to commit for those objects). Rewrote the dirty working-tree copy of `configure-insecure-registry.sh` (and `.gitattributes`) to LF and re-audited every tracked `.sh` for CR bytes — none remain.

**Lesson.** Whenever an `eol=` rule is added, or a shell script is suspected of stale line endings: (1) `git add --renormalize .`, (2) byte-audit `*.sh` for CR (`\r`), (3) commit any resulting index changes as their own commit. Do not assume `.gitattributes` alone healed files that were wrong before the rule landed.
