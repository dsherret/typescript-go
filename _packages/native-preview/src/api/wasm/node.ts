/**
 * Node.js entry point for the in-process WebAssembly API.
 *
 * Instantiates the tsgo-wasm reactor with Node's WASI implementation and returns
 * a fully synchronous {@link API} backed by it — no subprocess, no native addon.
 * The module is compiled and instantiated synchronously, so construction blocks
 * until the session is ready; every subsequent request is a plain function call.
 */

import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { WASI } from "node:wasi";
import type { FileSystem } from "../fs.ts";
import type { APIOptions } from "../options.ts";
import { API } from "../sync/api.ts";
import { type WasmExports, WasmChannel } from "../wasmChannel.ts";

/** Default location of the built reactor module within the package. */
const defaultWasmPath = fileURLToPath(new URL("../../../dist/typescript.wasm", import.meta.url));

/**
 * The WebAssembly globals this module uses, declared locally so the package does
 * not need the DOM library for its types.
 */
declare const WebAssembly: {
    Module: new(bytes: Uint8Array | ArrayBuffer) => object;
    Instance: new(module: object, imports: Record<string, unknown>) => { exports: unknown; };
};

export interface NodeWasmApiOptions {
    /**
     * The reactor module: a path to the `.wasm` file, or its bytes. Defaults to
     * the module bundled in this package's `dist/`.
     */
    wasm?: string | Uint8Array | ArrayBuffer;
    /** Current working directory used for module resolution. Defaults to "/". */
    cwd?: string;
    /** Virtual filesystem callbacks. */
    fs?: FileSystem;
    /** When true, collect per-request timing information. */
    collectTiming?: boolean;
}

/**
 * Creates a synchronous {@link API} backed by the in-process WebAssembly reactor.
 */
export function createWasmAPI(options: NodeWasmApiOptions = {}): API {
    const wasm = options.wasm ?? defaultWasmPath;
    const bytes = typeof wasm === "string" ? readFileSync(wasm) : wasm;
    const module = new WebAssembly.Module(bytes);

    const channel = new WasmChannel();
    const wasi = new WASI({ version: "preview1", args: ["tsgo-wasm"], env: {} });
    const instance = new WebAssembly.Instance(module, {
        wasi_snapshot_preview1: wasi.wasiImport,
        ts_host: channel.hostImports,
    });
    // Reactor: run package initialization without invoking a `main`.
    wasi.initialize(instance);
    channel.bind(instance.exports as unknown as WasmExports, options.cwd ?? "/");

    return new API({
        channel,
        fs: options.fs,
        collectTiming: options.collectTiming,
    } as unknown as APIOptions);
}
