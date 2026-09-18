export function safeGitHubURL(raw) {
  try {
    const url = new URL(raw);
    if (url.protocol !== "https:" || url.hostname !== "github.com") {
      return null;
    }
    return url.toString();
  } catch {
    return null;
  }
}

export function humanizeBytes(bytes) {
  if (!Number.isFinite(bytes) || bytes <= 0) {
    return "0 bytes";
  }
  if (bytes < 1024) {
    return `${Math.round(bytes)} ${Math.round(bytes) === 1 ? "byte" : "bytes"}`;
  }
  const units = ["KiB", "MiB", "GiB", "TiB"];
  let value = bytes;
  let unit = -1;
  do {
    value /= 1024;
    unit++;
  } while (value >= 1024 && unit < units.length - 1);
  const precision = value < 10 && !Number.isInteger(value) ? 1 : 0;
  return `${value.toFixed(precision)} ${units[unit]}`;
}

export function humanizeAge(timestamp, now = Date.now()) {
  const occurredAt = new Date(timestamp).getTime();
  if (!Number.isFinite(occurredAt) || !Number.isFinite(now)) {
    return "Unknown";
  }
  const minutes = Math.max(1, Math.floor(Math.max(0, now - occurredAt) / 60000));
  const units = [
    [365 * 24 * 60, "y"],
    [30 * 24 * 60, "m"],
    [7 * 24 * 60, "w"],
    [24 * 60, "d"],
    [60, "h"],
  ];
  for (const [size, suffix] of units) {
    if (minutes >= size) {
      return `${Math.floor(minutes / size)}${suffix}`;
    }
  }
  return `${minutes}min`;
}

export function humanizeUntil(timestamp, now = Date.now()) {
  const occursAt = new Date(timestamp).getTime();
  if (!Number.isFinite(occursAt) || !Number.isFinite(now)) {
    return "Unknown";
  }
  const minutes = Math.max(0, Math.ceil((occursAt - now) / 60000));
  if (minutes === 0) {
    return "now";
  }
  for (const [size, suffix] of [[24 * 60, "d"], [60, "h"]]) {
    if (minutes >= size) {
      return `${Math.floor(minutes / size)}${suffix}`;
    }
  }
  return `${minutes}min`;
}

export function presentSchedules(schedules, now = Date.now()) {
  const names = {
    github_refresh: "GitHub refresh",
    github_contribution: "GitHub contribution",
    feature_correlation: "Feature correlation",
  };
  return (schedules || []).filter((schedule) => schedule.enabled).map((schedule) => ({
    name: `${names[schedule.operation] || label(schedule.operation || "schedule")}` +
      `${schedule.collection ? ` · ${schedule.collection}` : ""}`,
    expression: schedule.expression,
    nextRun: schedule.nextRun,
    nextLabel: humanizeUntil(schedule.nextRun, now),
  }));
}

export function presentAnalysisOperations(status) {
  const configuration = status.analysisConfiguration;
  const jobs = status.analysisJobs;
  if (configuration.enabledCollections === 0) {
    return {
      state: "disabled",
      details: configuration.configuredProviders > 0
        ? `${configuration.configuredProviders} model provider profiles are configured, but none is assigned to a collection.`
        : "No model provider is configured or assigned to a collection.",
    };
  }
  const oldest = jobs.oldestQueuedAt
    ? new Date(jobs.oldestQueuedAt).toLocaleString()
    : "none";
  return {
    state: jobs.failures ? "degraded" : "healthy",
    details: `${jobs.queued} queued, ${jobs.running} running, ${jobs.stale} stale, ${jobs.failures} failures; oldest ${oldest}. Analysis is queued after collection detects changed inputs; it has no separate schedule.`,
  };
}

export function presentExternalURL(item) {
  try {
    const url = new URL(item.url);
    if (!["http:", "https:"].includes(url.protocol)) {
      return null;
    }
    return {
      href: url.toString(),
      label: item.url,
      source: `${label(item.sourceKind || "unknown")} · ${item.sourceId || "unknown source"}`.toLowerCase(),
      untrusted: true,
    };
  } catch {
    return null;
  }
}

export function presentChurn(additions, deletions) {
  return {
    additions: `+${additions}`,
    deletions: `−${deletions}`,
    label: `${additions} additions and ${deletions} deletions`,
  };
}

