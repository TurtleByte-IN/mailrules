// Command mailrules is the MailRules daemon and CLI: the free self-host build, with no
// modules (ext/daemon runs it).
package main

import (
	"fmt"
	"os"

	"github.com/TurtleByte-IN/mailrules/ext/daemon"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := daemon.Run(os.Args[1:], version); err != nil {
		fmt.Fprintln(os.Stderr, "mailrules:", err)
		os.Exit(1)
	}
}
