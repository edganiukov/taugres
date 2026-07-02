// Command tau manages reproducible, deterministic development environments.
package main

import (
	"os"

	"github.com/edganiukov/taugres/internal/cli"
)

func main() {
	os.Exit(cli.Main(cli.DefaultEnv()))
}
