// Command config-schema writes the current Maintainer Cockpit configuration
// schema for release automation.
package main

import (
	"fmt"
	"os"

	"github.com/ArthurSens/maintainer-cockpit/internal/config"
)

func main() {
	body, err := config.JSONSchema()
	if err == nil {
		_, err = os.Stdout.Write(body)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
