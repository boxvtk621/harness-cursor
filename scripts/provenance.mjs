import { createHash } from "node:crypto";
import { execFileSync } from "node:child_process";
import { mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const SOURCE_COMMIT = "d5edfb20f358bb0ce243d07840b49da8566ab0ca";
const root = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const manifestPath = join(root, "provenance", "source-manifest.csv");
const inventoryExact = new Set([
  ".github/workflows/quality.yml",
  ".gitignore",
  "AGENTS.md",
  "README.md",
]);
const inventoryPrefixes = [
  "harness/",
  "internal/strictjson/",
  "agent-service/contracts/logical-delete/",
];

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  try {
    main(process.argv.slice(2));
  } catch (error) {
    process.stderr.write(`${error instanceof Error ? error.message : String(error)}\n`);
    process.exitCode = 1;
  }
}

function main([command, ...args]) {
  const sourceIndex = args.indexOf("--source-repo");
  const sourceRepo = sourceIndex >= 0 ? resolve(args[sourceIndex + 1] ?? "") : "";
  if (command === "generate") {
    if (!sourceRepo) fail("generate requires --source-repo");
    generate(sourceRepo);
  } else if (command === "verify") {
    verify(sourceRepo);
  } else {
    fail("usage: provenance.mjs generate|verify [--source-repo PATH]");
  }
}

function generate(repo) {
  const sourcePaths = inventory(repo);
  const rows = sourcePaths.map((sourcePath) => {
    const source = git(repo, ["cat-file", "blob", `${SOURCE_COMMIT}:${sourcePath}`]);
    const mapping = mapSource(sourcePath);
    if (!mapping.destination) {
      return [SOURCE_COMMIT, sourcePath, "", sha256(source), "", "exclude", mapping.rationale];
    }
    const destinationPath = join(root, ...mapping.destination.split("/"));
    let destination;
    try {
      destination = readFileSync(destinationPath);
    } catch {
      fail(`mapped destination is missing: ${mapping.destination}`);
    }
    const sourceHash = sha256(source);
    const destinationHash = sha256(destination);
    return [
      SOURCE_COMMIT,
      sourcePath,
      mapping.destination,
      sourceHash,
      destinationHash,
      sourceHash === destinationHash ? "copy" : "rewrite",
      mapping.rationale,
    ];
  });
  mkdirSync(dirname(manifestPath), { recursive: true });
  const header = ["source_commit", "source_path", "destination_path", "source_sha256", "destination_sha256", "disposition", "rationale"];
  writeFileSync(manifestPath, [header, ...rows].map(csvLine).join("\n") + "\n");
  verify(repo);
}

function verify(repo) {
  const rows = parseCSV(readFileSync(manifestPath, "utf8"));
  const header = rows.shift();
  const expectedHeader = ["source_commit", "source_path", "destination_path", "source_sha256", "destination_sha256", "disposition", "rationale"];
  if (JSON.stringify(header) !== JSON.stringify(expectedHeader)) fail("manifest header is invalid");

  const sources = new Set();
  const destinations = new Set();
  for (const row of rows) {
    if (row.length !== expectedHeader.length) fail("manifest row has an invalid field count");
    const [commit, sourcePath, destinationPath, sourceHash, destinationHash, disposition, rationale] = row;
    if (commit !== SOURCE_COMMIT || !sourcePath || !/^[0-9a-f]{64}$/.test(sourceHash) || !rationale) {
      fail(`invalid manifest row for ${sourcePath || "<empty>"}`);
    }
    if (sources.has(sourcePath)) fail(`ambiguous source path: ${sourcePath}`);
    sources.add(sourcePath);
    if (disposition === "exclude") {
      if (destinationPath || destinationHash) fail(`excluded source has a destination: ${sourcePath}`);
      continue;
    }
    if (!destinationPath || !/^[0-9a-f]{64}$/.test(destinationHash) || !["copy", "rewrite"].includes(disposition)) {
      fail(`invalid included row for ${sourcePath}`);
    }
    if (destinations.has(destinationPath)) fail(`ambiguous destination path: ${destinationPath}`);
    destinations.add(destinationPath);
    const actualDestination = sha256(readFileSync(join(root, ...destinationPath.split("/"))));
    if (actualDestination !== destinationHash) fail(`destination hash mismatch: ${destinationPath}`);
    if (disposition === "copy" && sourceHash !== destinationHash) fail(`copy hash mismatch: ${sourcePath}`);
  }

  if (repo) {
    const expectedSources = inventory(repo);
    for (const sourcePath of expectedSources) {
      if (!sources.has(sourcePath)) fail(`unmatched source path: ${sourcePath}`);
      const actual = sha256(git(repo, ["cat-file", "blob", `${SOURCE_COMMIT}:${sourcePath}`]));
      const row = rows.find((candidate) => candidate[1] === sourcePath);
      if (row[3] !== actual) fail(`source hash mismatch: ${sourcePath}`);
      validateMappedRow(row);
    }
    for (const sourcePath of sources) {
      if (!expectedSources.includes(sourcePath)) fail(`out-of-scope source path: ${sourcePath}`);
    }
  }
  process.stdout.write(`provenance ok: ${rows.length} source files, ${destinations.size} included\n`);
}

function inventory(repo) {
  return git(repo, ["ls-tree", "-r", "--name-only", SOURCE_COMMIT])
    .toString("utf8")
    .split(/\r?\n/)
    .filter(Boolean)
    .filter((path) => inventoryExact.has(path) || inventoryPrefixes.some((prefix) => path.startsWith(prefix)))
    .sort();
}

