// Package contribution evaluates factual, collection-specific author context.
// It deliberately has no dependency on pull-request quality.
package contribution

import (
	"strings"
	"time"

	"github.com/ArthurSens/maintainer-cockpit/internal/config"
)

type Level string

const (
	LevelEstablished Level = "established"
	LevelSomeHistory Level = "some_history"
	LevelNew         Level = "new_to_collection"

	CompletenessComplete   = "complete"
	CompletenessIncomplete = "incomplete"
	SourceDefault          = "default"
	SourceLocal            = "local"
)

// Threshold records a value and whether it is product-default or local policy.
type Threshold struct {
	Value  int    `json:"value"`
	Source string `json:"source"`
}

// Policy contains all administrator-tunable author-context thresholds.
type Policy struct {
	EstablishedMergedPRs Threshold `json:"establishedMergedPRs"`
	UnusualActivity      struct {
		AccountAgeDays   Threshold `json:"accountAgeDays"`
		WindowDays       Threshold `json:"windowDays"`
		MinRepositories  Threshold `json:"minRepositories"`
		MinOrganizations Threshold `json:"minOrganizations"`
	} `json:"unusualActivity"`
}

// DefaultPolicy returns the documented product policy.
func DefaultPolicy() Policy {
	policy := Policy{
		EstablishedMergedPRs: Threshold{Value: 3, Source: SourceDefault},
	}
	policy.UnusualActivity.AccountAgeDays = Threshold{Value: 90, Source: SourceDefault}
	policy.UnusualActivity.WindowDays = Threshold{Value: 14, Source: SourceDefault}
	policy.UnusualActivity.MinRepositories = Threshold{Value: 20, Source: SourceDefault}
	policy.UnusualActivity.MinOrganizations = Threshold{Value: 3, Source: SourceDefault}
	return policy
}

// PolicyFromConfig overlays local collection values on product defaults.
func PolicyFromConfig(value *config.Contribution) Policy {
	policy := DefaultPolicy()
	if value == nil {
		return policy
	}
	if value.EstablishedMergedPRs != nil {
		policy.EstablishedMergedPRs = Threshold{Value: *value.EstablishedMergedPRs, Source: SourceLocal}
	}
	if unusual := value.UnusualActivity; unusual != nil {
		apply := func(target *Threshold, configured *int) {
			if configured != nil {
				*target = Threshold{Value: *configured, Source: SourceLocal}
			}
		}
		apply(&policy.UnusualActivity.AccountAgeDays, unusual.AccountAgeDays)
		apply(&policy.UnusualActivity.WindowDays, unusual.WindowDays)
		apply(&policy.UnusualActivity.MinRepositories, unusual.MinRepositories)
		apply(&policy.UnusualActivity.MinOrganizations, unusual.MinOrganizations)
	}
	return policy
}

// RepositoryHistory is factual contribution evidence for one configured repo.
type RepositoryHistory struct {
	Association    string `json:"association"`
	Merged         int    `json:"merged"`
	ClosedUnmerged int    `json:"closedUnmerged"`
	Open           int    `json:"open"`
}

// RepositoryActivity is one recent repository with author PR activity.
type RepositoryActivity struct {
	Repository string    `json:"repository"`
	OccurredAt time.Time `json:"occurredAt"`
}

// Input is all successfully collected evidence for an author in a collection.
type Input struct {
	Complete         bool
	AccountCreatedAt time.Time
	Repositories     map[string]RepositoryHistory
	CurrentOpen      map[string]int
	RecentActivity   []RepositoryActivity
}

// Counts keeps merged, closed-unmerged, and open facts separate.
type Counts struct {
	Merged         int `json:"merged"`
	ClosedUnmerged int `json:"closedUnmerged"`
	Open           int `json:"open"`
}

