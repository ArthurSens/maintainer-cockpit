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

func newReanalyzeCommand(stdout io.Writer) *cobra.Command {
	var configPath string
	var databasePath string
	var collectionID string
	command := &cobra.Command{
		Use:   "reanalyze",
		Short: "Enqueue explicit collection-wide model reanalysis",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			loaded, err := config.Load(configPath)
			if err != nil {
				return err
			}
			if collectionID == "" {
				return errors.New("--collection is required")
			}
			found := false
			for _, collection := range loaded.Collections {
				if collection.ID == collectionID {
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("collection %q is not configured", collectionID)
			}
			store, err := storage.Open(databasePath)
			if err != nil {
				return err
			}
			defer store.Close()
			enqueued, coalesced, err := store.EnqueueCollectionAnalysis(
				command.Context(), collectionID, true, time.Now().UTC(),
			)
			if err != nil {
				return err
			}
			fmt.Fprintf(
				stdout, "reanalysis requested: %d enqueued, %d coalesced\n",
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
