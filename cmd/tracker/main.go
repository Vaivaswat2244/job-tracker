// Command tracker is the job application pipeline CLI.
package main

import (
	"os"

	"github.com/Vaivaswat2244/job-tracker/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:]))
}
