// Command meta-ads is a command line interface to the Meta Marketing API.
package main

import (
	"os"

	"github.com/KudcraftsHQ/meta-ads-cli/internal/dispatch"
)

func main() {
	os.Exit(dispatch.Execute())
}