function mapSource(path) {
  const rewrittenRoot = {
    ".github/workflows/quality.yml": ".github/workflows/quality.yml",
    ".gitignore": ".gitignore",
    "AGENTS.md": "AGENTS.md",
    "README.md": "README.md",
    "harness/Makefile": "Makefile",
    "harness/go.mod": "go.mod",
    "harness/go.sum": "go.sum",
    "harness/delivery/Dockerfile.cursor": "delivery/Dockerfile.cursor",
    "harness/delivery/Dockerfile.cursor.dockerignore": "delivery/Dockerfile.cursor.dockerignore",
  };
  if (rewrittenRoot[path]) return { destination: rewrittenRoot[path], rationale: "standalone path or repository boundary rewrite" };
  if (path.startsWith("internal/strictjson/")) return { destination: path, rationale: "minimal local strict JSON dependency" };
  if (path.startsWith("agent-service/contracts/logical-delete/")) {
    return { destination: path.replace("agent-service/contracts/", "contracts/"), rationale: "localized logical-delete DTO dependency" };
  }

  const omitted = new Map([
    ["harness/runtime/approval_race_recovery.go", "provider-specific Codex recovery"],
    ["harness/runtime/approval_race_recovery_test.go", "provider-specific Codex recovery test"],
    ["harness/runtime/code_mode_recovery.go", "provider-specific Codex recovery"],
    ["harness/runtime/code_mode_recovery_test.go", "provider-specific Codex recovery test"],
    ["harness/runtime/delta_overflow_recovery.go", "provider-specific Codex recovery"],
    ["harness/runtime/delta_overflow_recovery_test.go", "provider-specific Codex recovery test"],
    ["harness/runtime/history_coordinator_integration_test.go", "cross-component Agent Service and Panel integration test"],
    ["harness/tests/integration/artifact_http_test.go", "test-only dependency on excluded Harness client"],
    ["harness/tests/integration/barrier_http_test.go", "test-only dependency on excluded Harness client"],
    ["harness/tests/integration/http_test.go", "test-only dependency on excluded Harness client"],
    ["harness/tests/integration/scheduler_http_test.go", "test-only dependency on excluded Harness client"],
  ]);
  if (omitted.has(path)) return { destination: "", rationale: omitted.get(path) };

  const includedPrefixes = [
    "harness/adapters/contract/",
    "harness/adapters/cursor/",
    "harness/api/",
    "harness/cmd/",
    "harness/contracts/",
    "harness/runtime/",
    "harness/tests/fixture/",
    "harness/tests/integration/",
    "harness/tools/",
  ];
  if (includedPrefixes.some((prefix) => path.startsWith(prefix))) {
    return { destination: path.slice("harness/".length), rationale: "Cursor Harness dependency closure" };
  }
  if (path.startsWith("harness/adapters/codex/")) return { destination: "", rationale: "Codex provider is outside this repository" };
  if (path.startsWith("harness/client/")) return { destination: "", rationale: "not reachable from the Cursor node binary" };
  if (path.startsWith("harness/delivery/Dockerfile.codex")) return { destination: "", rationale: "Codex image is outside this repository" };
  if (path.startsWith("harness/delivery/harness-backup/")) return { destination: "", rationale: "host backup provisioning is outside the standalone node closure" };
  if (path.startsWith("harness/delivery/harness-probe/")) return { destination: "", rationale: "mixed-provider probe tooling is outside the standalone node closure" };
  if (path === "harness/README.md") return { destination: "", rationale: "replaced by standalone repository README" };
  fail(`source path has no explicit disposition: ${path}`);
}

export function validateMappedRow(row) {
  const [, sourcePath, destinationPath, sourceHash, destinationHash, disposition, rationale] = row;
  const mapping = mapSource(sourcePath);
  const expectedDisposition = mapping.destination
    ? sourceHash === destinationHash ? "copy" : "rewrite"
    : "exclude";
  if (destinationPath !== mapping.destination || disposition !== expectedDisposition || rationale !== mapping.rationale) {
    fail(`source mapping mismatch: ${sourcePath}`);
  }
}

function git(repo, args) {
  return execFileSync("git", ["-c", `safe.directory=${repo.replaceAll("\\", "/")}`, "-C", repo, ...args], { encoding: null });
}

function sha256(value) {
  return createHash("sha256").update(value).digest("hex");
}

function csvLine(fields) {
  return fields.map((value) => `"${String(value).replaceAll('"', '""')}"`).join(",");
}

export function parseCSV(text) {
  const rows = [];
  const content = text.replace(/\r?\n$/, "");
  if (content === "") return rows;
  for (const line of content.split(/\r?\n/)) {
    const fields = [];
    let index = 0;
    while (index < line.length) {
      if (line[index] !== '"') fail("manifest fields must be quoted");
      index += 1;
      let value = "";
      let closed = false;
      while (index < line.length) {
        if (line[index] === '"') {
          if (line[index + 1] === '"') {
            value += '"';
            index += 2;
            continue;
          }
          closed = true;
          index += 1;
          break;
        }
        value += line[index++];
      }
      if (!closed) fail("manifest quoted field is unterminated");
      fields.push(value);
      if (index === line.length) break;
      if (line[index] !== ",") fail("manifest separator is invalid");
      index += 1;
      if (index === line.length) fail("manifest field is missing");
    }
    rows.push(fields);
  }
  return rows;
}

function fail(message) {
  throw new Error(message);
}