export function presentRelationshipContext(context) {
  const state = ["complete", "partial"].includes(context?.completeness)
    ? context.completeness
    : "unknown";
  const messages = {
    complete: "GitHub relationship collection completed.",
    partial: "Some GitHub relationship evidence may be missing.",
    unknown: "GitHub relationship collection has not completed.",
  };
  return {
    state,
    label: label(state),
    message: messages[state],
    collectedAt: context?.collectedAt || null,
    warning: state !== "complete",
  };
}

export function presentAnalysis(analysis) {
  const status = analysis?.status || "unknown";
  const analysisStatus = analysis?.analysisStatus || (
    status === "complete" ? "available" :
      status === "partial" ? "partial" :
        status === "failed" ? "failed" :
          status === "unknown" ? "invalid" : "pending"
  );
  const cognitive = analysis?.reviewCognitiveLoad;
  const available = Boolean(cognitive && ["low", "medium", "high"].includes(cognitive.overall));
  const waiting = analysis?.waitingOn || [];
  const componentNames = [
    ["Change scope", "changeScope"],
    ["Required context", "requiredContext"],
    ["Conceptual complexity", "conceptualComplexity"],
    ["Review risk", "reviewRisk"],
  ];
  const components = cognitive
    ? componentNames.map(([name, key]) => ({
        name,
        level: cognitive[key]?.level || "unknown",
        label: label(cognitive[key]?.level || "unknown"),
        reason: cognitive[key]?.reason || "No explanation was provided.",
        sourceIDs: cognitive[key]?.sourceIDs || [],
        completeness: cognitive[key]?.evidenceCompleteness || "",
        partialReason: cognitive[key]?.partialReason || "",
      }))
    : [];
  const provenance = analysis?.provider
    ? `${analysis.provider}${analysis.fallbackFrom ? ` (fallback after ${analysis.fallbackFrom})` : ""}` +
      `${analysis.model ? ` · ${analysis.model}` : ""}` +
      `${analysis.analyzedAt ? ` · ${new Date(analysis.analyzedAt).toLocaleString()}` : ""}`
    : "";
  const explanations = [];
  if (analysisStatus === "pending") {
    explanations.push("Review cognitive load analysis is pending.");
  } else if (analysisStatus === "stale") {
    explanations.push(available
      ? "Review cognitive load is stale; showing the last valid value while updated analysis is pending."
      : "Retained analysis sections are stale while updated analysis is pending. Review Cognitive Load is not available.");
  } else if (analysisStatus === "partial") {
    explanations.push("Some latest model response sections were invalid; valid response sections were retained.");
  } else if (analysisStatus === "failed") {
    explanations.push(analysis?.error || "The provider or analysis input build failed.");
  } else if (analysisStatus === "invalid") {
    explanations.push(analysis?.error || "The model response was invalid.");
  }
  if (cognitive?.evidenceCompleteness === "partial") {
    explanations.push(
      `Review cognitive load is based on partial evidence${cognitive.partialReason
        ? `: ${cognitive.partialReason}`
        : "."}`,
    );
  }
  if (analysis?.diffCompleteness === "unavailable") {
    explanations.push("Diff evidence was unavailable.");
  } else if (analysis?.diffCompleteness === "partial" || analysis?.diffTruncated) {
    explanations.push("Diff evidence was truncated or incomplete.");
  }
  return {
    available: Boolean(available),
    status,
    analysisStatus,
    level: available ? cognitive.overall : "unknown",
    label: available ? label(cognitive.overall) : "—",
    message: available ? "" : analysis?.error || "Model analysis is not available.",
    waitingLabels: waiting.map((item) => label(item.party || "unknown")),
    waiting: waiting.map((item) => ({
      party: label(item.party || "unknown"),
      reason: item.reason || "",
      sourceIDs: item.sourceIDs || [],
    })),
    components,
    provenance,
    rationale: cognitive?.rationale || "",
    sourceIDs: cognitive?.sourceIDs || [],
    completeness: cognitive?.evidenceCompleteness || "",
    partialReason: cognitive?.partialReason || "",
    attemptHealth: label(status),
    explanation: [...new Set(explanations)].join(" "),
  };
}

