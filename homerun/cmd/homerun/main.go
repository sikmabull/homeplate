// Command homerun turns your own machine into your GitHub Actions CI.
//
// GitHub keeps doing orchestration, PR checks, and logs (all free).
// Your hardware does the compute (also free: self-hosted runners have no
// per-minute billing).
package main

import (
	"fmt"
	"os"

	"github.com/homerun-ci/homerun/internal/cli"
)

func main() {
	if err := cli.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "homerun: "+err.Error())
		os.Exit(1)
	}
}
