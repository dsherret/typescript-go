// Builds the tsgo-wasm reactor module (see cmd/tsgo-wasm) into the
// native-preview package so it can be shipped and loaded in-process.
//
//   node _scripts/build-wasm.mjs
//
// Requires the Go toolchain (see go.mod for the required version).
//
// The reactor ships gzipped: 43 MiB of Go compiles to about 9.5 MiB, which the
// loader gunzips on first use. See tsgo-wasm/BREAKING-CHANGES.md §7 for the
// trade.
import { execFileSync } from "node:child_process";
import {
    mkdirSync,
    readFileSync,
    rmSync,
    writeFileSync,
} from "node:fs";
import {
    dirname,
    join,
} from "node:path";
import { fileURLToPath } from "node:url";
import { gzipSync } from "node:zlib";

const repoRoot = fileURLToPath(new URL("../", import.meta.url));
const distDir = join(repoRoot, "_packages", "native-preview", "dist");
// The Go build has to land somewhere before it can be compressed; only the
// compressed copy is kept, so that there is one artifact and no way for a stale
// uncompressed one to be picked up instead.
const raw = join(distDir, "typescript.wasm");
const out = join(distDir, "typescript.wasm.gz");
mkdirSync(dirname(raw), { recursive: true });

execFileSync("go", ["build", "-buildmode=c-shared", "-o", raw, "./cmd/tsgo-wasm"], {
    cwd: repoRoot,
    stdio: "inherit",
    env: { ...process.env, GOOS: "wasip1", GOARCH: "wasm" },
});

const bytes = readFileSync(raw);
// Level 9 rather than the default 6: it is 0.8% smaller for a second of build
// time, and this file is downloaded far more often than it is built.
const compressed = gzipSync(bytes, { level: 9 });
writeFileSync(out, compressed);
rmSync(raw);

console.log(`built ${out} (${mib(compressed.length)} gzipped, ${mib(bytes.length)} raw)`);

function mib(byteLength) {
    return `${(byteLength / 1024 / 1024).toFixed(2)} MiB`;
}