export function presentDecisionBrief(detail, now = Date.now()) {
  const pullRequest = detail?.pullRequest || {};
  const analysis = detail?.analysis || {};
  const cognitive = analysis.reviewCognitiveLoad;
  const reviewRisk = cognitive?.reviewRisk;
  const riskAvailable = Boolean(reviewRisk);
  const risk = {
    available: riskAvailable,
    level: riskAvailable ? reviewRisk.level || "unknown" : "unknown",
    label: riskAvailable ? `${label(reviewRisk.level || "unknown")} review risk` : "Review risk unknown",
    reason: riskAvailable
      ? reviewRisk.reason || "No explanation was provided."
      : analysis.error || "Model analysis is not available.",
    sourceIDs: riskAvailable ? reviewRisk.sourceIDs || [] : [],
    cognitiveLabel: cognitive
      ? `${label(cognitive.overall || "unknown")} review cognitive load`
      : "Review cognitive load unknown",
  };

  const findingCount = Array.isArray(detail?.quality?.findings)
    ? detail.quality.findings.length
    : Number(detail?.quality?.findings || 0);
  const quality = presentQuality({
    level: detail?.quality?.level,
    findings: findingCount,
  });

  const readiness = detail?.readiness || pullRequest.readiness || {};
  const reviewState = pullRequest.reviewState || "unknown";
  const checks = readiness.checks || {};
  const runs = checks.runs || [];
  const failedConclusions = new Set([
    "failure", "cancelled", "timed_out", "action_required", "startup_failure", "stale",
  ]);
  const passedConclusions = new Set(["success", "neutral", "skipped"]);
  const failed = runs.filter((run) => failedConclusions.has(run.conclusion)).length;
  const passed = runs.filter((run) => passedConclusions.has(run.conclusion)).length;
  const pending = runs.filter((run) => run.status !== "completed" || !run.conclusion).length;
  const checksLabel = Number(checks.total) === 0
    ? "No check runs"
    : `${failed} failed · ${pending} pending · ${passed} passed`;
  const reviewLabels = {
    draft: "Draft",
    review_requested: "Review requested",
    changes_requested: "Changes requested",
    approved: "Approved",
    reviewed: "Reviewed",
    none: "No review",
    unknown: "Review state unknown",
  };
  const mergeableKnown = typeof readiness.mergeable === "boolean";
  const conflicts = readiness.mergeable === false || readiness.mergeableState === "dirty";
  const readinessSignals = [
    {
      key: "draft",
      state: reviewState === "unknown" ? "unknown" : reviewState === "draft" ? "attention" : "clear",
      label: reviewState === "unknown" ? "Draft state unknown" :
        reviewState === "draft" ? "Draft" : "Not a draft",
    },
    {
      key: "review",
      state: reviewState === "changes_requested" ? "blocked" :
        reviewState === "review_requested" || reviewState === "none" ? "attention" :
          reviewState === "unknown" ? "unknown" : "clear",
      label: reviewLabels[reviewState] || label(reviewState),
    },
    {
      key: "checks",
      state: failed ? "blocked" : pending ? "attention" :
        Number(checks.total) === 0 ? "neutral" : "clear",
      label: checksLabel,
    },
    {
      key: "conflicts",
      state: conflicts ? "blocked" : mergeableKnown ? "clear" : "unknown",
      label: conflicts ? "Merge conflicts detected" :
        mergeableKnown ? "No merge conflicts" : "Merge conflicts unknown",
    },
  ];

  const contribution = presentContribution(pullRequest.contribution);
  const unusual = pullRequest.contribution?.unusualActivity;
  const unusualPolicy = pullRequest.contribution?.policy?.unusualActivity;
  const unusualPresentation = unusual?.detected ? {
    detail: `${unusual.accountAgeDays}-day-old account with activity in ` +
      `${unusual.repositories} repositories across ${unusual.organizations} organizations ` +
      `within ${unusual.windowDays} days.`,
    thresholds: unusualPolicy
      ? `Triggers for accounts under ${unusualPolicy.accountAgeDays.value} days with activity in ` +
        `at least ${unusualPolicy.minRepositories.value} repositories across at least ` +
        `${unusualPolicy.minOrganizations.value} organizations within ` +
        `${unusualPolicy.windowDays.value} days.`
      : "Collection policy thresholds unavailable.",
  } : null;

  const collectedAt = new Date(readiness.collectedAt).getTime();
  return {
    risk,
    quality,
    readiness: readinessSignals,
    freshness: Number.isFinite(collectedAt) && collectedAt >= 0
      ? `Updated ${humanizeAge(readiness.collectedAt, now)} ago`
      : "Updated time unknown",
    people: {
      waiting: (analysis.waitingOn || []).map((item) => ({
        party: label(item.party || "unknown"),
        reason: item.reason || "",
        sourceIDs: item.sourceIDs || [],
      })),
      requestedReviewers: readiness.requestedReviewers || [],
      requestedTeams: readiness.requestedTeams || [],
      contribution,
      unusual: unusualPresentation,
    },
    checks: { ...checks, runs },
  };
}