// UnusualActivity is an independent, inspectable experimental signal.
type UnusualActivity struct {
	Detected       bool `json:"detected"`
	Experimental   bool `json:"experimental"`
	AccountAgeDays int  `json:"accountAgeDays"`
	Repositories   int  `json:"repositories"`
	Organizations  int  `json:"organizations"`
	WindowDays     int  `json:"windowDays"`
}

// Evaluation is the public collection-specific author context.
type Evaluation struct {
	Completeness       string                       `json:"completeness"`
	Level              Level                        `json:"level,omitempty"`
	Association        string                       `json:"association,omitempty"`
	Counts             Counts                       `json:"counts"`
	Repositories       map[string]RepositoryHistory `json:"repositories"`
	UnusualActivity    *UnusualActivity             `json:"unusualActivity,omitempty"`
	Policy             Policy                       `json:"policy"`
	QualityIndependent bool                         `json:"qualityIndependent"`
}

// Evaluate derives named context and the independent unusual-activity signal.
func Evaluate(input Input, policy Policy, now time.Time) Evaluation {
	result := Evaluation{
		Completeness:       CompletenessIncomplete,
		Repositories:       input.Repositories,
		Policy:             policy,
		QualityIndependent: true,
	}
	if result.Repositories == nil {
		result.Repositories = make(map[string]RepositoryHistory)
	}
	for _, history := range result.Repositories {
		result.Counts.Merged += history.Merged
		result.Counts.ClosedUnmerged += history.ClosedUnmerged
		result.Counts.Open += history.Open
		if associationRank(history.Association) > associationRank(result.Association) {
			result.Association = history.Association
		}
	}
	if !input.Complete {
		return result
	}

	result.Completeness = CompletenessComplete
	priorOpen := result.Counts.Open
	for repository, count := range input.CurrentOpen {
		if count < 0 {
			continue
		}
		priorOpen -= min(count, result.Repositories[repository].Open)
	}
	switch {
	case associationRank(result.Association) >= associationRank("COLLABORATOR"),
		result.Counts.Merged >= policy.EstablishedMergedPRs.Value:
		result.Level = LevelEstablished
	case result.Counts.Merged+result.Counts.ClosedUnmerged+priorOpen > 0:
		result.Level = LevelSomeHistory
	default:
		result.Level = LevelNew
	}

	cutoff := now.AddDate(0, 0, -policy.UnusualActivity.WindowDays.Value)
	repositories := make(map[string]struct{})
	organizations := make(map[string]struct{})
	for _, activity := range input.RecentActivity {
		if activity.Repository == "" || activity.OccurredAt.Before(cutoff) ||
			activity.OccurredAt.After(now) {
			continue
		}
		repositories[strings.ToLower(activity.Repository)] = struct{}{}
		if owner, _, ok := strings.Cut(activity.Repository, "/"); ok && owner != "" {
			organizations[strings.ToLower(owner)] = struct{}{}
		}
	}
	accountAge := -1
	young := false
	if !input.AccountCreatedAt.IsZero() && !input.AccountCreatedAt.After(now) {
		accountAge = int(now.Sub(input.AccountCreatedAt).Hours() / 24)
		young = accountAge < policy.UnusualActivity.AccountAgeDays.Value
	}
	result.UnusualActivity = &UnusualActivity{
		Detected: young &&
			len(repositories) >= policy.UnusualActivity.MinRepositories.Value &&
			len(organizations) >= policy.UnusualActivity.MinOrganizations.Value,
		Experimental:   true,
		AccountAgeDays: accountAge,
		Repositories:   len(repositories),
		Organizations:  len(organizations),
		WindowDays:     policy.UnusualActivity.WindowDays.Value,
	}
	return result
}

func associationRank(value string) int {
	switch strings.ToUpper(value) {
	case "OWNER":
		return 5
	case "MEMBER":
		return 4
	case "COLLABORATOR":
		return 3
	case "CONTRIBUTOR":
		return 2
	case "FIRST_TIME_CONTRIBUTOR", "FIRST_TIMER":
		return 1
	default:
		return 0
	}
}
