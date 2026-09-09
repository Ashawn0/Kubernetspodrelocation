package main

import (
	"bytes"
	"fmt"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
)

// sampleHostCPUPct returns Windows host total CPU utilization (% Processor Time)
// via Get-Counter. This is host-level load (Hyper-V parent + all VMs/processes),
// not guest PSI and not "steal time" directly — but it is the practical covariate
// for whether the physical machine was busy during a trial.
//
// Record-only: callers must not fail or gate trials on this value.
// Non-Windows returns (0, nil) and leaves fields unset by the attach helpers.
func sampleHostCPUPct() (float64, error) {
	if runtime.GOOS != "windows" {
		return 0, nil
	}
	// SampleInterval 1s is required for a meaningful CookedValue (instant 0-length
	// samples are undefined). This blocks ~1s per call by design.
	const ps = `
$ErrorActionPreference = 'Stop'
$c = Get-Counter -Counter '\Processor(_Total)\% Processor Time' -SampleInterval 1 -MaxSamples 1
[Console]::Out.Write(($c.CounterSamples | Select-Object -First 1).CookedValue.ToString([cultureinfo]::InvariantCulture))
`
	cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", ps)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return 0, fmt.Errorf("Get-Counter host CPU: %s", msg)
	}
	out := strings.TrimSpace(stdout.String())
	if out == "" {
		return 0, fmt.Errorf("Get-Counter host CPU: empty output")
	}
	v, err := strconv.ParseFloat(out, 64)
	if err != nil {
		return 0, fmt.Errorf("parse host CPU %% %q: %w", out, err)
	}
	return v, nil
}

func attachHostCPUStart(detail map[string]any) {
	if runtime.GOOS != "windows" {
		return
	}
	pct, err := sampleHostCPUPct()
	if err != nil {
		detail["host_cpu_query_error_start"] = err.Error()
		return
	}
	detail["host_cpu_pct_start"] = pct
}

func attachHostCPUEnd(detail map[string]any) {
	if runtime.GOOS != "windows" {
		return
	}
	pct, err := sampleHostCPUPct()
	if err != nil {
		detail["host_cpu_query_error_end"] = err.Error()
		return
	}
	detail["host_cpu_pct_end"] = pct
}
