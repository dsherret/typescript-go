/**
 * In-process WebAssembly transport for the API client.
 *
 * The tsgo-wasm reactor module (see cmd/tsgo-wasm) runs the API server in the
 * same process as the client. This channel drives it synchronously and exposes
 * the same surface as the subprocess-backed {@link SyncRpcChannel}, so the two
 * are interchangeable behind {@link Client}. Filesystem access is delegated back
 * to JS through the host imports returned by {@link WasmChannel.hostImports},
 * reusing the same callback contract as the STDIO server.
 */

/**
 * A synchronous request channel to the API server. Implemented by both the
 * subprocess-backed SyncRpcChannel and the in-process {@link WasmChannel}.
 */
export interface RpcChannel {
    lastBytesSent: number;
    lastBytesReceived: number;
    requestSync(method: string, payload: string): string;
    requestBinarySync(method: string, payload: Uint8Array): Uint8Array;
    registerCallback(name: string, callback: (name: string, payload: string) => string): void;
    close(): void;
}

/** The exports of the tsgo-wasm reactor module. */
export interface WasmExports {
    memory: WebAssembly.Memory;
    create_session(cwdPtr: number, cwdLen: number): void;
    get_request_buffer(size: number): number;
    handle_request(methodLen: number, payloadLen: number): number;
    response_ptr(): number;
    response_len(): number;
    response_is_binary(): number;
}

/** The `ts_host` import module the reactor expects. */
export interface WasmHostImports {
    callback(namePtr: number, nameLen: number, argPtr: number, argLen: number): number;
    read_result(destPtr: number): void;
}

const EMPTY = new Uint8Array(0);

export class WasmChannel implements RpcChannel {
    lastBytesSent = 0;
    lastBytesReceived = 0;

    private exports: WasmExports | undefined;
    private readonly callbacks = new Map<string, (name: string, payload: string) => string>();
    private readonly encoder = new TextEncoder();
    private readonly decoder = new TextDecoder();
    private callbackResult = EMPTY;

    /**
     * The `ts_host` imports bound to this channel. Pass these to the module's
     * import object. They are only invoked while a request is in flight (i.e.
     * after {@link bind}), so it is safe to read them before binding.
     */
    readonly hostImports: WasmHostImports = {
        callback: (namePtr, nameLen, argPtr, argLen) => {
            const name = this.readString(namePtr, nameLen);
            const arg = this.readString(argPtr, argLen);
            const callback = this.callbacks.get(name);
            const result = callback ? callback(name, arg) : "";
            this.callbackResult = result.length ? this.encoder.encode(result) : EMPTY;
            return this.callbackResult.length;
        },
        read_result: destPtr => {
            if (this.callbackResult.length) {
                this.view(destPtr, this.callbackResult.length).set(this.callbackResult);
            }
        },
    };

    /**
     * Binds an instantiated reactor and creates the API session. Must be called
     * exactly once, before any request.
     */
    bind(exports: WasmExports, cwd: string): void {
        this.exports = exports;
        const cwdBytes = this.encoder.encode(cwd);
        const ptr = exports.get_request_buffer(cwdBytes.length || 1);
        if (cwdBytes.length) {
            this.view(ptr, cwdBytes.length).set(cwdBytes);
        }
        exports.create_session(ptr, cwdBytes.length);
    }

    registerCallback(name: string, callback: (name: string, payload: string) => string): void {
        this.callbacks.set(name, callback);
    }

    requestSync(method: string, payload: string): string {
        const response = this.request(method, payload ? this.encoder.encode(payload) : EMPTY);
        return response.length ? this.decoder.decode(response) : "";
    }

    requestBinarySync(method: string, payload: Uint8Array): Uint8Array {
        return this.request(method, payload);
    }

    close(): void {
        this.exports = undefined;
        this.callbacks.clear();
    }

    private request(method: string, payload: Uint8Array): Uint8Array {
        const exports = this.exports;
        if (!exports) {
            throw new Error("WasmChannel is not bound to a module");
        }
        const methodBytes = this.encoder.encode(method);
        const size = methodBytes.length + payload.length;
        const ptr = exports.get_request_buffer(size || 1);
        const buffer = this.view(ptr, size);
        buffer.set(methodBytes, 0);
        buffer.set(payload, methodBytes.length);
        this.lastBytesSent = size;

        const status = exports.handle_request(methodBytes.length, payload.length);
        const response = this.view(exports.response_ptr(), exports.response_len()).slice();
        this.lastBytesReceived = response.length;
        if (status !== 0) {
            throw new Error(this.decoder.decode(response) || "tsgo-wasm request failed");
        }
        return response;
    }

    /**
     * A view over `len` bytes of linear memory at `ptr`. Always reads
     * `memory.buffer` afresh because it is detached whenever the module grows
     * its memory.
     */
    private view(ptr: number, len: number): Uint8Array {
        return new Uint8Array(this.exports!.memory.buffer, ptr, len);
    }

    private readString(ptr: number, len: number): string {
        return this.decoder.decode(this.view(ptr, len));
    }
}
