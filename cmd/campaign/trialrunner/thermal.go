package main

import (
	"bytes"
	"fmt"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// countThermalThrottleEvents returns how many Microsoft-Windows-Kernel-Processor-Power
// event ID 37 (thermal throttle) entries fall in [start, end] on the Windows host.
// Non-Windows: 0, nil. Query failures return an error; callers must not fail the trial.
//
// Get-Counter "% of Maximum Frequency" is intentionally not used — flat/uninformative
// on this Hyper-V host. Event ID 37 is confirmed to fire only on real thermal throttles.
func countThermalThrottleEvents(start, end time.Time) (int, error) {
	if runtime.GOOS != "windows" {
		return 0, nil
	}
	if end.Before(start) {
		end = start
	}
	// Local DateTime via Unix ms avoids PowerShell culture/offset parsing issues.
	ps := fmt.Sprintf(`
$ErrorActionPreference = 'SilentlyContinue'
$s = [DateTimeOffset]::FromUnixTimeMilliseconds(%d).LocalDateTime
$e = [DateTimeOffset]::FromUnixTimeMilliseconds(%d).LocalDateTime
$ev = @(Get-WinEvent -FilterHashtable @{
  LogName = 'System'
  ProviderName = 'Microsoft-Windows-Kernel-Processor-Power'
  Id = 37
  StartTime = $s
  EndTime = $e
} -ErrorAction SilentlyContinue)
[Console]::Out.Write($ev.Count)
`, start.UnixMilli(), end.UnixMilli())

	cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", ps)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return 0, fmt.Errorf("Get-WinEvent thermal query: %s", msg)
	}
	out := strings.TrimSpace(stdout.String())
	if out == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(out)
	if err != nil {
		return 0, fmt.Errorf("parse thermal event count %q: %w", out, err)
	}
	if n < 0 {
		n = 0
	}
	return n, nil
}

func attachThermalThrottle(detail map[string]any, start, end time.Time) {
	n, err := countThermalThrottleEvents(start, end)
	detail["thermal_throttle_events_during_trial"] = n
	detail["thermal_throttle_detected"] = n > 0
	if err != nil {
		detail["thermal_throttle_query_error"] = err.Error()
	}
}
