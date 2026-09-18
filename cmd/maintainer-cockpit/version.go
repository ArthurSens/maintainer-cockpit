package main

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

var (
	version   = "dev"
	commit    = "unknown"
	buildDate = "unknown"
)

type buildInformation struct {
	Version string
	Commit  string
	Date    string
}

func newVersionCommand(stdout io.Writer, build buildInformation) *cobra.Command {
	var short bool
	command := &cobra.Command{
		Use:   "version",
		Short: "Print version and build information",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if short {
				_, err := fmt.Fprintln(stdout, build.Version)
				return err
			}
			_, err := fmt.Fprintf(
				stdout,
				"maintainer-cockpit %s\ncommit: %s\nbuilt: %s\n",
				build.Version,
				build.Commit,
				build.Date,
			)
			return err
		},
	}
	command.Flags().BoolVar(&short, "short", false, "print only the version")
	return command
}
