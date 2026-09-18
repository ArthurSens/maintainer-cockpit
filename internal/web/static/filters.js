const FILTER_KEYS = [
  "q", "repo", "review", "quality", "review_load", "analysis_status", "waiting", "waiting_mode",
];
const OPTIONAL_FILTER_KEYS = [
  "repo", "review", "quality", "review_load", "analysis_status", "waiting",
];

export function activeFilterKeys(query) {
  return OPTIONAL_FILTER_KEYS.filter((key) => query.has(key) && query.get(key));
}

export function updateFilters(current, entries) {
  const next = new URLSearchParams(current);
  for (const key of FILTER_KEYS) {
    next.delete(key);
  }
  next.delete("cursor");
  const waiting = [];
  for (const [key, value] of entries) {
    if (!FILTER_KEYS.includes(key) || !value) {
      continue;
    }
    if (key === "waiting") {
      waiting.push(value);
    } else {
      next.set(key, value);
    }
  }
  if (waiting.length) {
    next.set("waiting", waiting.sort().join(","));
  } else {
    next.delete("waiting_mode");
  }
  return next;
}
