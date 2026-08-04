const assert = require("node:assert/strict");
const test = require("node:test");

const {
  assertReleaseMajorMatchesModule,
  parseModulePath,
  parseReleaseMajor,
} = require("./verify-release-major.cjs");

test("parseModulePath returns the semantic import major", () => {
  assert.equal(parseModulePath("module github.com/LerianStudio/lib-observability/v2\n"), 2);
  assert.equal(parseModulePath("module github.com/LerianStudio/lib-observability\n"), 1);
});

test("parseModulePath rejects missing and malformed module directives", () => {
  assert.throws(() => parseModulePath("go 1.26.3\n"), /module directive/);
  assert.throws(() => parseModulePath("module github.com/example/project/v1\n"), /invalid semantic import version/);
});

test("parseReleaseMajor accepts semantic-release versions", () => {
  assert.equal(parseReleaseMajor("2.1.1"), 2);
  assert.equal(parseReleaseMajor("v2.2.0-beta.1"), 2);
});

test("parseReleaseMajor rejects malformed versions", () => {
  assert.throws(() => parseReleaseMajor("release-2"), /invalid release version/);
});

test("assertReleaseMajorMatchesModule accepts matching majors", () => {
  assert.doesNotThrow(() =>
    assertReleaseMajorMatchesModule(
      "module github.com/LerianStudio/lib-observability/v2\n",
      "2.1.1",
    ),
  );
});

test("assertReleaseMajorMatchesModule blocks mismatched majors", () => {
  assert.throws(
    () =>
      assertReleaseMajorMatchesModule(
        "module github.com/LerianStudio/lib-observability/v2\n",
        "3.0.0",
      ),
    /release major v3 does not match Go module major v2/,
  );
});
