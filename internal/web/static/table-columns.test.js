import test from "node:test";
import assert from "node:assert/strict";

import {
  defaultTableLayout,
  normalizeTableLayout,
  reorderTableColumn,
  toggleTableColumn,
  visibleTableColumns,
} from "./table-columns.js";

test("defaults to a focused table with fixed identity and actions", () => {
  const layout = defaultTableLayout({ showActions: true });

  assert.deepEqual(layout.map(({ id }) => id), [
    "actions", "number", "repository", "title", "contribution",
    "churn", "quality", "review_load", "waiting", "review", "updated",
  ]);
  assert.deepEqual(visibleTableColumns(layout).map(({ id }) => id), [
    "actions", "number", "repository", "title",
    "churn", "quality", "review_load", "waiting", "updated",
  ]);
  assert.equal(
    layout.find(({ id }) => id === "review_load").label,
    "Review Cognitive Load",
  );
});

test("toggles optional columns without hiding fixed columns", () => {
  const layout = defaultTableLayout({ showActions: true });

  assert.equal(
    toggleTableColumn(layout, "contribution")
      .find(({ id }) => id === "contribution").visible,
    true,
  );
  assert.equal(
    toggleTableColumn(layout, "title")
      .find(({ id }) => id === "title").visible,
    true,
  );
});

test("reorders data columns while keeping actions pinned first", () => {
  const layout = defaultTableLayout({ showActions: true });
  const reordered = reorderTableColumn(layout, "updated", "number");

  assert.deepEqual(reordered.slice(0, 3).map(({ id }) => id), [
    "actions", "updated", "number",
  ]);
  assert.deepEqual(
    reorderTableColumn(reordered, "actions", "title"),
    reordered,
  );
});

test("normalizes persisted layouts across authentication and schema changes", () => {
  const saved = [
    { id: "review", visible: true },
    { id: "title", visible: false },
    { id: "unknown", visible: true },
    { id: "review", visible: false },
  ];
  const anonymous = normalizeTableLayout(saved, { showActions: false });
  const authenticated = normalizeTableLayout(saved, { showActions: true });

  assert.equal(anonymous.some(({ id }) => id === "actions"), false);
  assert.equal(anonymous.some(({ id }) => id === "unknown"), false);
  assert.equal(anonymous.find(({ id }) => id === "title").visible, true);
  assert.equal(anonymous.find(({ id }) => id === "review").visible, true);
  assert.equal(authenticated[0].id, "actions");
});
