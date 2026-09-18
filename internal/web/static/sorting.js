const SORT_KEYS = new Set([
  "number",
  "repository",
  "title",
  "author",
  "churn",
  "review",
  "quality",
  "review_load",
  "updated",
]);

export function advanceSorting(state, key) {
  const sorts = state.sorts.map((sort) => ({ ...sort }));
  const index = sorts.findIndex((sort) => sort.key === key);

  if (index === -1) {
    return {
      sorts: state.customized
        ? [...sorts, { key, order: "asc" }]
        : [{ key, order: "asc" }],
      customized: true,
    };
  }
  if (sorts[index].order === "asc") {
    sorts[index].order = "desc";
  } else {
    sorts.splice(index, 1);
  }
  return { sorts, customized: true };
}

export function parseSorting(query) {
  if (!query.has("sort")) {
    return {
      sorts: [{ key: "updated", order: "desc" }],
      customized: false,
    };
  }
  if (query.get("sort") === "none") {
    return { sorts: [], customized: true };
  }
  const keys = (query.get("sort") || "").split(",");
  const orders = (query.get("order") || "").split(",");
  const seen = new Set();
  const sorts = keys
    .map((key, index) => ({ key, order: orders[index] }))
    .filter((sort) => {
      if (!SORT_KEYS.has(sort.key) ||
          !["asc", "desc"].includes(sort.order) ||
          seen.has(sort.key)) {
        return false;
      }
      seen.add(sort.key);
      return true;
    });
  return sorts.length
    ? { sorts, customized: true }
    : { sorts: [{ key: "updated", order: "desc" }], customized: false };
}

export function writeSorting(query, state) {
  query.delete("cursor");
  if (!state.sorts.length) {
    query.set("sort", "none");
    query.delete("order");
    return;
  }
  query.set("sort", state.sorts.map((sort) => sort.key).join(","));
  query.set("order", state.sorts.map((sort) => sort.order).join(","));
}
