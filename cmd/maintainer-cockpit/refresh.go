package main

import (
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/ArthurSens/maintainer-cockpit/internal/config"
	"github.com/ArthurSens/maintainer-cockpit/internal/storage"
)

func newRefreshCommand(stdout io.Writer) *cobra.Command {
	var configPath string
	var databasePath string
	var repository string
	command := &cobra.Command{
		Use:   "refresh",
		Short: "Enqueue an administrator-requested repository refresh",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			loaded, err := config.Load(configPath)
			if err != nil {
				return err
			}
			store, err := storage.Open(databasePath)
			if err != nil {
				return err
			}
			defer store.Close()
			repositories := configuredRepositories(loaded)
			if repository != "" {
				repositories = []string{repository}
			}
			enqueued := 0
			for _, candidate := range repositories {
				added, err := store.EnqueueRefreshWithForce(
					command.Context(), candidate, time.Now().UTC(), true,
				)
				if err != nil {
					return err
				}
				if added {
					enqueued++
				}
			}
			fmt.Fprintf(stdout, "refresh requested: %d enqueued, %d coalesced\n", enqueued, len(repositories)-enqueued)
			return nil
		},
	}
	command.Flags().StringVar(&configPath, "config", "config/maintainer-cockpit.yaml", "path to configuration")
	command.Flags().StringVar(&databasePath, "database", "data/maintainer-cockpit.db", "path to SQLite database")
	command.Flags().StringVar(&repository, "repository", "", "refresh only this configured owner/repository")
	return command
}

func configuredRepositories(loaded config.Config) []string {
	seen := make(map[string]struct{})
	var repositories []string
	for _, collection := range loaded.Collections {
		for _, repository := range collection.Repositories {
			if _, exists := seen[repository]; exists {
				continue
			}
			seen[repository] = struct{}{}
			repositories = append(repositories, repository)
		}
	}
	return repositories
}
