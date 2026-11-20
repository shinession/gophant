// Command gophant migrates a PostgreSQL 16 database to PostgreSQL 18 with a snapshot copy and
// logical WAL replay. See docs/gophant.md for the design.
package main

import (
	"os"

	"github.com/shinession/gophant/internal/cli"
)

// version is set at build time: go build -ldflags "-X main.version=1.2.3".
var version = "dev"

func main() {
	os.Exit(cli.Execute(version))
}
