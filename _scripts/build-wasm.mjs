// Builds the tsgo-wasm reactor module (see cmd/tsgo-wasm) into the
// native-preview package so it can be shipped and loaded in-process.
//
//   node _scripts/build-wasm.mjs
//
// Requires the Go toolchain (see go.mod for the required version).
import { execFileSync } from "node:child_process";
import {
    mkdirSync,
    statSync,
} from "node:fs";
import {
    dirname,
    join,
} from "node:path";
import { fileURLToPath } from "node:url";

const repoRoot = fileURLToPath(new URL("../", import.meta.url));
const out = join(repoRoot, "_packages", "native-preview", "dist", "typescript.wasm");
mkdirSync(dirname(out), { recursive: true });

execFileSync("go", ["build", "-buildmode=c-shared", "-o", out, "./cmd/tsgo-wasm"], {
    cwd: repoRoot,
    stdio: "inherit",
    env: { ...process.env, GOOS: "wasip1", GOARCH: "wasm" },
});

console.log(`built ${out} (${mib(statSync(out).size)})`);

function mib(byteLength) {
    return `${(byteLength / 1024 / 1024).toFixed(2)} MiB`;
}
