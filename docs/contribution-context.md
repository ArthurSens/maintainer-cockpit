# Contribution context

Contribution context is factual and collection-specific. It never changes pull
request quality.

## Levels

- **Established**: the author has an `OWNER`, `MEMBER`, or `COLLABORATOR`
  association in a configured repository, or has at least three merged pull
  requests across the collection.
- **Some history**: the author has prior open, merged, or closed-unmerged pull
  request activity below the established threshold.
- **New to collection**: no prior pull request activity was found. Pull requests
  currently in the collection do not count as prior activity.

The established merged-PR threshold defaults to `3` and can be configured per
collection. The public author page shows repository association and merged,
closed-unmerged, and open counts separately.

## Experimental unusual activity

The independent experimental signal is detected only when all of these default
conditions hold:

- the account is under 90 days old;
- the author opened pull requests in at least 20 repositories;
- those repositories span at least three organizations;
- the activity occurred within the last 14 days.

All four thresholds can be tuned per collection. Contribution history and the
experimental activity evidence share the collection's required five-field UTC
`contribution.schedule`. At each match, work is built independently from the
latest stored open-PR authors; it does not wait for a core refresh. Omitting
the contribution block disables these periodic GitHub calls. Evaluation uses
the current time, so age and activity-window conditions may expire before the
next collection.

GitHub search is capped at 1,000 results and may report incomplete results. A
failed or truncated account, collection-history, or recent-activity request
marks author history incomplete. In that state, Maintainer Cockpit suppresses
both the contribution level and experimental signal instead of judging partial
evidence.
