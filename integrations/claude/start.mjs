import { spawn } from "node:child_process";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { nativeBinary, patchBinary } from "./patch.mjs";

// Development launcher only; production executes the build-time patched binary.
const dir = mkdtempSync(join(tmpdir(), "bifrost-claude-"));
process.on("exit", () => rmSync(dir, { recursive: true, force: true }));
const binary = join(dir, "claude-raw");
patchBinary(
  nativeBinary,
  binary,
);
const child = spawn(binary, [], { stdio: "inherit", env: process.env });
for (const signal of ["SIGINT", "SIGTERM"]) process.on(signal, () => child.kill(signal));
child.on("error", () => {
  process.exitCode = 1;
});
child.on("exit", (code) => {
  process.exitCode = code ?? 1;
});
