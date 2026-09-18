import test from "node:test";
import assert from "node:assert/strict";

import {
  humanizeAge,
  humanizeBytes,
  humanizeUntil,
  presentAnalysis,
  presentAnalysisOperations,
  presentSchedules,
  presentContribution,
  presentDecisionBrief,
  presentChurn,
  presentExternalURL,
  presentFinding,
  presentPullRequest,
  presentQuality,
  presentQualityPolicy,
  presentRefresh,
  presentRelationshipContext,
  presentRouteWarnings,
  presentGitHubRoutes,
} from "./view-model.js";

test("presents a compact risk, readiness, and people decision brief", () => {
  const got = presentDecisionBrief({
    pullRequest: {
      reviewState: "review_requested",
      readiness: {
        requestedReviewers: ["alice"],
        requestedTeams: ["maintainers"],
        mergeable: false,
        mergeableState: "dirty",
        collectedAt: "2026-09-12T11:00:00Z",
        checks: { total: 3, runs: [
          { name: "unit", status: "completed", conclusion: "success" },
          { name: "lint", status: "completed", conclusion: "failure" },
          { name: "integration", status: "in_progress", conclusion: "" },
        ] },
      },
      contribution: {
        completeness: "complete",
        level: "some_history",
        association: "CONTRIBUTOR",
        counts: { merged: 1, closedUnmerged: 2, open: 1 },
      },
    },
    analysis: {
      status: "complete",
      analysisStatus: "available",
      reviewCognitiveLoad: {
        overall: "high",
        reviewRisk: {
          level: "high", reason: "Touches authentication boundaries.",
          sourceIDs: ["file:auth.go"],
        },
      },
      waitingOn: [{ party: "maintainer", reason: "Review requested.", sourceIDs: ["review-1"] }],
    },
    quality: { level: "review_suggested", findings: [{ summary: "Missing tests." }] },
  }, Date.parse("2026-09-12T12:00:00Z"));

  assert.deepEqual(got.risk, {
    available: true, level: "high", label: "High review risk",
    reason: "Touches authentication boundaries.", sourceIDs: ["file:auth.go"],
    cognitiveLabel: "High review cognitive load",
  });
  assert.equal(got.quality.label, "Review suggested");
  assert.deepEqual(got.readiness.map(({ state, label }) => ({ state, label })), [
    { state: "clear", label: "Not a draft" },
    { state: "attention", label: "Review requested" },
    { state: "blocked", label: "1 failed · 1 pending · 1 passed" },
    { state: "blocked", label: "Merge conflicts detected" },
  ]);
  assert.equal(got.freshness, "Updated 1h ago");
  assert.deepEqual(got.people.requestedReviewers, ["alice"]);
  assert.equal(got.people.contribution.evidence, "CONTRIBUTOR · 1 merged · 2 closed unmerged · 1 open");
});

test("keeps unavailable decision signals visible and exposes unusual activity facts", () => {
  const got = presentDecisionBrief({
    pullRequest: {
      reviewState: "none",
      readiness: { requestedReviewers: [], requestedTeams: [], checks: { total: 0, runs: [] } },
      contribution: {
        completeness: "complete",
        level: "new_to_collection",
        association: "FIRST_TIME_CONTRIBUTOR",
        counts: { merged: 0, closedUnmerged: 0, open: 1 },
        unusualActivity: {
          detected: true, experimental: true, accountAgeDays: 20,
          repositories: 24, organizations: 4, windowDays: 14,
        },
        policy: { unusualActivity: {
          accountAgeDays: { value: 90 }, minRepositories: { value: 20 },
          minOrganizations: { value: 3 }, windowDays: { value: 14 },
        } },
      },
    },
    analysis: { status: "failed", error: "Provider unavailable.", waitingOn: [] },
    quality: { level: "no_concerns", findings: [] },
  });

  assert.equal(got.risk.available, false);
  assert.equal(got.risk.label, "Review risk unknown");
  assert.equal(got.readiness[2].label, "No check runs");
  assert.equal(got.readiness[3].label, "Merge conflicts unknown");
  assert.match(got.people.unusual.detail, /20-day-old account.*24 repositories.*4 organizations/);
  assert.match(got.people.unusual.thresholds, /under 90 days.*at least 20 repositories/);
  assert.equal(got.freshness, "Updated time unknown");
});

