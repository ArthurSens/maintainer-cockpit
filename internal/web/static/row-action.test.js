import test from "node:test";
import assert from "node:assert/strict";

import { isRowActivation } from "./row-action.js";

test("activates a pull request row from a non-interactive click", () => {
  assert.equal(isRowActivation({
    type: "click",
    target: { closest: () => null },
  }), true);
});

test("leaves links and other controls independently clickable", () => {
  assert.equal(isRowActivation({
    type: "click",
    target: { closest: () => ({}) },
  }), false);
});

test("activates a focused row with Enter or Space", () => {
  const row = {};
  assert.equal(isRowActivation({
    type: "keydown", key: "Enter", target: row, currentTarget: row,
  }), true);
  assert.equal(isRowActivation({
    type: "keydown", key: " ", target: row, currentTarget: row,
  }), true);
  assert.equal(isRowActivation({
    type: "keydown", key: "Escape", target: row, currentTarget: row,
  }), false);
});
