import test from "node:test";
import assert from "node:assert/strict";

import {
  groupMinePullRequests,
  importantPath,
  presentImportant,
} from "./important.js";

test("presents an accessible Gmail-style importance toggle", () => {
  assert.deepEqual(presentImportant(false), {
    active: false,
    className: "importance-toggle",
    label: "Mark as Important",
    icon: "☆",
  });
  assert.deepEqual(presentImportant(true), {
    active: true,
    className: "importance-toggle active",
    label: "Remove from Important",
    icon: "⭐",
  });
});

test("builds a collection-scoped Important mutation path", () => {
  assert.equal(
    importantPath("arthursens", {
      repository: "ArthurSens/maintainer-cockpit",
      number: 23,
    }),
    "/api/collections/arthursens/pull-requests/ArthurSens/maintainer-cockpit/23/important",
  );
  assert.throws(() => importantPath("arthursens", {
    repository: "invalid", number: 23,
  }));
});

test("groups Mine into authored and Important tables without hiding overlap", () => {
  const authored = { number: 1, authorID: 42 };
  const important = { number: 2, authorID: 99, personal: { important: true } };
  const both = { number: 3, authorID: 42, personal: { important: true } };

  assert.deepEqual(
    groupMinePullRequests([authored, important, both], 42),
    {
      authored: [authored, both],
      important: [important, both],
    },
  );
});
