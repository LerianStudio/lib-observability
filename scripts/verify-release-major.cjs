#!/usr/bin/env node

const fs = require("node:fs");

function parseModulePath(goMod) {
  const directive = goMod.match(/^\s*module\s+(\S+)\s*$/m);
  if (!directive) {
    throw new Error("go.mod is missing its module directive");
  }

  const majorSuffix = directive[1].match(/\/v(\d+)$/);
  if (!majorSuffix) {
    return 1;
  }

  const major = Number.parseInt(majorSuffix[1], 10);
  if (!Number.isSafeInteger(major) || major < 2) {
    throw new Error(`invalid semantic import version in module path: ${directive[1]}`);
  }

  return major;
}

function parseReleaseMajor(version) {
  const semanticVersion = version.match(/^v?(\d+)\.\d+\.\d+(?:[-+][0-9A-Za-z.-]+)?$/);
  if (!semanticVersion) {
    throw new Error(`invalid release version: ${version}`);
  }

  return Number.parseInt(semanticVersion[1], 10);
}

function assertReleaseMajorMatchesModule(goMod, version) {
  const moduleMajor = parseModulePath(goMod);
  const releaseMajor = parseReleaseMajor(version);

  if (moduleMajor !== releaseMajor) {
    throw new Error(
      `release major v${releaseMajor} does not match Go module major v${moduleMajor}`,
    );
  }
}

if (require.main === module) {
  try {
    const version = process.argv[2] || "";
    const goMod = fs.readFileSync("go.mod", "utf8");
    assertReleaseMajorMatchesModule(goMod, version);
  } catch (error) {
    console.error(`verify-release-major: ${error.message}`);
    process.exitCode = 1;
  }
}

module.exports = {
  assertReleaseMajorMatchesModule,
  parseModulePath,
  parseReleaseMajor,
};
