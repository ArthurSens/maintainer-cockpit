import test from "node:test";
import assert from "node:assert/strict";

import {
  clampGraphZoom,
  clampNodePosition,
  edgePresentation,
  graphLayout,
  isPointOutsideRect,
  memberLabelLines,
  memberRelationshipSummaries,
  visibleMembers,
  groupDetailPath,
} from "./groups.js";

test("hides historical members until requested", () => {
  const members = [
    { sourceID: "one", state: "open" },
    { sourceID: "two", state: "closed" },
    { sourceID: "three", state: "merged" },
  ];
  assert.deepEqual(visibleMembers(members, false), [members[0]]);
  assert.deepEqual(visibleMembers(members, true), members);
});

test("lays out every visible member without requiring a center node", () => {
  const members = [
    { sourceID: "one" },
    { sourceID: "two" },
    { sourceID: "three" },
  ];
  const positions = graphLayout(members, 640, 360);
  assert.equal(positions.size, 3);
  for (const member of members) {
    const position = positions.get(member.sourceID);
    assert.ok(position.x > 0 && position.x < 640);
    assert.ok(position.y > 0 && position.y < 360);
  }
  assert.notDeepEqual(positions.get("one"), positions.get("two"));
});

test("builds an encoded public group detail endpoint", () => {
  assert.equal(
    groupDetailPath("otel-interop", "feature/a b"),
    "/api/collections/otel-interop/groups/feature%2Fa%20b",
  );
});

test("wraps repository identities without hiding the pull request number", () => {
  assert.deepEqual(
    memberLabelLines({ repository: "ArthurSens/prometheus-operator", number: 310 }),
    ["ArthurSens/", "prometheus-operator #310"],
  );
});

test("presents relationship labels and direction explicitly", () => {
  assert.deepEqual(edgePresentation("stacked_on_top_of"), {
    label: "stacked on top of",
    directed: true,
  });
  assert.deepEqual(edgePresentation("competing_design"), {
    label: "competing design",
    directed: false,
  });
});

test("keeps graph zoom within usable bounds", () => {
  assert.equal(clampGraphZoom(0.1), 0.6);
  assert.equal(clampGraphZoom(1.4), 1.4);
  assert.equal(clampGraphZoom(8), 2.5);
});

test("distinguishes backdrop clicks from clicks inside the dialog", () => {
  const rect = { left: 20, right: 120, top: 30, bottom: 90 };
  assert.equal(isPointOutsideRect({ x: 10, y: 50 }, rect), true);
  assert.equal(isPointOutsideRect({ x: 80, y: 50 }, rect), false);
});

test("keeps dragged nodes fully inside the graph", () => {
  assert.deepEqual(
    clampNodePosition({ x: -20, y: 500 }, 760, 420),
    { x: 105, y: 388 },
  );
});

test("summarizes incoming, outgoing, and undirected node relationships", () => {
  const edges = [
    { from: "one", to: "two", type: "stacked_on_top_of", reason: "Depends on two." },
    { from: "three", to: "one", type: "related", reason: "Shares a feature." },
  ];
  assert.deepEqual(memberRelationshipSummaries("one", edges), [
    "stacked on top of → two: Depends on two.",
    "related — three: Shares a feature.",
  ]);
  assert.deepEqual(memberRelationshipSummaries("two", edges), [
    "stacked on top of ← one: Depends on two.",
  ]);
});
