import test from "node:test";
import assert from "node:assert/strict";

import { advanceSorting, parseSorting, writeSorting } from "./sorting.js";

test("cycles a column from ascending to descending to removed", () => {
  let state = { sorts: [], customized: true };

  state = advanceSorting(state, "author");
  assert.deepEqual(state.sorts, [{ key: "author", order: "asc" }]);

  state = advanceSorting(state, "author");
  assert.deepEqual(state.sorts, [{ key: "author", order: "desc" }]);

  state = advanceSorting(state, "author");
  assert.deepEqual(state.sorts, []);
});

test("clicking several columns retains click order as sort priority", () => {
  let state = { sorts: [], customized: true };
  state = advanceSorting(state, "repository");
  state = advanceSorting(state, "updated");

  assert.deepEqual(state.sorts, [
    { key: "repository", order: "asc" },
    { key: "updated", order: "asc" },
  ]);
});

test("the first custom column replaces the implicit updated sort", () => {
  const state = advanceSorting({
    sorts: [{ key: "updated", order: "desc" }],
    customized: false,
  }, "title");

  assert.deepEqual(state.sorts, [{ key: "title", order: "asc" }]);
});

test("round trips multi-column sorting through URL parameters", () => {
  const query = new URLSearchParams("sort=repository%2Cupdated&order=asc%2Cdesc");
  const state = parseSorting(query);
  assert.deepEqual(state, {
    sorts: [
      { key: "repository", order: "asc" },
      { key: "updated", order: "desc" },
    ],
    customized: true,
  });

  const next = new URLSearchParams();
  writeSorting(next, state);
  assert.equal(next.toString(), "sort=repository%2Cupdated&order=asc%2Cdesc");
});

test("accepts quality as a sortable column", () => {
  const query = new URLSearchParams("sort=quality&order=desc");
  assert.deepEqual(parseSorting(query), {
    sorts: [{ key: "quality", order: "desc" }],
    customized: true,
  });
});

test("accepts review load as a sortable column", () => {
  assert.deepEqual(
    parseSorting(new URLSearchParams("sort=review_load&order=desc")),
    {
      sorts: [{ key: "review_load", order: "desc" }],
      customized: true,
    },
  );
});

test("writes an explicit empty sort after every column is removed", () => {
  const query = new URLSearchParams("cursor=old");
  writeSorting(query, { sorts: [], customized: true });

  assert.equal(query.get("sort"), "none");
  assert.equal(query.has("order"), false);
  assert.equal(query.has("cursor"), false);
});
