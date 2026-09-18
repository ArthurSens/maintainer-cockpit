import test from "node:test";
import assert from "node:assert/strict";

import { activeFilterKeys, updateFilters } from "./filters.js";

test("applies table filters while preserving sorting and clearing pagination", () => {
  const current = new URLSearchParams(
    "sort=updated&order=desc&cursor=old&q=old&repo=old%2Frepo",
  );
  const next = updateFilters(current, [
    ["q", "metrics"],
    ["repo", "ArthurSens/blog"],
    ["review", "none"],
    ["quality", "review_suggested"],
  ]);

  assert.equal(
    next.toString(),
    "sort=updated&order=desc&q=metrics&repo=ArthurSens%2Fblog&review=none&quality=review_suggested",
  );
});

test("empty controls remove their filters", () => {
  const current = new URLSearchParams(
    "q=old&repo=old%2Frepo&review=approved&quality=strong_concerns",
  );
  const next = updateFilters(current, [
    ["q", ""],
    ["repo", ""],
    ["review", ""],
    ["quality", ""],
  ]);

  assert.equal(next.toString(), "");
});

test("encodes simultaneous waiting-on filters with any or all semantics", () => {
  const next = updateFilters(new URLSearchParams(), [
    ["review_load", "medium"],
    ["analysis_status", "partial"],
    ["waiting", "maintainer"],
    ["waiting", "author"],
    ["waiting_mode", "all"],
  ]);

  assert.equal(
    next.toString(),
    "review_load=medium&analysis_status=partial&waiting_mode=all&waiting=author%2Cmaintainer",
  );
});

test("shows only optional filters that are applied in the URL", () => {
  const query = new URLSearchParams(
    "q=metrics&repo=ArthurSens%2Fblog&review_load=high&analysis_status=stale&waiting=author%2Cmaintainer&waiting_mode=all",
  );

  assert.deepEqual(activeFilterKeys(query), [
    "repo", "review_load", "analysis_status", "waiting",
  ]);
  assert.deepEqual(activeFilterKeys(new URLSearchParams()), []);
});
