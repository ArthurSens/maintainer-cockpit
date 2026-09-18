import test from "node:test";
import assert from "node:assert/strict";

import {
  groupHiddenPullRequests,
  hiddenChoiceRequest,
  hiddenPath,
  hiddenRequest,
  presentHidden,
} from "./hidden.js";

test("hiddenPath builds a collection-specific private-state endpoint", () => {
  assert.equal(
    hiddenPath("acme", { repository: "acme/widgets", number: 7 }),
    "/api/collections/acme/pull-requests/acme/widgets/7/hidden",
  );
  assert.throws(
    () => hiddenPath("acme", { repository: "not-a-repository", number: 7 }),
    /invalid hidden pull request identity/,
  );
});

test("hiddenRequest requires a snooze condition and normalizes dates", () => {
  assert.deepEqual(hiddenRequest({
    kind: "snoozed",
    reason: "  Waiting for feedback  ",
    snoozedUntil: "2026-09-12T09:30",
    wakeOnActivity: true,
  }), {
    kind: "snoozed",
    reason: "Waiting for feedback",
    snoozedUntil: new Date("2026-09-12T09:30").toISOString(),
    wakeOnActivity: true,
  });
  assert.throws(
    () => hiddenRequest({ kind: "snoozed", reason: "", wakeOnActivity: false }),
    /date or next activity/,
  );
  assert.deepEqual(hiddenRequest({
    kind: "ignored",
    reason: "Not my area",
    snoozedUntil: "2026-09-12T09:30",
    wakeOnActivity: true,
  }), {
    kind: "ignored",
    reason: "Not my area",
    snoozedUntil: null,
    wakeOnActivity: false,
  });
});

test("presentHidden exposes private labels and new ignored activity", () => {
  assert.deepEqual(presentHidden({
    kind: "ignored",
    reason: "Not my area",
    newActivity: true,
  }), {
    active: true,
    kind: "ignored",
    label: "Ignored",
    reason: "Not my area",
    newActivity: true,
    condition: "Until restored",
  });
  assert.equal(presentHidden(null).active, false);
});

test("presents timed snoozes as a human-readable remaining duration", () => {
  const now = new Date("2026-09-10T12:00:00Z");
  const cases = [
    ["2026-09-10T12:30:00Z", "30 min remaining"],
    ["2026-09-10T18:00:00Z", "6 hours remaining"],
    ["2026-09-13T12:00:00Z", "3 days remaining"],
    ["2026-10-01T12:00:00Z", "3 weeks remaining"],
    ["2027-03-10T12:00:00Z", "6 months remaining"],
    ["2028-09-10T12:00:00Z", "2 years remaining"],
  ];
  for (const [snoozedUntil, condition] of cases) {
    assert.equal(
      presentHidden({ kind: "snoozed", snoozedUntil }, now).condition,
      condition,
    );
  }
});

test("groups the combined Hidden view into Snoozed and Ignored tables", () => {
  const pullRequests = [
    { number: 1, personal: { hidden: { kind: "ignored" } } },
    { number: 2, personal: { hidden: { kind: "snoozed" } } },
    { number: 3 },
  ];
  assert.deepEqual(groupHiddenPullRequests(pullRequests), {
    snoozed: [pullRequests[1]],
    ignored: [pullRequests[0]],
  });
});

test("maps the row modal choices to forever, activity, or timed state", () => {
  const now = new Date("2026-09-10T12:00:00Z");
  assert.deepEqual(hiddenChoiceRequest({
    choice: "forever", reason: "Not relevant",
  }, now), {
    kind: "ignored",
    reason: "Not relevant",
    snoozedUntil: null,
    wakeOnActivity: false,
  });
  assert.deepEqual(hiddenChoiceRequest({
    choice: "activity", reason: "",
  }, now), {
    kind: "snoozed",
    reason: "",
    snoozedUntil: null,
    wakeOnActivity: true,
  });
  assert.deepEqual(hiddenChoiceRequest({
    choice: "period",
    snoozedUntil: "2026-09-11T12:00:00Z",
    reason: "Revisit tomorrow",
  }, now), {
    kind: "snoozed",
    reason: "Revisit tomorrow",
    snoozedUntil: "2026-09-11T12:00:00.000Z",
    wakeOnActivity: false,
  });
  assert.throws(
    () => hiddenChoiceRequest({
      choice: "period", snoozedUntil: "2026-09-10T11:00:00Z",
    }, now),
    /future date and time/,
  );
});