export function presentContribution(contribution) {
  if (contribution?.completeness !== "complete") {
    return { available: false, label: "History incomplete" };
  }
  const counts = contribution.counts || {};
  const association = contribution.association || "NONE";
  const unusual = contribution.unusualActivity;
  return {
    available: true,
    level: contribution.level,
    label: label(contribution.level || "unknown"),
    evidence: `${association} · ${counts.merged || 0} merged · ` +
      `${counts.closedUnmerged || 0} closed unmerged · ${counts.open || 0} open`,
    unusual: unusual?.detected
      ? `Experimental unusual activity: ${unusual.repositories} repositories across ` +
        `${unusual.organizations} organizations in ${unusual.windowDays} days`
      : "",
  };
}

export function presentPullRequest(pr) {
  const size = `+${pr.additions}/−${pr.deletions}`;
  return {
    number: `#${pr.number}`,
    repository: pr.repository,
    title: pr.title,
    author: pr.author || "Unknown",
    size,
    changedFiles: Number.isInteger(pr.changedFiles)
      ? `${pr.changedFiles} changed files`
      : "Changed-file count unavailable",
    reviewState: label(pr.reviewState || "none"),
    updatedAt: pr.updatedAt,
    githubURL: safeGitHubURL(pr.url),
  };
}

const QUALITY_LABELS = {
  no_concerns: "No concerns detected",
  review_suggested: "Review suggested",
  strong_concerns: "Strong concerns",
};

export function presentQuality(quality) {
  const level = quality?.level || "no_concerns";
  const findings = Number.isInteger(quality?.findings) ? quality.findings : 0;
  return {
    level,
    label: QUALITY_LABELS[level] || label(level),
    findings,
    detail: findings ? `${findings} finding${findings === 1 ? "" : "s"}` : "",
  };
}

export function presentFinding(finding) {
  return {
    rule: label(finding.rule || "unknown"),
    severity: label(finding.severity || "unknown"),
    provenance: label(finding.provenance || "unknown"),
    summary: finding.summary || "",
    completeness: finding.completeness || "unknown",
    partial: finding.completeness !== "complete",
    confidence: finding.confidence ? label(finding.confidence) : "",
    sourceIDs: finding.sourceIDs || [],
    evidence: (finding.evidence || []).map((item) => ({
      detail: item.detail || "",
      href: item.url ? safeGitHubURL(item.url) : null,
    })),
  };
}

export function presentQualityPolicy(policy) {
  const rows = [];
  const add = (rule, name, threshold) => {
    if (threshold && Number.isInteger(threshold.value)) {
      rows.push({
        rule,
        name,
        value: threshold.value,
        local: threshold.source === "local",
      });
    }
  };
  const mismatch = policy?.descriptionDiffMismatch;
  add("Description diff mismatch", "Minimum references", mismatch?.minReferences);
  add(
    "Description diff mismatch",
    "Strong mismatch references",
    mismatch?.strongMismatchMinReferences,
  );
  const broad = policy?.broadAdditionsOnly;
  add("Broad additions only", "Minimum files", broad?.minFiles);
  add("Broad additions only", "Minimum additions", broad?.minAdditions);
  add("Broad additions only", "Maximum deletions", broad?.maxDeletions);
  return rows;
}

