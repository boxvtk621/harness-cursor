import assert from "node:assert/strict";
import test from "node:test";

import { parseCSV, validateMappedRow } from "./provenance.mjs";

const sourceHash = "a".repeat(64);
const destinationHash = "b".repeat(64);
const included = [
  "d5edfb20f358bb0ce243d07840b49da8566ab0ca",
  "harness/go.mod",
  "go.mod",
  sourceHash,
  destinationHash,
  "rewrite",
  "standalone path or repository boundary rewrite",
];

test("parseCSV rejects an unterminated final quoted field", () => {
  assert.throws(() => parseCSV('"source","unterminated'), /unterminated/);
});

test("parseCSV accepts escaped quotes and a final newline", () => {
  assert.deepEqual(parseCSV('"a""b","c"\n'), [["a\"b", "c"]]);
});

test("source mapping rejects contradictory destination, disposition, and rationale", () => {
  assert.doesNotThrow(() => validateMappedRow(included));
  for (const [index, value] of [
    [2, "wrong/go.mod"],
    [5, "copy"],
    [6, "invented rationale"],
  ]) {
    const row = [...included];
    row[index] = value;
    assert.throws(() => validateMappedRow(row), /source mapping mismatch/);
  }
});

test("excluded source must retain its canonical empty destination", () => {
  const excluded = [
    included[0],
    "harness/adapters/codex/adapter.go",
    "",
    sourceHash,
    "",
    "exclude",
    "Codex provider is outside this repository",
  ];
  assert.doesNotThrow(() => validateMappedRow(excluded));
  excluded[2] = "adapters/codex/adapter.go";
  assert.throws(() => validateMappedRow(excluded), /source mapping mismatch/);
});
