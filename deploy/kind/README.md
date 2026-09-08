# kind (harness iteration only)

kind remains in the layout for fast Go harness iteration on a single shared kernel.

It does **not** close Stage 0 construct validity for CPU/memory/IO PSI or registry-network independence. Prefer `deploy/local-vm/` for those checks on this workstation; lab/university kubeadm is still required for IO PSI and registry isolation.

No cluster config is checked in yet. Do not treat a missing kind config as a reason to start `cmd/stage0` probes.