test("humanizes elapsed time using compact age units", () => {
  const now = Date.parse("2026-09-12T12:00:00Z");
  assert.equal(humanizeAge("2026-09-12T11:59:30Z", now), "1min");
  assert.equal(humanizeAge("2026-09-12T10:00:00Z", now), "2h");
  assert.equal(humanizeAge("2026-09-09T12:00:00Z", now), "3d");
  assert.equal(humanizeAge("2026-08-29T12:00:00Z", now), "2w");
  assert.equal(humanizeAge("2026-06-12T12:00:00Z", now), "3m");
  assert.equal(humanizeAge("2024-09-12T12:00:00Z", now), "2y");
  assert.equal(humanizeAge("not-a-date", now), "Unknown");
});

test("presents cron expressions with a compact time until the next run", () => {
  const now = Date.parse("2026-09-12T12:00:00Z");
  assert.equal(humanizeUntil("2026-09-12T12:45:00Z", now), "45min");
  assert.equal(humanizeUntil("2026-09-12T16:00:00Z", now), "4h");
  assert.equal(humanizeUntil("2026-09-15T12:00:00Z", now), "3d");
  assert.deepEqual(presentSchedules([{
    operation: "feature_correlation",
    collection: "acme",
    expression: "0 4 * * *",
    enabled: true,
    nextRun: "2026-09-13T04:00:00Z",
  }], now), [{
    name: "Feature correlation · acme",
    expression: "0 4 * * *",
    nextRun: "2026-09-13T04:00:00Z",
    nextLabel: "16h",
  }]);
});

test("humanizes storage bytes with binary units", () => {
  assert.equal(humanizeBytes(929792), "908 KiB");
  assert.equal(humanizeBytes(1536), "1.5 KiB");
  assert.equal(humanizeBytes(0), "0 bytes");
});

test("distinguishes disabled analysis from an empty healthy queue", () => {
  assert.deepEqual(presentAnalysisOperations({
    analysisConfiguration: { configuredProviders: 0, enabledCollections: 0 },
    analysisJobs: { queued: 0, running: 0, stale: 0, failures: 0 },
  }), {
    state: "disabled",
    details: "No model provider is configured or assigned to a collection.",
  });

  const configured = presentAnalysisOperations({
    analysisConfiguration: { configuredProviders: 1, enabledCollections: 1 },
    analysisJobs: { queued: 0, running: 0, stale: 0, failures: 0 },
  });
  assert.equal(configured.state, "healthy");
  assert.match(configured.details, /queued after collection detects changed inputs/);
});

test("presents review cognitive load, simultaneous waiting states, and provenance", () => {
  const got = presentAnalysis({
    status: "complete",
    analysisStatus: "available",
    reviewCognitiveLoad: {
      overall: "medium",
      changeScope: {
        level: "medium",
        reason: "Several files change together.",
        sourceIDs: ["PR_acme_widgets_7"],
      },
      requiredContext: {
        level: "medium",
        reason: "Two APIs interact.",
        sourceIDs: ["PR_acme_widgets_7"],
      },
      conceptualComplexity: {
        level: "low",
        reason: "Localized.",
        sourceIDs: ["file:main.go"],
      },
      reviewRisk: {
        level: "low",
        reason: "Low risk.",
        sourceIDs: ["PR_acme_widgets_7"],
      },
    },
    waitingOn: [
      { party: "author", reason: "Changes requested.", sourceIDs: ["review-1"] },
      { party: "maintainer", reason: "Review requested.", sourceIDs: ["review-2"] },
    ],
    provider: "fallback",
    fallbackFrom: "primary",
    model: "test-model",
    analyzedAt: "2026-09-10T12:00:00Z",
  });

  assert.equal(got.available, true);
  assert.equal(got.level, "medium");
  assert.equal(got.label, "Medium");
  assert.deepEqual(got.waitingLabels, ["Author", "Maintainer"]);
  assert.equal(got.components.length, 4);
  assert.match(got.provenance, /fallback \(fallback after primary\) · test-model/);
});

test("keeps factual views useful when model analysis is unavailable", () => {
  assert.deepEqual(presentAnalysis({
    status: "failed",
    analysisStatus: "failed",
    error: "Model provider unavailable; factual pull request data remains available.",
    waitingOn: [],
  }), {
    available: false,
    status: "failed",
    analysisStatus: "failed",
    level: "unknown",
    label: "—",
    message: "Model provider unavailable; factual pull request data remains available.",
    waitingLabels: [],
    waiting: [],
    components: [],
    provenance: "",
    rationale: "",
    sourceIDs: [],
    completeness: "",
    partialReason: "",
    attemptHealth: "Failed",
    explanation: "Model provider unavailable; factual pull request data remains available.",
  });
});

