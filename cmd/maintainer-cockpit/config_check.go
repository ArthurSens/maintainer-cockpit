package main

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/ArthurSens/maintainer-cockpit/internal/config"
)

func newConfigCheckCommand(stdout io.Writer) *cobra.Command {
	var configPath string
	command := &cobra.Command{
		Use:   "config-check",
		Short: "Validate Maintainer Cockpit configuration",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			loaded, err := config.Load(configPath)
			if err != nil {
				return err
			}
			noun := "collections"
			if len(loaded.Collections) == 1 {
				noun = "collection"
			}
			fmt.Fprintf(stdout, "configuration is valid: %d %s\n", len(loaded.Collections), noun)
			return nil
		},
	}
	command.Flags().StringVar(&configPath, "config", "config/maintainer-cockpit.yaml", "path to configuration")
	return command
}
