const collectionIDPattern = /^[a-z][a-z0-9-]{0,62}$/;

export function groupHiddenPullRequests(pullRequests) {
  return {
    snoozed: pullRequests.filter((pr) => pr.personal?.hidden?.kind === "snoozed"),
    ignored: pullRequests.filter((pr) => pr.personal?.hidden?.kind === "ignored"),
  };
}

export function hiddenPath(collectionID, pr) {
  const parts = pr.repository?.split("/");
  if (!collectionIDPattern.test(collectionID) ||
      parts?.length !== 2 ||
      !parts[0] ||
      !parts[1] ||
      !Number.isInteger(pr.number) ||
      pr.number <= 0) {
    throw new Error("invalid hidden pull request identity");
  }
  return `/api/collections/${encodeURIComponent(collectionID)}/pull-requests/` +
    `${encodeURIComponent(parts[0])}/${encodeURIComponent(parts[1])}/${pr.number}/hidden`;
}

export function hiddenChoiceRequest(values, now = new Date()) {
  if (values.choice === "forever") {
    return hiddenRequest({ kind: "ignored", reason: values.reason });
  }
  if (values.choice === "activity") {
    return hiddenRequest({
      kind: "snoozed",
      reason: values.reason,
      wakeOnActivity: true,
    });
  }
  const snoozedUntil = new Date(values.snoozedUntil);
  if (values.choice !== "period" ||
      Number.isNaN(snoozedUntil.getTime()) ||
      snoozedUntil <= now) {
    throw new Error("Choose a future date and time.");
  }
  return hiddenRequest({
    kind: "snoozed",
    reason: values.reason,
    snoozedUntil: snoozedUntil.toISOString(),
  });
}

export function hiddenRequest(values) {
  const kind = values.kind;
  const reason = (values.reason || "").trim();
  if (reason.length > 500) {
    throw new Error("Reason must not exceed 500 characters.");
  }
  if (kind === "ignored") {
    return { kind, reason, snoozedUntil: null, wakeOnActivity: false };
  }
  if (kind !== "snoozed") {
    throw new Error("Choose Snooze or Ignore.");
  }
  const wakeOnActivity = Boolean(values.wakeOnActivity);
  const snoozedUntil = values.snoozedUntil
    ? new Date(values.snoozedUntil).toISOString()
    : null;
  if (!snoozedUntil && !wakeOnActivity) {
    throw new Error("Choose a snooze date or next activity.");
  }
  return { kind, reason, snoozedUntil, wakeOnActivity };
}

function remainingDuration(until, now) {
  const milliseconds = new Date(until).getTime() - now.getTime();
  if (!Number.isFinite(milliseconds) || milliseconds <= 0) {
    return "Ending now";
  }
  const minutes = milliseconds / (60 * 1000);
  const label = (value, unit) => unit === "min"
    ? `${value} min remaining`
    : `${value} ${unit}${value === 1 ? "" : "s"} remaining`;
  if (minutes < 60) {
    return label(Math.ceil(minutes), "min");
  }
  const hours = minutes / 60;
  if (hours < 24) {
    return label(Math.ceil(hours), "hour");
  }
  const days = hours / 24;
  if (days < 14) {
    return label(Math.ceil(days), "day");
  }
  if (days < 60) {
    return label(Math.round(days / 7), "week");
  }
  if (days < 730) {
    return label(Math.round(days / 30.4375), "month");
  }
  return label(Math.round(days / 365.25), "year");
}

export function presentHidden(state, now = new Date()) {
  if (!state) {
    return {
      active: false,
      kind: "",
      label: "",
      reason: "",
      newActivity: false,
      condition: "",
    };
  }
  if (state.kind === "ignored") {
    return {
      active: true,
      kind: state.kind,
      label: "Ignored",
      reason: state.reason || "",
      newActivity: Boolean(state.newActivity),
      condition: "Until restored",
    };
  }
  const conditions = [];
  if (state.snoozedUntil) {
    conditions.push(remainingDuration(state.snoozedUntil, now));
  }
  if (state.wakeOnActivity) {
    conditions.push("until next activity");
  }
  return {
    active: true,
    kind: state.kind,
    label: "Snoozed",
    reason: state.reason || "",
    newActivity: false,
    condition: conditions.join(" or "),
  };
}