test("keeps last valid load and explains stale partial and diff health", () => {
  const got = presentAnalysis({
    status: "partial",
    analysisStatus: "stale",
    reviewCognitiveLoad: {
      overall: "low",
      rationale: "The change is localized.",
      evidenceCompleteness: "partial",
      partialReason: "One file was truncated.",
      changeScope: { level: "low", reason: "One package.", sourceIDs: ["diff:a.go"] },
      requiredContext: { level: "unknown", reason: "Context missing.", sourceIDs: [] },
      conceptualComplexity: { level: "low", reason: "Direct logic.", sourceIDs: ["diff:a.go"] },
      reviewRisk: { level: "medium", reason: "Touches auth.", sourceIDs: ["diff:a.go"] },
    },
    diffCompleteness: "partial",
    diffTruncated: true,
    error: "Latest analysis returned partial evidence.",
    waitingOn: [],
  });

  assert.equal(got.available, true);
  assert.equal(got.label, "Low");
  assert.equal(got.components.length, 4);
  assert.equal(got.components[1].label, "Unknown");
  assert.equal(got.rationale, "The change is localized.");
  assert.match(got.explanation, /stale/i);
  assert.match(got.explanation, /partial evidence/i);
  assert.match(got.explanation, /truncated/i);
});

test("explains stale non-load sections and partial response validation accurately", () => {
  const stale = presentAnalysis({
    status: "complete",
    analysisStatus: "stale",
    waitingOn: [{ party: "author", reason: "Updates requested.", sourceIDs: [] }],
  });
  assert.equal(stale.available, false);
  assert.match(stale.explanation, /retained analysis sections/i);
  assert.match(stale.explanation, /Review Cognitive Load is not available/i);

  const partial = presentAnalysis({
    status: "partial",
    analysisStatus: "partial",
    waitingOn: [],
  });
  assert.match(partial.explanation, /response sections were invalid/i);
  assert.doesNotMatch(partial.explanation, /used partial evidence/i);
});

test("presents collection contribution context without changing quality", () => {
  assert.deepEqual(presentContribution({
    completeness: "complete",
    level: "some_history",
    counts: { merged: 1, closedUnmerged: 2, open: 1 },
    association: "CONTRIBUTOR",
    unusualActivity: {
      detected: true,
      experimental: true,
      repositories: 20,
      organizations: 3,
      windowDays: 14,
    },
  }), {
    available: true,
    level: "some_history",
    label: "Some history",
    evidence: "CONTRIBUTOR · 1 merged · 2 closed unmerged · 1 open",
    unusual: "Experimental unusual activity: 20 repositories across 3 organizations in 14 days",
  });

  assert.deepEqual(presentContribution({ completeness: "incomplete" }), {
    available: false,
    label: "History incomplete",
  });
});

test("presents persisted pull request data for the public table", () => {
  const got = presentPullRequest({
    repository: "prometheus/node_exporter",
    number: 123,
    title: "Make collector behavior explicit",
    author: "octocat",
    additions: 42,
    deletions: 7,
    changedFiles: 3,
    reviewState: "approved",
    updatedAt: "2026-09-08T15:00:00Z",
    url: "https://github.com/prometheus/node_exporter/pull/123",
  });

  assert.deepEqual(got, {
    number: "#123",
    repository: "prometheus/node_exporter",
    title: "Make collector behavior explicit",
    author: "octocat",
    size: "+42/−7",
    changedFiles: "3 changed files",
    reviewState: "Approved",
    updatedAt: "2026-09-08T15:00:00Z",
    githubURL: "https://github.com/prometheus/node_exporter/pull/123",
  });
});

test("presents raw size without classification metadata", () => {
  const got = presentPullRequest({
    repository: "acme/widgets",
    number: 7,
    title: "Generated output",
    additions: 100,
    deletions: 4,
    updatedAt: "2026-09-08T15:00:00Z",
    url: "https://github.com/acme/widgets/pull/7",
  });

  assert.equal(got.size, "+100/−4");
});

test("presents additions and deletions separately for diff-like colors", () => {
  assert.deepEqual(presentChurn(12, 3), {
    additions: "+12",
    deletions: "−3",
    label: "12 additions and 3 deletions",
  });
});

