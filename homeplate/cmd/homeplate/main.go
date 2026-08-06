// Command homeplate turns your own machine into your GitHub Actions CI.
//
// GitHub keeps doing orchestration, PR checks, and logs. Your hardware does
// the compute: since March 2026 self-hosted runtime on private repos bills
// $0.002/min (public repos free), which is still up to ~31x cheaper than
// hosted macOS runners - and you own the silicon.
package main

import (
	"fmt"
	"os"

	"github.com/homeplate-ci/homeplate/internal/cli"
)

func main() {
	if err := cli.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "homeplate: "+err.Error())
		os.Exit(1)
	}
}
