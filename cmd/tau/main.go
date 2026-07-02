// Command tau is a fast per-directory shell environment tool.
package main

import (
	"os"

	"github.com/edganiukov/taugres/internal/cli"
)

func main() {
	os.Exit(cli.Main(cli.DefaultEnv()))
}
