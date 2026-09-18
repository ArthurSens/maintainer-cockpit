import test from "node:test";
import assert from "node:assert/strict";

import { floatingTooltipPosition } from "./tooltip.js";

test("places a tooltip above its trigger when space is available", () => {
  assert.deepEqual(floatingTooltipPosition(
    { top: 200, bottom: 214, left: 300, width: 14 },
    { width: 280, height: 80 },
    { width: 800, height: 600 },
  ), { top: 113, left: 167 });
});

test("places a tooltip below its trigger when the table top would clip it", () => {
  assert.deepEqual(floatingTooltipPosition(
    { top: 20, bottom: 34, left: 300, width: 14 },
    { width: 280, height: 80 },
    { width: 800, height: 600 },
  ), { top: 41, left: 167 });
});

test("keeps a tooltip inside the viewport horizontally", () => {
  assert.deepEqual(floatingTooltipPosition(
    { top: 200, bottom: 214, left: 5, width: 14 },
    { width: 280, height: 80 },
    { width: 320, height: 600 },
  ), { top: 113, left: 12 });
});
