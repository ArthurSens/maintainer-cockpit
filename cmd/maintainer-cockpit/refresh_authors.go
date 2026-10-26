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

func newRefreshAuthorsCommand(stdout io.Writer) *cobra.Command {
	var configPath, databasePath, collectionID string
	command := &cobra.Command{
		Use: "refresh-authors", Short: "Enqueue public account and activity evidence for a collection",
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			if collectionID == "" {
				return errors.New("--collection is required")
			}
			loaded, err := config.Load(configPath)
			if err != nil {
				return err
			}
			enabled := false
			for _, collection := range loaded.Collections {
				if collection.ID == collectionID && collection.Contribution != nil {
					enabled = true
				}
			}
			if !enabled {
				return fmt.Errorf("collection %q does not enable contribution evidence", collectionID)
			}
			store, err := storage.Open(databasePath)
			if err != nil {
				return err
			}
			defer store.Close()
			count, err := store.EnqueueAncillaryAuthorRefresh(command.Context(), collectionID, time.Now().UTC())
			if err != nil {
				return err
			}
			fmt.Fprintf(stdout, "author refresh requested: %d enqueued; active duplicates coalesced\n", count)
			return nil
		},
	}
	command.Flags().StringVar(&configPath, "config", "config/maintainer-cockpit.yaml", "path to configuration")
	command.Flags().StringVar(&databasePath, "database", "data/maintainer-cockpit.db", "path to SQLite database")
	command.Flags().StringVar(&collectionID, "collection", "", "configured collection ID")
	return command
}
