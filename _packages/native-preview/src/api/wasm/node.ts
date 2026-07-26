/**
 * Node.js entry point for the in-process WebAssembly API.
 *
 * Instantiates the tsgo-wasm reactor with Node's WASI implementation and returns
 * a fully synchronous {@link API} backed by it — no subprocess, no native addon.
 * The module is compiled and instantiated synchronously, so construction blocks
 * until the session is ready; every subsequent request is a plain function call.
 */

import { existsSync, readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { WASI } from "node:wasi";
import type { FileSystem } from "../fs.ts";
import type { APIOptions } from "../options.ts";
import { API } from "../sync/api.ts";
import { type WasmExports, WasmChannel } from "../wasmChannel.ts";

/**
 * Default location of the built reactor module.
 *
 * Normally that is `dist/typescript.wasm` within this package. A bundler that
 * inlines this module invalidates that relative path, so a copy placed beside
 * the bundle is accepted too.
 */
function getDefaultWasmPath(): string {
    const candidates = ["../../../dist/typescript.wasm", "./typescript.wasm"]
        .map(candidate => fileURLToPath(new URL(candidate, import.meta.url)));
    return candidates.find(existsSync) ?? candidates[0];
}

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
    /**
     * Directory the default lib files are read from, through {@link fs}. Defaults
     * to the lib files bundled in the module.
     */
    defaultLibraryPath?: string;
    /** Whether the file system distinguishes case. Defaults to true. */
    useCaseSensitiveFileNames?: boolean;
    /** Virtual filesystem callbacks. */
    fs?: FileSystem;
    /** When true, collect per-request timing information. */
    collectTiming?: boolean;
}

/**
 * Creates a synchronous {@link API} backed by the in-process WebAssembly reactor.
 */
export function createWasmAPI(options: NodeWasmApiOptions = {}): API {
    const module = compileModule(options.wasm ?? getDefaultWasmPath());

    const channel = new WasmChannel();
    const wasi = new WASI({ version: "preview1", args: ["tsgo-wasm"], env: {} });
    const instance = new WebAssembly.Instance(module, {
        wasi_snapshot_preview1: wasi.wasiImport,
        ts_host: channel.hostImports,
    });
    // Reactor: run package initialization without invoking a `main`.
    wasi.initialize(instance);
    channel.bind(instance.exports as unknown as WasmExports, {
        cwd: options.cwd ?? "/",
        ...(options.defaultLibraryPath !== undefined ? { defaultLibraryPath: options.defaultLibraryPath } : {}),
        ...(options.useCaseSensitiveFileNames !== undefined ? { useCaseSensitiveFileNames: options.useCaseSensitiveFileNames } : {}),
    });

    return new API({
        channel,
        fs: options.fs,
        collectTiming: options.collectTiming,
    } as unknown as APIOptions);
}

/**
 * Compiled modules, keyed by wasm path. The reactor is tens of megabytes and
 * ts-morph creates an API per operation in places, so compiling it once and
 * instantiating it many times is the difference between ~40ms and ~1ms of
 * startup per instance. Only path-specified modules are cached; caller-supplied
 * bytes are compiled each time, since they carry no stable identity.
 */
const moduleCache = new Map<string, object>();

function compileModule(wasm: string | Uint8Array | ArrayBuffer): object {
    if (typeof wasm !== "string") {
        return new WebAssembly.Module(wasm);
    }
    let module = moduleCache.get(wasm);
    if (module === undefined) {
        module = new WebAssembly.Module(readFileSync(wasm));
        moduleCache.set(wasm, module);
    }
    return module;
}
