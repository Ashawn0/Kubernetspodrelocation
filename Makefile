.PHONY: help schedprobe psiprobe imageprobe imageprobe-layer-share uidprobe uidprobe-decomp-repeat test

help:
	@echo "Stage 0 probes: make schedprobe | psiprobe | imageprobe | imageprobe-layer-share | uidprobe | uidprobe-decomp-repeat"
	@echo "Registry + ground-truth images: deploy/registry/bringup.ps1"

test:
	go test ./internal/...

schedprobe:
	go run ./cmd/stage0/schedprobe

psiprobe:
	go run ./cmd/stage0/psiprobe

imageprobe:
	go run ./cmd/stage0/imageprobe -suite plumbing

imageprobe-layer-share:
	go run ./cmd/stage0/imageprobe -suite layer-share

uidprobe:
	go run ./cmd/stage0/uidprobe -suite construct

uidprobe-decomp-repeat:
	go run ./cmd/stage0/uidprobe -suite decomp-repeat -repeats 8
