package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/ArthurSens/maintainer-cockpit/internal/analysis"
	listquery "github.com/ArthurSens/maintainer-cockpit/internal/query"
)

var (
	ErrPullRequestNotFound          = errors.New("pull request not found")
	ErrImportantPullRequestNotFound = errors.New("important pull request not found")
)

// PersonalPullRequestState is private to one GitHub identity and collection.
type PersonalPullRequestState struct {
	Important bool                    `json:"important"`
	Hidden    *HiddenPullRequestState `json:"hidden,omitempty"`
}

// MarkPullRequestImportant adds a collection PR to one user's Mine view.
func (s *Store) MarkPullRequestImportant(
	ctx context.Context,
	userID int64,
	collectionID, repository string,
	number int,
) error {
	if userID <= 0 {
		return errors.New("user ID must be positive")
	}
	result, err := s.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO important_pull_requests (
			user_id, collection_id, repository, number
		)
		SELECT ?, collection_id, repository, number
		FROM collection_pull_requests
		WHERE collection_id = ? AND repository = ? AND number = ?
	`, userID, collectionID, repository, number)
	if err != nil {
		return fmt.Errorf("mark pull request Important: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect Important write: %w", err)
	}
	if affected == 0 {
		var exists int
		err := s.db.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM important_pull_requests
			WHERE user_id = ? AND collection_id = ? AND repository = ? AND number = ?
		`, userID, collectionID, repository, number).Scan(&exists)
		if err != nil {
			return fmt.Errorf("find Important pull request: %w", err)
		}
		if exists == 0 {
			return ErrPullRequestNotFound
		}
	}
	return nil
}

// UnmarkPullRequestImportant removes a collection PR from one user's Mine view.
func (s *Store) UnmarkPullRequestImportant(
	ctx context.Context,
	userID int64,
	collectionID, repository string,
	number int,
) error {
	result, err := s.db.ExecContext(ctx, `
		DELETE FROM important_pull_requests
		WHERE user_id = ? AND collection_id = ? AND repository = ? AND number = ?
	`, userID, collectionID, repository, number)
	if err != nil {
		return fmt.Errorf("unmark pull request Important: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect Important deletion: %w", err)
	}
	if affected == 0 {
		return ErrImportantPullRequestNotFound
	}
	return nil
}

// AttachImportantPullRequests adds only the requesting user's private marker.
func (s *Store) AttachImportantPullRequests(
	ctx context.Context,
	userID int64,
	collectionID string,
	pullRequests []PullRequest,
) error {
	rows, err := s.db.QueryContext(ctx, `
		SELECT repository, number
		FROM important_pull_requests
		WHERE user_id = ? AND collection_id = ?
	`, userID, collectionID)
	if err != nil {
		return fmt.Errorf("list Important pull requests: %w", err)
	}
	defer rows.Close()
	important := make(map[string]struct{})
	for rows.Next() {
		var repository string
		var number int
		if err := rows.Scan(&repository, &number); err != nil {
			return fmt.Errorf("scan Important pull request: %w", err)
		}
		important[importantKey(repository, number)] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate Important pull requests: %w", err)
	}
	for index := range pullRequests {
		if _, exists := important[importantKey(
			pullRequests[index].Repository, pullRequests[index].Number,
		)]; exists {
			if pullRequests[index].Personal == nil {
				pullRequests[index].Personal = &PersonalPullRequestState{}
			}
			pullRequests[index].Personal.Important = true
		}
	}
	return nil
}

// AttachImportantPullRequest adds only the requesting user's private marker.
func (s *Store) AttachImportantPullRequest(
	ctx context.Context,
	userID int64,
	collectionID string,
	detail *PullRequestDetail,
) error {
	var exists int
	err := s.db.QueryRowContext(ctx, `
		SELECT 1 FROM important_pull_requests
		WHERE user_id = ? AND collection_id = ? AND repository = ? AND number = ?
	`, userID, collectionID, detail.Repository, detail.Number).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load Important pull request: %w", err)
	}
	if detail.Personal == nil {
		detail.Personal = &PersonalPullRequestState{}
	}
	detail.Personal.Important = true
	return nil
}

// ListMinePullRequestsPage returns authored and personally Important PRs.
func (s *Store) ListMinePullRequestsPage(
	ctx context.Context,
	collectionID string,
	userID int64,
	options listquery.Options,
) (PullRequestPage, error) {
	pullRequests, err := s.ListPullRequests(ctx, collectionID)
	if err != nil {
		return PullRequestPage{}, err
	}
	if err := s.AttachImportantPullRequests(ctx, userID, collectionID, pullRequests); err != nil {
		return PullRequestPage{}, err
	}
	mine := pullRequests[:0]
	for _, pr := range pullRequests {
		if pr.AuthorID == userID || pr.Personal != nil && pr.Personal.Important {
			mine = append(mine, pr)
		}
	}
	total := len(mine)
	matched := mine[:0]
	for _, pr := range mine {
		if minePullRequestMatches(pr, options) {
			matched = append(matched, pr)
		}
	}
	sortMinePullRequests(matched, options)
	matchedCount := len(matched)
	if options.Offset > len(matched) {
		matched = nil
	} else {
		matched = matched[options.Offset:]
	}
	var nextCursor string
	if len(matched) > options.Limit {
		matched = matched[:options.Limit]
		nextCursor, err = options.NextCursor(options.Offset + len(matched))
		if err != nil {
			return PullRequestPage{}, err
		}
	}
	return PullRequestPage{
		PullRequests: matched,
		Total:        total,
		Matched:      matchedCount,
		NextCursor:   nextCursor,
	}, nil
}

