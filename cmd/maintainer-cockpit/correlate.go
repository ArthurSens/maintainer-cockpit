package main

import (
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/ArthurSens/maintainer-cockpit/internal/config"
	"github.com/ArthurSens/maintainer-cockpit/internal/storage"
)

func newCorrelateCommand(stdout io.Writer) *cobra.Command {
	var configPath string
	var databasePath string
	var collectionID string
	command := &cobra.Command{
		Use:   "correlate",
		Short: "Enqueue explicit collection feature correlation",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			loaded, err := config.Load(configPath)
			if err != nil {
				return err
			}
			var configured *config.Collection
			for index := range loaded.Collections {
				if loaded.Collections[index].ID == collectionID {
					configured = &loaded.Collections[index]
					break
				}
			}
			if collectionID == "" {
				return errors.New("--collection is required")
			}
			if configured == nil {
				return fmt.Errorf("collection %q is not configured", collectionID)
			}
			if configured.FeatureCorrelation == nil {
				return fmt.Errorf("collection %q does not enable feature correlation", collectionID)
			}
			store, err := storage.Open(databasePath)
			if err != nil {
				return err
			}
			defer store.Close()
			enqueued, coalesced, err := store.EnqueueCollectionCorrelation(
				command.Context(), collectionID, true, time.Now().UTC(),
			)
			if err != nil {
				return err
			}
			fmt.Fprintf(
				stdout, "correlation requested: %d enqueued, %d coalesced\n",
				enqueued, coalesced,
			)
			return nil
		},
	}
	command.Flags().StringVar(&configPath, "config", "config/maintainer-cockpit.yaml", "path to configuration")
	command.Flags().StringVar(&databasePath, "database", "data/maintainer-cockpit.db", "path to SQLite database")
	command.Flags().StringVar(&collectionID, "collection", "", "configured collection ID")
	return command
}