export function presentRefresh(collection) {
  const state = collection.refreshState || "never";
  const limited = (collection.completeness || []).length > 0;
  const warning = ["stale", "failed", "incomplete"].includes(state) || limited;
  const labels = {
    never: "Not refreshed",
    fresh: "Current",
    stale: "Refresh overdue",
    running: "Refreshing",
    failed: "Refresh failed",
    incomplete: "Incomplete refresh",
  };
  let message = "";
  if (warning && collection.lastSuccessfulRefresh) {
    message = "Showing the last complete snapshot.";
  }
  if (collection.refreshError) {
    message = `${message} ${collection.refreshError}`.trim();
  }
  if (limited) {
    const limits = collection.completeness.map((item) => {
      const count = item.total ? `${item.collected} of ${item.total}` : `${item.collected}`;
      return `${item.repository || "repository"} ${label(item.scope)}: ${count} collected`;
    });
    message = `${message} Limited GitHub context: ${limits.join("; ")}.`.trim();
  }
  return {
    state,
    label: labels[state] || label(state),
    message,
    timestamp: collection.lastSuccessfulRefresh || null,
    warning,
    limited,
  };
}

const ROUTE_OPERATION_LABELS = {
  discovery: "Pull request discovery",
  contribution_evidence: "Contributor history",
};

const ROUTE_STATUS_MESSAGES = {
  fallback: "Using a fallback GitHub connection.",
  pending: "Waiting for its first GitHub access attempt.",
  failed: "Could not access GitHub.",
};

const FRESHNESS_LABELS = {
  never: "No complete data yet",
  fresh: "Current data",
  stale: "Data may be out of date",
  running: "Refresh in progress",
  failed: "Last refresh failed",
  incomplete: "Last refresh was incomplete",
};

export function presentRouteWarnings(warnings) {
  if (!Array.isArray(warnings) || warnings.length === 0) {
    return {
      visible: false,
      disclosure: { element: "details", alertRole: "alert", open: false, dismissible: false },
      summary: "",
      guidance: "",
      repositories: [],
    };
  }
  const grouped = new Map();
  for (const warning of warnings) {
    const repository = warning.repository || "Repository";
    if (!grouped.has(repository)) {
      grouped.set(repository, []);
    }
    const statusParts = [FRESHNESS_LABELS[warning.freshness] || label(warning.freshness || "unknown")];
    if (warning.lastAttempt) {
      statusParts.push(`Last attempt ${new Date(warning.lastAttempt).toLocaleString()}`);
    } else if (warning.lastSuccessfulRefresh) {
      statusParts.push(`Last complete refresh ${new Date(warning.lastSuccessfulRefresh).toLocaleString()}`);
    } else {
      statusParts.push("No attempt time is available");
    }
    grouped.get(repository).push({
      label: ROUTE_OPERATION_LABELS[warning.operation] || label(warning.operation || "collection"),
      message: ROUTE_STATUS_MESSAGES[warning.status] || "GitHub access needs attention.",
      statusLine: `${statusParts.join(". ")}.`,
    });
  }
  const repositories = [...grouped.entries()].map(([repository, operations]) => ({
    repository,
    operations,
  }));
  return {
    visible: true,
    disclosure: { element: "details", alertRole: "alert", open: false, dismissible: false },
    summary: `GitHub access needs attention for ${repositories.length} ` +
      `${repositories.length === 1 ? "repository" : "repositories"}`,
    guidance: "Collection may be delayed or incomplete. Contact your deployment Admin for help.",
    repositories,
  };
}

export function presentGitHubRoutes(routes) {
  return (routes || []).map((route) => ({
    repository: route.repository || "Unknown repository",
    operationLabel: ROUTE_OPERATION_LABELS[route.operation] || label(route.operation || "unknown"),
    pending: Boolean(route.pending),
    lastAttempt: route.lastAttempt || null,
    attempts: [...(route.attempts || [])]
      .sort((left, right) => left.priority - right.priority)
      .map((attempt) => ({
        priority: attempt.priority,
        profile: attempt.profile || "Unnamed profile",
        profileTypeLabel: attempt.profileType === "github_app"
          ? "GitHub App"
          : attempt.profileType === "fine_grained_pat"
            ? "Fine-grained PAT"
            : label(attempt.profileType || "unknown"),
        selectedLabel: attempt.selected ? "Selected" : "Not selected",
        outcomeLabel: label(attempt.outcome || "unknown"),
        failureLabel: attempt.failureCategory ? label(attempt.failureCategory) : "None",
        attemptedAt: attempt.attemptedAt || null,
        detail: attempt.detail || "",
        remediation: attempt.remediation || "No remediation provided.",
      })),
  }));
}

function label(value) {
  const words = value.replaceAll("_", " ");
  return words.charAt(0).toUpperCase() + words.slice(1);
}
