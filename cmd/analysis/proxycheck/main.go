// Command proxycheck builds a PROXY INTEGRATION CHECK oracle-baseline export
// from a finished pilot CSV with an artificial cal/eval split.
//
// This is NOT a campaign result. Filenames and JSON fields are labeled proxy so
// the output can never be confused with real GapCaptured. See
// docs/campaign-design.md §10.
//
// Example:
//
//	go run ./cmd/analysis/proxycheck \
//	  -csv analysis/testdata/pilot-variance-18trial-smoke.csv \
//	  -out-dir experiments/results/integration-check
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"reloc-disrupt/internal/evalexport"
)

const proxyNote = "PROXY INTEGRATION CHECK: real pilot trial costs with an artificial " +
	"cal/eval split (replicates 1-2 calibration, 3 evaluation per cell). " +
	"Not a methodological campaign result; GapCaptured from this path is plumbing " +
	"evidence only, not model performance."

func main() {
	csvPath := flag.String("csv", "analysis/testdata/pilot-variance-18trial-smoke.csv",
		"finished pilot CSV (NOT the live 90-trial file under experiments/results/pilot/)")
	outDir := flag.String("out-dir", "experiments/results/integration-check", "output directory")
	flag.Parse()

	trials, err := evalexport.LoadTrialsCSV(*csvPath)
	must(err)

	// Artificial split purely to exercise the pipeline (not a real design choice).
	exp, err := evalexport.Build(trials, []int{1, 2}, []int{3})
	must(err)
	exp.ProxyIntegrationCheck = true
	exp.ProxyNote = proxyNote
	absCSV, _ := filepath.Abs(*csvPath)
	exp.SourceCSV = absCSV

	day := time.Now().UTC().Format("20060102")
	outPath := filepath.Join(*outDir, fmt.Sprintf("proxy-oracle-baseline-export-%s.json", day))
	must(evalexport.WriteFile(outPath, exp))
	fmt.Printf("PROXY INTEGRATION CHECK export written (NOT a campaign result):\n  %s\n", outPath)
	fmt.Printf("oracle_selected_cell=%s corrected=%.6f cells=%d\n",
		exp.OracleSelectedCell, exp.OracleCorrectedCost, len(exp.Cells))
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
