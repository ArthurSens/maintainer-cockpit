package contribution

import (
	"testing"
	"time"

	"github.com/ArthurSens/maintainer-cockpit/internal/config"
)

func TestEvaluateClassifiesCollectionSpecificHistory(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input Input
		want  Level
	}{
		{
			name: "repository association is established",
			input: Input{
				Complete: true,
				Repositories: map[string]RepositoryHistory{
					"acme/widgets": {Association: "MEMBER"},
				},
			},
			want: LevelEstablished,
		},
		{
			name: "three merged pull requests is established by default",
			input: Input{
				Complete: true,
				Repositories: map[string]RepositoryHistory{
					"acme/widgets": {Merged: 2},
					"acme/gadgets": {Merged: 1},
				},
			},
			want: LevelEstablished,
		},
		{
			name: "any prior contribution is some history",
			input: Input{
				Complete: true,
				Repositories: map[string]RepositoryHistory{
					"acme/widgets": {ClosedUnmerged: 1},
				},
			},
			want: LevelSomeHistory,
		},
		{
			name: "no prior contribution is new",
			input: Input{
				Complete:     true,
				Repositories: map[string]RepositoryHistory{},
			},
			want: LevelNew,
		},
		{
			name: "the focal open pull request is not prior history",
			input: Input{
				Complete: true,
				Repositories: map[string]RepositoryHistory{
					"acme/widgets": {Open: 1},
				},
				CurrentOpen: map[string]int{"acme/widgets": 1},
			},
			want: LevelNew,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got := Evaluate(test.input, DefaultPolicy(), time.Now())
			if got.Level != test.want {
				t.Errorf("Evaluate().Level = %q, want %q", got.Level, test.want)
			}
		})
	}
}

func TestEvaluateUsesConfiguredEstablishedThreshold(t *testing.T) {
	t.Parallel()

	policy := DefaultPolicy()
	policy.EstablishedMergedPRs = Threshold{Value: 5, Source: SourceLocal}
	got := Evaluate(Input{
		Complete: true,
		Repositories: map[string]RepositoryHistory{
			"acme/widgets": {Merged: 3},
		},
	}, policy, time.Now())

	if got.Level != LevelSomeHistory {
		t.Errorf("Evaluate().Level = %q, want %q", got.Level, LevelSomeHistory)
	}
}

func TestPolicyFromConfigMarksLocalThresholds(t *testing.T) {
	t.Parallel()

	established, organizations := 5, 4
	policy := PolicyFromConfig(&config.Contribution{
		EstablishedMergedPRs: &established,
		UnusualActivity: &config.UnusualActivity{
			MinOrganizations: &organizations,
		},
	})
	if policy.EstablishedMergedPRs != (Threshold{Value: 5, Source: SourceLocal}) {
		t.Errorf("EstablishedMergedPRs = %+v", policy.EstablishedMergedPRs)
	}
	if policy.UnusualActivity.MinOrganizations != (Threshold{Value: 4, Source: SourceLocal}) {
		t.Errorf("MinOrganizations = %+v", policy.UnusualActivity.MinOrganizations)
	}
	if policy.UnusualActivity.WindowDays.Source != SourceDefault {
		t.Errorf("WindowDays = %+v, want product default", policy.UnusualActivity.WindowDays)
	}
}

func TestUnusualActivityRequiresEveryThresholdAndExpires(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	recent := make([]RepositoryActivity, 0, 20)
	for index := range 20 {
		owner := "one"
		if index >= 7 {
			owner = "two"
		}
		if index >= 14 {
			owner = "three"
		}
		recent = append(recent, RepositoryActivity{
			Repository: owner + "/repo-" + string(rune('a'+index)),
			OccurredAt: now.Add(-13 * 24 * time.Hour),
		})
	}
	input := Input{
		Complete:         true,
		AccountCreatedAt: now.Add(-89 * 24 * time.Hour),
		RecentActivity:   recent,
		Repositories:     map[string]RepositoryHistory{},
	}

	got := Evaluate(input, DefaultPolicy(), now)
	if got.UnusualActivity == nil || !got.UnusualActivity.Detected {
		t.Fatalf("Evaluate().UnusualActivity = %+v, want detected", got.UnusualActivity)
	}

	expired := Evaluate(input, DefaultPolicy(), now.Add(2*24*time.Hour))
	if expired.UnusualActivity == nil || expired.UnusualActivity.Detected {
		t.Errorf("expired unusual activity = %+v, want not detected", expired.UnusualActivity)
	}
}

func TestEvaluateSuppressesJudgmentsWhenHistoryIsIncomplete(t *testing.T) {
	t.Parallel()

	got := Evaluate(Input{Complete: false}, DefaultPolicy(), time.Now())
	if got.Level != "" {
		t.Errorf("Evaluate().Level = %q, want suppressed", got.Level)
	}
	if got.UnusualActivity != nil {
		t.Errorf("Evaluate().UnusualActivity = %+v, want suppressed", got.UnusualActivity)
	}
	if got.Completeness != CompletenessIncomplete {
		t.Errorf("Evaluate().Completeness = %q, want incomplete", got.Completeness)
	}
}
