// Command breakwater-agent is the Windows agent service (stub for M1 cross-compile).
// Full service implementation begins in M2.
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
	fmt.Fprintln(os.Stderr, "breakwater-agent: Windows service implementation begins in M2 (see PLAN.md)")
	fmt.Fprintln(os.Stderr, "This binary is a cross-compile stub for M1 CI.")
	os.Exit(0)
}
