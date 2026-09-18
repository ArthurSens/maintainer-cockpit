import test from "node:test";
import assert from "node:assert/strict";

import {
  adminPath,
  authorPath,
  collectionPath,
  navigationTarget,
  parseLocation,
  settingsPath,
} from "./router.js";

test("parses the public collections landing page", () => {
  assert.deepEqual(parseLocation("/"), { page: "collections" });
});

test("parses a canonical collection page", () => {
  assert.deepEqual(parseLocation("/collections/exporters"), {
    page: "collection",
    collectionID: "exporters",
  });
});

test("rejects malformed collection routes", () => {
  assert.equal(parseLocation("/collections/Exporters"), null);
  assert.equal(parseLocation("/collections/exporters/extra"), null);
  assert.equal(parseLocation("/collections/%E0%A4%A"), null);
});

test("builds collection paths without string-concatenated URLs", () => {
  const url = new URL(collectionPath("otel-interop"), "https://example.test");
  assert.equal(url.href, "https://example.test/collections/otel-interop");
});

test("parses and builds public author paths", () => {
  assert.deepEqual(parseLocation("/collections/otel/authors/octocat"), {
    page: "author",
    collectionID: "otel",
    login: "octocat",
  });
  assert.equal(authorPath("otel", "octo cat"), "/collections/otel/authors/octo%20cat");
});

test("parses and builds the private settings page", () => {
  assert.deepEqual(parseLocation("/settings"), { page: "settings" });
  assert.equal(settingsPath(), "/settings");
});

test("parses and builds the deployment administrator page", () => {
  assert.deepEqual(parseLocation("/admin"), { page: "admin" });
  assert.equal(adminPath(), "/admin");
});

test("keeps view parameters during client-side navigation", () => {
  const url = new URL("https://example.test/collections/otel?view=mine");
  assert.equal(navigationTarget(url), "/collections/otel?view=mine");
});
