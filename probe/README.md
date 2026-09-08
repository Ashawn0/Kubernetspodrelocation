# In-cluster probe workload

- `uidserver/` — Go HTTP server that echoes Downward API `metadata.uid` as `X-Pod-Uid`.
- Stage 0 `uidprobe` defaults to an inline Python server on `python:3.12-alpine` (no custom image push). Build/push this Go binary for campaign runs if preferred (`-image`).