test("labels relationship context completeness explicitly", () => {
  assert.deepEqual(presentRelationshipContext({
    completeness: "complete",
    collectedAt: "2026-09-12T12:00:00Z",
  }), {
    state: "complete",
    label: "Complete",
    message: "GitHub relationship collection completed.",
    collectedAt: "2026-09-12T12:00:00Z",
    warning: false,
  });
  assert.equal(presentRelationshipContext({ completeness: "partial" }).label, "Partial");
  assert.equal(presentRelationshipContext({ completeness: "unknown" }).label, "Unknown");
});

test("presents incomplete refreshes without hiding the last complete snapshot", () => {
  assert.deepEqual(presentRefresh({
    refreshState: "incomplete",
    lastSuccessfulRefresh: "2026-09-08T15:00:00Z",
    refreshError: "detail request failed",
  }), {
    state: "incomplete",
    label: "Incomplete refresh",
    message: "Showing the last complete snapshot. detail request failed",
    timestamp: "2026-09-08T15:00:00Z",
    warning: true,
    limited: false,
  });
});

test("presents provider limits separately from refresh health", () => {
  assert.deepEqual(presentRefresh({
    refreshState: "fresh",
    completeness: [{
      repository: "acme/widgets",
      scope: "discovery_search",
      collected: 1000,
      total: 1200,
    }],
  }), {
    state: "fresh",
    label: "Current",
    message: "Limited GitHub context: acme/widgets Discovery search: 1000 of 1200 collected.",
    timestamp: null,
    warning: true,
    limited: true,
  });
});

test("groups sanitized route warnings by repository with maintainer guidance", () => {
  const got = presentRouteWarnings([
    {
      repository: "acme/widgets",
      operation: "discovery",
      status: "fallback",
      freshness: "fresh",
      lastAttempt: "2026-09-11T12:00:00Z",
      profile: "must-not-appear",
      failureCategory: "credential_invalid",
    },
    {
      repository: "acme/widgets",
      operation: "contribution_evidence",
      status: "pending",
      freshness: "stale",
      lastSuccessfulRefresh: "2026-09-10T12:00:00Z",
    },
    {
      repository: "acme/gadgets",
      operation: "discovery",
      status: "failed",
      freshness: "failed",
      lastAttempt: "2026-09-11T13:00:00Z",
    },
  ]);

  assert.equal(got.visible, true);
  assert.deepEqual(got.disclosure, {
    element: "details",
    alertRole: "alert",
    open: false,
    dismissible: false,
  });
  assert.equal(got.summary, "GitHub access needs attention for 2 repositories");
  assert.match(got.guidance, /contact your deployment Admin/i);
  assert.equal(got.repositories.length, 2);
  assert.deepEqual(got.repositories[0].operations.map((operation) => operation.label), [
    "Pull request discovery",
    "Contributor history",
  ]);
  assert.match(got.repositories[0].operations[0].message, /fallback GitHub connection/i);
  assert.match(got.repositories[0].operations[0].statusLine, /Current data.*Last attempt/i);
  assert.match(got.repositories[0].operations[1].message, /waiting for its first GitHub access attempt/i);
  assert.match(got.repositories[1].operations[0].message, /could not access GitHub/i);
  assert.doesNotMatch(JSON.stringify(got), /must-not-appear|credential_invalid/);
  assert.deepEqual(presentRouteWarnings([]), {
    visible: false,
    disclosure: {
      element: "details",
      alertRole: "alert",
      open: false,
      dismissible: false,
    },
    summary: "",
    guidance: "",
    repositories: [],
  });
});

test("presents ordered administrator GitHub route attempts", () => {
  const got = presentGitHubRoutes([{
    repository: "acme/widgets",
    operation: "discovery",
    pending: false,
    attempts: [
      {
        profile: "<primary>",
        profileType: "github_app",
        priority: 0,
        selected: false,
        outcome: "advanced",
        failureCategory: "permission_missing",
        detail: "<credential detail>",
        attemptedAt: "2026-09-11T12:00:00Z",
        remediation: "Grant read access.",
      },
      {
        profile: "fallback",
        profileType: "fine_grained_pat",
        priority: 1,
        selected: true,
        outcome: "selected",
        attemptedAt: "2026-09-11T12:00:01Z",
      },
    ],
  }]);

  assert.equal(got[0].operationLabel, "Pull request discovery");
  assert.deepEqual(got[0].attempts.map((attempt) => attempt.priority), [0, 1]);
  assert.equal(got[0].attempts[0].profile, "<primary>");
  assert.equal(got[0].attempts[0].profileTypeLabel, "GitHub App");
  assert.equal(got[0].attempts[0].outcomeLabel, "Advanced");
  assert.equal(got[0].attempts[0].failureLabel, "Permission missing");
  assert.equal(got[0].attempts[0].detail, "<credential detail>");
  assert.equal(got[0].attempts[1].selectedLabel, "Selected");
});

