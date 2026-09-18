package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ArthurSens/maintainer-cockpit/internal/config"
	"github.com/ArthurSens/maintainer-cockpit/internal/githubapp"
)

func newSetupCheckCommand(stdout io.Writer) *cobra.Command {
	var configPath string
	command := &cobra.Command{
		Use:   "setup-check",
		Short: "Validate configured GitHub authentication profiles",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return runSetupCheck(command.Context(), stdout, configPath)
		},
	}
	command.Flags().StringVar(&configPath, "config", "config/maintainer-cockpit.yaml", "path to configuration")
	return command
}

func runSetupCheck(ctx context.Context, stdout io.Writer, configPath string) error {
	loaded, err := config.Load(configPath)
	if err != nil {
		return err
	}
	if loaded.GitHub == nil || loaded.GitHub.Authentication == nil {
		return errors.New("github configuration is required for setup-check; configure github.authentication")
	}
	router, err := newGitHubClients(loaded)
	if err != nil {
		return err
	}
	results := router.SetupCheck(ctx, configuredRepositories(loaded)...)
	firstInvalid := false
	anyValid := false
	profileIndex := make(map[string]int)
	for owner, assignment := range loaded.GitHub.Authentication.Owners {
		for index, profile := range assignment.Profiles {
			profileIndex[strings.ToLower(owner)+"\x00"+profile] = index
		}
	}
	for _, result := range results {
		status := "OK"
		detail := ""
		if result.Valid {
			anyValid = true
			if result.Type == githubapp.ProfileGitHubApp {
				detail = fmt.Sprintf(" app=%s account=%s", result.Setup.AppSlug, result.Setup.AccountLogin)
			} else {
				detail = fmt.Sprintf(" account=%s REST+GraphQL", result.Setup.AccountLogin)
			}
		} else {
			status = "WARNING"
			detail = ": " + result.Err.Error()
			if profileIndex[strings.ToLower(result.Owner)+"\x00"+result.Profile] == 0 {
				firstInvalid = true
				status = "ERROR"
			}
		}
		fmt.Fprintf(
			stdout, "%s owner=%s repository=%s profile=%s type=%s%s\n",
			status, result.Owner, result.Repository, result.Profile, result.Type, detail,
		)
	}
	if firstInvalid {
		return errors.New("one or more primary GitHub authentication routes are invalid")
	}
	if !anyValid {
		return errors.New("all GitHub authentication routes are invalid")
	}
	return nil
}

func newGitHubClients(loaded config.Config) (*githubapp.Router, error) {
	if loaded.GitHub == nil || loaded.GitHub.Authentication == nil {
		return nil, errors.New("github.authentication is required for collection")
	}
	authentication := loaded.GitHub.Authentication
	common := githubapp.Options{
		DiscoveryPlans:           discoveryPlans(loaded),
		CollectContext:           true,
		CollectContribution:      true,
		CollectProgress:          true,
		RequireMembersRead:       requiresMembersRead(loaded),
		ContributionWindowDays:   contributionWindowDays(loaded),
		ContributionRepositories: contributionRepositories(loaded),
	}
	names := make([]string, 0, len(authentication.Profiles))
	for name := range authentication.Profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	clients := make(map[string]githubapp.Profile, len(names))
	for _, name := range names {
		configured := authentication.Profiles[name]
		var (
			client *githubapp.Client
			err    error
		)
		switch configured.Type {
		case "github_app":
			secret, resolveErr := config.ResolveSecret(*configured.PrivateKey)
			if resolveErr != nil {
				return nil, fmt.Errorf("configure GitHub profile %q: %w", name, resolveErr)
			}
			options := common
			options.AppID = *configured.AppID
			options.InstallationID = *configured.InstallationID
			options.PrivateKeyPEM = secret
			client, err = githubapp.New(options)
			if err == nil {
				clients[name] = githubapp.Profile{
					Name: name, Type: githubapp.ProfileGitHubApp, Client: client,
				}
			}
		case "fine_grained_pat":
			secret, resolveErr := config.ResolveSecret(*configured.Token)
			if resolveErr != nil {
				return nil, fmt.Errorf("configure GitHub profile %q: %w", name, resolveErr)
			}
			client, err = githubapp.NewPAT(githubapp.PATOptions{
				Token: string(secret), DiscoveryPlans: common.DiscoveryPlans,
				CollectContext:           common.CollectContext,
				CollectContribution:      common.CollectContribution,
				CollectProgress:          common.CollectProgress,
				RequireMembersRead:       common.RequireMembersRead,
				ContributionWindowDays:   common.ContributionWindowDays,
				ContributionRepositories: common.ContributionRepositories,
			})
			if err == nil {
				clients[name] = githubapp.Profile{
					Name: name, Type: githubapp.ProfileFineGrainedPAT, Client: client,
				}
			}
		default:
			err = fmt.Errorf("unsupported profile type %q", configured.Type)
		}
		if err != nil {
			return nil, fmt.Errorf("configure GitHub profile %q: %w", name, err)
		}
	}
	routes := make(map[string][]githubapp.Profile, len(authentication.Owners))
	for owner, assignment := range authentication.Owners {
		for _, name := range assignment.Profiles {
			routes[owner] = append(routes[owner], clients[name])
		}
	}
	router, err := githubapp.NewRouter(routes)
	if err != nil {
		return nil, fmt.Errorf("configure GitHub router: %w", err)
	}
	return router, nil
}

func requiresMembersRead(loaded config.Config) bool {
	for _, collection := range loaded.Collections {
		if collection.Authorization != nil &&
			(len(collection.Authorization.Organizations) > 0 || len(collection.Authorization.Teams) > 0) {
			return true
		}
	}
	return false
}

func discoveryPlans(loaded config.Config) map[string][]githubapp.DiscoveryTarget {
	plans := make(map[string][]githubapp.DiscoveryTarget)
	for _, collection := range loaded.Collections {
		for _, repository := range collection.Repositories {
			discovery := config.RepositoryDiscovery{Mode: "all_open"}
			for identity, configured := range collection.Discovery {
				if strings.EqualFold(identity, repository) {
					discovery = configured
					break
				}
			}
			plans[repository] = append(plans[repository], githubapp.DiscoveryTarget{
				CollectionID: collection.ID, Mode: discovery.Mode, Query: discovery.Query,
			})
		}
	}
	return plans
}

func contributionWindowDays(loaded config.Config) int {
	window := 14
	for _, collection := range loaded.Collections {
		if collection.Contribution != nil &&
			collection.Contribution.UnusualActivity != nil &&
			collection.Contribution.UnusualActivity.WindowDays != nil &&
			*collection.Contribution.UnusualActivity.WindowDays > window {
			window = *collection.Contribution.UnusualActivity.WindowDays
		}
	}
	return window
}

func contributionRepositories(loaded config.Config) map[string][]string {
	result := make(map[string][]string)
	for _, collection := range loaded.Collections {
		for _, source := range collection.Repositories {
			seen := make(map[string]struct{}, len(result[source])+len(collection.Repositories))
			for _, repository := range result[source] {
				seen[repository] = struct{}{}
			}
			for _, repository := range collection.Repositories {
				if _, exists := seen[repository]; exists {
					continue
				}
				seen[repository] = struct{}{}
				result[source] = append(result[source], repository)
			}
		}
	}
	for source := range result {
		sort.Strings(result[source])
	}
	return result
}
