import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { readFileSync, writeFileSync } from "node:fs";
import ts from "typescript";

export const nativeBinary = new URL(
  `./node_modules/@anthropic-ai/claude-agent-sdk-linux-${process.arch}/claude`,
  import.meta.url,
);

// Claude Code 2.1.287, Linux x64 and ARM64 glibc.
// Refuse other builds and never overwrite an existing executable.
export function patchBinary(input, output) {
  const bin = readFileSync(input);
  const build = {
    "3920489a5109cff5786a1a392c25277408ff22bc796d5edb9c16a60e5a1718f0": {
      machine: 62, sectionOffset: 89120768, trailer: 244257718,
      sourceLength: 23369, bytecodeLength: 81632,
    },
    "e4daf793d1e74fb0d9874dd09e98690bbfd7be515f78a87fd05b9e2b4bb33b03": {
      machine: 183, sectionOffset: 88604672, trailer: 243599044,
      sourceLength: 23282, bytecodeLength: 81608,
    },
  }[createHash("sha256").update(bin).digest("hex")];
  assert.ok(
    build,
    "unsupported Claude executable; re-inspect the build before patching",
  );
  assert.equal(bin.readUInt16LE(18), build.machine);
  const base = build.sectionOffset + 8;
  const trailer = bin.lastIndexOf("\n---- Bun! ----\n");
  assert.equal(trailer, build.trailer);
  const footer = trailer - 32;
  const modulesOffset = bin.readUInt32LE(footer + 8);
  const modulesLength = bin.readUInt32LE(footer + 12);
  const entry = bin.readUInt32LE(footer + 16);
  assert.equal(entry, 6);
  const at = base + modulesOffset + entry * 52;
  const pointer = (offset) =>
    bin.subarray(
      base + bin.readUInt32LE(offset),
      base + bin.readUInt32LE(offset) + bin.readUInt32LE(offset + 4),
    );
  assert.equal(pointer(at).toString(), "/$bunfs/root/cli");
  const source = pointer(at + 8);
  assert.equal(source.length, build.sourceLength);
  assert.equal(bin.readUInt32LE(at + 28), build.bytecodeLength);
  const compile = (name) => ts
    .transpileModule(readFileSync(new URL(`./${name}.ts`, import.meta.url), "utf8"), {
      compilerOptions: { target: ts.ScriptTarget.ESNext, module: ts.ModuleKind.ESNext, removeComments: true },
    })
    .outputText.replace(
      /import \{([^}]+)\} from "(node:[^"]+)";/g,
      'const {$1} = import.meta.require("$2");',
    )
    .replace(/export /g, "");
  let entrySource = readFileSync(new URL("./entry.js", import.meta.url), "utf8");
  if (build.machine === 183) {
    // Inspected ARM bundle imports. Keep the bridge's native interface unchanged.
    for (const [from, to] of [
      ["chunk-da9jta6b.js", "chunk-2sd5rhmn.js"],
      ["chunk-cd7krv4h.js", "chunk-6wmwkkdp.js"],
      ["chunk-y8e2dq66.js", "chunk-9a2naz8s.js"],
      ["chunk-ktfrs76h.js", "chunk-bkt3vxmz.js"],
      ["chunk-djntk4j0.js", "chunk-feyfqvka.js"],
      ["chunk-vrng99ca.js", "chunk-rtycvjbr.js"],
      ["chunk-ydbv64xy.js", "chunk-4z0v3gv5.js"],
      ["chunk-k985080f.js", "chunk-g77csgy5.js"],
      ["const { tD }", "const { tN: tD }"],
      ["const { LHe }", "const { LMe: LHe }"],
      ["const { el, In, h$, JU }", "const { el, On: In, h$, JB: JU }"],
    ]) {
      assert.ok(entrySource.includes(from), `missing pinned native import: ${from}`);
      entrySource = entrySource.replaceAll(from, to);
    }
  }
  const replacement = Buffer.from(
    entrySource.replace(
      "// __BIFROST_BRIDGE__",
      compile("bridge"),
    ).replace("// __BIFROST_ACCOUNTS__", compile("accounts")),
  );
  assert.ok(replacement.length <= source.length);
  source.fill(32);
  replacement.copy(source);
  // Force Bun to compile the new entry source rather than execute old bytecode.
  // Leave the other embedded modules and native executable layout unchanged.
  bin.writeUInt32LE(0, at + 28);
  bin.writeUInt32LE(0, base + modulesOffset + modulesLength + entry * 4);
  writeFileSync(output, bin, { flag: "wx", mode: 0o700 });
}

if (process.argv[1] === new URL(import.meta.url).pathname) {
  if (process.argv.length !== 4) throw new Error("usage: node patch.mjs INPUT OUTPUT");
  patchBinary(process.argv[2], process.argv[3]);
}