func minePullRequestMatches(pr PullRequest, options listquery.Options) bool {
	if options.Repository != "" && pr.Repository != options.Repository {
		return false
	}
	if options.Review != "" && pr.ReviewState != options.Review {
		return false
	}
	if options.Quality != "" && pr.Quality.Level != options.Quality {
		return false
	}
	if options.ReviewLoad != "" &&
		(pr.Analysis.ReviewCognitiveLoad == nil ||
			pr.Analysis.ReviewCognitiveLoad.Overall != options.ReviewLoad) {
		return false
	}
	if options.AnalysisStatus != "" && pr.Analysis.AnalysisStatus != options.AnalysisStatus {
		return false
	}
	if len(options.WaitingOn) > 0 && !mineWaitingMatches(pr.Analysis.WaitingOn, options) {
		return false
	}
	if options.Query == "" {
		return true
	}
	query := strings.ToLower(options.Query)
	number := strings.TrimPrefix(query, "#")
	return strings.Contains(strings.ToLower(pr.Title), query) ||
		strings.Contains(strings.ToLower(pr.Author), query) ||
		strings.Contains(strings.ToLower(pr.Repository), query) ||
		strconv.Itoa(pr.Number) == number
}

func mineWaitingMatches(waiting []analysis.WaitingState, options listquery.Options) bool {
	parties := make(map[string]struct{}, len(waiting))
	for _, item := range waiting {
		parties[item.Party] = struct{}{}
	}
	matches := 0
	for _, party := range options.WaitingOn {
		if _, exists := parties[party]; exists {
			matches++
		}
	}
	if options.WaitingMode == "all" {
		return matches == len(options.WaitingOn)
	}
	return matches > 0
}

func sortMinePullRequests(pullRequests []PullRequest, options listquery.Options) {
	sorts := strings.Split(options.Sort, ",")
	orders := strings.Split(options.Order, ",")
	sort.SliceStable(pullRequests, func(i, j int) bool {
		left, right := pullRequests[i], pullRequests[j]
		for index, key := range sorts {
			if key == "review_load" {
				leftAvailable := mineReviewLoadAvailable(left.Analysis.ReviewCognitiveLoad)
				rightAvailable := mineReviewLoadAvailable(right.Analysis.ReviewCognitiveLoad)
				if leftAvailable != rightAvailable {
					return leftAvailable
				}
			}
			comparison := compareMinePullRequests(left, right, key)
			if comparison == 0 {
				continue
			}
			if index < len(orders) && orders[index] == "desc" {
				return comparison > 0
			}
			return comparison < 0
		}
		if left.Repository != right.Repository {
			return left.Repository < right.Repository
		}
		return left.Number < right.Number
	})
}

func compareMinePullRequests(left, right PullRequest, key string) int {
	switch key {
	case "updated":
		return left.UpdatedAt.Compare(right.UpdatedAt)
	case "number":
		return left.Number - right.Number
	case "repository":
		return strings.Compare(left.Repository, right.Repository)
	case "title":
		return strings.Compare(strings.ToLower(left.Title), strings.ToLower(right.Title))
	case "author":
		return strings.Compare(strings.ToLower(left.Author), strings.ToLower(right.Author))
	case "churn":
		return pullRequestChurn(left) - pullRequestChurn(right)
	case "review":
		return strings.Compare(left.ReviewState, right.ReviewState)
	case "quality":
		return mineQualityRank(left.Quality.Level) - mineQualityRank(right.Quality.Level)
	case "review_load":
		return mineReviewLoadRank(left.Analysis.ReviewCognitiveLoad) -
			mineReviewLoadRank(right.Analysis.ReviewCognitiveLoad)
	default:
		return 0
	}
}

func pullRequestChurn(pr PullRequest) int {
	return pr.Additions + pr.Deletions
}

func mineQualityRank(level string) int {
	return map[string]int{
		"no_concerns": 0, "review_suggested": 1, "strong_concerns": 2,
	}[level]
}

func mineReviewLoadAvailable(load *analysis.ReviewCognitiveLoad) bool {
	return load != nil &&
		(load.Overall == "low" || load.Overall == "medium" || load.Overall == "high")
}

func mineReviewLoadRank(load *analysis.ReviewCognitiveLoad) int {
	if !mineReviewLoadAvailable(load) {
		return 0
	}
	return map[string]int{
		"low": 1, "medium": 2, "high": 3,
	}[load.Overall]
}

func importantKey(repository string, number int) string {
	return repository + "\x00" + strconv.Itoa(number)
}
