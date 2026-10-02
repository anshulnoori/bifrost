import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { readFileSync, writeFileSync } from "node:fs";
import ts from "typescript";

// Claude Code 2.1.287, Linux x64 glibc only.
// Refuse other builds and never overwrite an existing executable.
export function patchBinary(input, output) {
  const bin = readFileSync(input);
  assert.equal(
    createHash("sha256").update(bin).digest("hex"),
    "3920489a5109cff5786a1a392c25277408ff22bc796d5edb9c16a60e5a1718f0",
    "unsupported Claude executable; re-inspect the build before patching",
  );
  const sectionOffset = 89120768;
  const base = sectionOffset + 8;
  const trailer = bin.lastIndexOf("\n---- Bun! ----\n");
  assert.equal(trailer, 244257718);
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
  assert.equal(source.length, 23369);
  assert.equal(bin.readUInt32LE(at + 28), 81632);
  const compile = (name) => ts
    .transpileModule(readFileSync(new URL(`./${name}.ts`, import.meta.url), "utf8"), {
      compilerOptions: { target: ts.ScriptTarget.ESNext, module: ts.ModuleKind.ESNext, removeComments: true },
    })
    .outputText.replace(
      /import \{([^}]+)\} from "(node:[^"]+)";/g,
      'const {$1} = import.meta.require("$2");',
    )
    .replace(/export /g, "");
  const replacement = Buffer.from(
    readFileSync(new URL("./entry.js", import.meta.url), "utf8").replace(
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
