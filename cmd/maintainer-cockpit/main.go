// Command maintainer-cockpit runs and validates Maintainer Cockpit.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	command := newRootCommand(stdout, stderr)
	command.SetArgs(args)
	return command.Execute()
}

func newRootCommand(stdout, stderr io.Writer) *cobra.Command {
	return newRootCommandWithBuildInformation(stdout, stderr, buildInformation{
		Version: version,
		Commit:  commit,
		Date:    buildDate,
	})
}

func newRootCommandWithBuildInformation(
	stdout, stderr io.Writer,
	build buildInformation,
) *cobra.Command {
	command := &cobra.Command{
		Use:           "maintainer-cockpit",
		Short:         "Prioritize open-source pull request reviews",
		SilenceErrors: true,
		SilenceUsage:  true,
	}
	command.SetOut(stdout)
	command.SetErr(stderr)
	command.AddCommand(
		newConfigCheckCommand(stdout),
		newCorrelateCommand(stdout),
		newReanalyzeCommand(stdout),
		newRefreshCommand(stdout),
		newServeCommand(stdout, stderr),
		newSetupCheckCommand(stdout),
		newVersionCommand(stdout, build),
	)
	return command
}