test("presents only http external URLs as explicitly untrusted", () => {
  assert.deepEqual(presentExternalURL({
    url: "https://example.com/design",
    sourceKind: "pull_request",
    sourceId: "acme/widgets#7",
  }), {
    href: "https://example.com/design",
    label: "https://example.com/design",
    source: "pull request · acme/widgets#7",
    untrusted: true,
  });
  assert.equal(presentExternalURL({ url: "javascript:alert(1)" }), null);
});

test("presents the three quality levels without a numeric score", () => {
  assert.deepEqual(presentQuality({ level: "no_concerns", findings: 0 }), {
    level: "no_concerns",
    label: "No concerns detected",
    findings: 0,
    detail: "",
  });
  assert.deepEqual(presentQuality({ level: "review_suggested", findings: 1 }), {
    level: "review_suggested",
    label: "Review suggested",
    findings: 1,
    detail: "1 finding",
  });
  assert.deepEqual(presentQuality({ level: "strong_concerns", findings: 2 }), {
    level: "strong_concerns",
    label: "Strong concerns",
    findings: 2,
    detail: "2 findings",
  });
  assert.equal(presentQuality(undefined).label, "No concerns detected");
});

test("presents a finding with provenance, completeness, and safe evidence links", () => {
  const got = presentFinding({
    rule: "description_diff_mismatch",
    severity: "high",
    provenance: "rule_derived",
    summary: "The description references 3 file(s); none of them appear in the diff.",
    completeness: "partial",
    evidence: [
      {
        kind: "description_reference",
        detail: "The description references cmd/serve/main.go, which is not part of the diff.",
        url: "https://github.com/acme/widgets/pull/7",
      },
      { kind: "policy_threshold", detail: "Policy flags at least 10 files." },
      { kind: "external", detail: "unsafe", url: "javascript:alert(1)" },
    ],
  });

  assert.equal(got.rule, "Description diff mismatch");
  assert.equal(got.severity, "High");
  assert.equal(got.provenance, "Rule derived");
  assert.equal(got.partial, true);
  assert.equal(got.confidence, "");
  assert.deepEqual(got.sourceIDs, []);
  assert.equal(got.evidence.length, 3);
  assert.equal(got.evidence[0].href, "https://github.com/acme/widgets/pull/7");
  assert.equal(got.evidence[1].href, null);
  assert.equal(got.evidence[2].href, null);
});

test("presents model finding confidence and source identities", () => {
  const got = presentFinding({
    rule: "missing_expected_tests",
    severity: "medium",
    provenance: "model_inferred",
    summary: "Expected tests are absent.",
    completeness: "complete",
    confidence: "high",
    sourceIDs: ["PR_acme_widgets_7", "file:main.go"],
  });

  assert.equal(got.rule, "Missing expected tests");
  assert.equal(got.provenance, "Model inferred");
  assert.equal(got.confidence, "High");
  assert.deepEqual(got.sourceIDs, ["PR_acme_widgets_7", "file:main.go"]);
});

test("presents quality policy thresholds and identifies local values", () => {
  const rows = presentQualityPolicy({
    descriptionDiffMismatch: {
      minReferences: { value: 1, source: "default" },
      strongMismatchMinReferences: { value: 3, source: "default" },
    },
    broadAdditionsOnly: {
      minFiles: { value: 25, source: "local" },
      minAdditions: { value: 400, source: "default" },
      maxDeletions: { value: 10, source: "default" },
    },
  });

  assert.equal(rows.length, 5);
  const minFiles = rows.find((row) => row.name === "Minimum files");
  assert.deepEqual(minFiles, {
    rule: "Broad additions only",
    name: "Minimum files",
    value: 25,
    local: true,
  });
  assert.equal(rows.filter((row) => row.local).length, 1);
});

test("does not present an untrusted pull request URL as a link", () => {
  const got = presentPullRequest({
    repository: "prometheus/node_exporter",
    number: 123,
    title: "Unsafe URL",
    additions: 0,
    deletions: 0,
    updatedAt: "2026-09-08T15:00:00Z",
    url: "javascript:alert(1)",
  });

  assert.equal(got.githubURL, null);
});
