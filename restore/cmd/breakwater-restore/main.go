// Command breakwater-restore is the restore TUI (stub for M1 module layout).
package main

import (
	"fmt"
	"os"
)

var version = "0.0.1-dev"

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "-version" || os.Args[1] == "--version") {
		fmt.Println(version)
		return
	}
	fmt.Fprintln(os.Stderr, "breakwater-restore: not yet implemented (see PLAN.md)")
	os.Exit(1)
}
