package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/ArthurSens/maintainer-cockpit/internal/storage"
)

type fixtureFile struct {
	PullRequests []fixturePullRequest `json:"pullRequests"`
}

type fixturePullRequest struct {
	CollectionID string `json:"collectionID"`
	storage.PullRequest
}

// SeedFixture imports explicit development fixture data into storage. It is
// intended for demos and container smoke tests, not collection.
func (a *Application) SeedFixture(ctx context.Context, path string) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open fixture %q: %w", path, err)
	}
	defer file.Close()

	var fixture fixtureFile
	decoder := json.NewDecoder(io.LimitReader(file, 10<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&fixture); err != nil {
		return fmt.Errorf("decode fixture %q: %w", path, err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err != nil {
			return fmt.Errorf("decode trailing fixture data: %w", err)
		}
		return errors.New("fixture must contain exactly one JSON document")
	}
	if len(fixture.PullRequests) == 0 {
		return errors.New("fixture must contain at least one pull request")
	}
	for index, entry := range fixture.PullRequests {
		if !collectionIDRE.MatchString(entry.CollectionID) {
			return fmt.Errorf("pullRequests[%d]: invalid collection ID %q", index, entry.CollectionID)
		}
		if err := a.store.UpsertPullRequest(ctx, entry.CollectionID, entry.PullRequest); err != nil {
			return fmt.Errorf("pullRequests[%d]: %w", index, err)
		}
	}
	return nil
}
