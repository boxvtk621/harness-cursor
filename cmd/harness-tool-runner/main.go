package main

import (
	"os"

	"github.com/boxvtk621/harness-cursor/tools"
)

func main() {
	os.Exit(toolrunner.HelperMain(os.Args[1:], os.Stdin, os.Stdout))
}
