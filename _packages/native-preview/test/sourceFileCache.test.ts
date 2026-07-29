import assert from "node:assert";
import {
    describe,
    test,
} from "node:test";
import type { SnapshotChanges } from "../src/api/proto.ts";
import { SourceFileCache } from "../src/api/sourceFileCache.ts";
import type {
    Path,
    SourceFile,
} from "../src/ast/index.ts";

/**
 * A model of what the cache is supposed to answer, written the way the cache used to
 * work: every (snapshot, project) pair keeps the whole map of what it resolved, and a
 * new snapshot is given a copy of the previous one's minus the files that changed.
 *
 * The cache does that lazily so that the common case is a hand-over rather than a
 * copy, and every test below drives the two side by side and checks they agree.
 */
class Model {
    private scopes = new Map<number, Map<string, Map<string, string>>>();

    set(path: string, snapshotId: number, projectId: string, version: string): void {
        this.scopeFor(snapshotId, projectId).set(path, version);
    }

    retainForSnapshot(newSnapshotId: number, previousSnapshotId: number, changes: SnapshotChanges | undefined): void {
        const removed = new Set(changes?.removedProjects ?? []);
        const previous = this.scopes.get(previousSnapshotId);
        if (previous === undefined) return;
        for (const [projectId, scope] of previous) {
            if (removed.has(projectId)) continue;
            const projectChanges = changes?.changedProjects?.[projectId];
            const invalid = new Set([...projectChanges?.changedFiles ?? [], ...projectChanges?.deletedFiles ?? []]);
            const target = this.scopeFor(newSnapshotId, projectId);
            for (const [path, version] of scope) {
                if (invalid.has(path) || target.has(path)) continue;
                target.set(path, version);
            }
        }
    }

    releaseSnapshot(snapshotId: number): void {
        this.scopes.delete(snapshotId);
    }

    /** Every (path, snapshot, project) any scope has ever mentioned, and its answer. */
    *expectations(paths: readonly string[], snapshotIds: readonly number[], projectIds: readonly string[]): Generator<[string, number, string, string | undefined]> {
        for (const snapshotId of snapshotIds) {
            for (const projectId of projectIds) {
                for (const path of paths) {
                    yield [path, snapshotId, projectId, this.scopes.get(snapshotId)?.get(projectId)?.get(path)];
                }
            }
        }
    }

    private scopeFor(snapshotId: number, projectId: string): Map<string, string> {
        let projects = this.scopes.get(snapshotId);
        if (projects === undefined) {
            projects = new Map();
            this.scopes.set(snapshotId, projects);
        }
        let scope = projects.get(projectId);
        if (scope === undefined) {
            scope = new Map();
            projects.set(projectId, scope);
        }
        return scope;
    }
}

/** A stand-in for a decoded source file, identified by the version it was made from. */
function fileFor(path: string, version: string): SourceFile {
    return { fileName: path, version } as unknown as SourceFile;
}

class Harness {
    cache = new SourceFileCache();
    model = new Model();
    paths = new Set<string>();
    snapshots = new Set<number>();
    projects = new Set<string>();

    set(path: string, snapshotId: number, projectId: string, version: string): void {
        this.paths.add(path);
        this.snapshots.add(snapshotId);
        this.projects.add(projectId);
        this.cache.set(path as Path, fileFor(path, version), "opts", version, snapshotId, projectId);
        this.model.set(path, snapshotId, projectId, version);
    }

    retainForSnapshot(newSnapshotId: number, previousSnapshotId: number, changes?: SnapshotChanges): void {
        this.snapshots.add(newSnapshotId);
        this.cache.retainForSnapshot(newSnapshotId, previousSnapshotId, changes);
        this.model.retainForSnapshot(newSnapshotId, previousSnapshotId, changes);
    }

    releaseSnapshot(snapshotId: number): void {
        this.cache.releaseSnapshot(snapshotId);
        this.model.releaseSnapshot(snapshotId);
    }

    /** Checks every answer the cache can give against the one the model says it owes. */
    check(where: string): void {
        for (const [path, snapshotId, projectId, expected] of this.model.expectations([...this.paths], [...this.snapshots], [...this.projects])) {
            const actual = this.cache.getRetained(path as Path, snapshotId, projectId) as { version?: string; } | undefined;
            assert.strictEqual(
                actual?.version,
                expected,
                `${where}: ${path} for snapshot ${snapshotId} of ${projectId}`,
            );
        }
        this.checkAccounting(where);
    }

    /**
     * Checks the cache's own books: every entry a scope points at is in the cache,
     * every entry in the cache is pointed at, and each one's count is how many scopes
     * point at it. An entry counted short is evicted while something is still reading
     * it, and one counted long never leaves.
     */
    checkAccounting(where: string): void {
        const internals = this.cache as unknown as {
            cache: Map<string, { refCount: number; }[]>;
            scopes: Map<number, Map<string, Map<string, { refCount: number; }>>>;
        };
        const counted = new Map<object, number>();
        for (const projects of internals.scopes.values()) {
            for (const scope of projects.values()) {
                for (const [path, entry] of scope) {
                    assert.ok(internals.cache.get(path)?.includes(entry), `${where}: ${path} was evicted while a scope still pointed at it`);
                    counted.set(entry, (counted.get(entry) ?? 0) + 1);
                }
            }
        }
        for (const [path, entries] of internals.cache) {
            for (const entry of entries) {
                assert.strictEqual(entry.refCount, counted.get(entry) ?? 0, `${where}: ${path} is counted wrong`);
            }
        }
    }

    /** Releases everything and checks that nothing is left behind. */
    checkNothingLeaks(): void {
        for (const snapshotId of this.snapshots) this.releaseSnapshot(snapshotId);
        assert.strictEqual(this.cache.size, 0, "entries left after every snapshot was released");
    }
}

const changedIn = (projectId: string, ...files: string[]): SnapshotChanges => ({ changedProjects: { [projectId]: { changedFiles: files } } });

describe("SourceFileCache", () => {
    test("hands a scope over when the previous snapshot is released next", () => {
        const h = new Harness();
        h.set("/a.ts", 1, "p", "a1");
        h.set("/b.ts", 1, "p", "b1");
        h.retainForSnapshot(2, 1, changedIn("p", "/a.ts"));
        h.check("before the release");
        h.releaseSnapshot(1);
        h.check("after the release");
        assert.strictEqual(h.cache.getRetained("/b.ts" as Path, 2, "p"), h.cache.getRetained("/b.ts" as Path, 2, "p"));
        h.checkNothingLeaks();
    });

    test("copies a scope when the previous snapshot goes on being read", () => {
        const h = new Harness();
        h.set("/a.ts", 1, "p", "a1");
        h.set("/b.ts", 1, "p", "b1");
        h.retainForSnapshot(2, 1, changedIn("p", "/a.ts"));
        h.set("/a.ts", 2, "p", "a2");
        // snapshot 3 forces the pending retain to be copied rather than handed over
        h.retainForSnapshot(3, 2, changedIn("p", "/b.ts"));
        h.check("with three snapshots alive");
        h.releaseSnapshot(1);
        h.check("after the first is released");
        h.releaseSnapshot(2);
        h.check("after the second is released");
        h.checkNothingLeaks();
    });

    test("a copied scope leaves out a changed file nothing has fetched again", () => {
        const h = new Harness();
        h.set("/a.ts", 1, "p", "a1");
        h.set("/b.ts", 1, "p", "b1");
        h.retainForSnapshot(2, 1, changedIn("p", "/a.ts"));
        // snapshot 3 forces the copy while snapshot 2 has never asked for /a.ts, so
        // there is nothing standing in the way of the stale entry being carried over
        h.retainForSnapshot(3, 2, undefined);
        h.check("after the copy");
        assert.strictEqual(h.cache.getRetained("/a.ts" as Path, 2, "p"), undefined);
        assert.strictEqual(h.cache.getRetained("/a.ts" as Path, 3, "p"), undefined);
        h.checkNothingLeaks();
    });

    test("a snapshot released before the one it inherits from takes nothing with it", () => {
        const h = new Harness();
        h.set("/a.ts", 1, "p", "a1");
        h.retainForSnapshot(2, 1, undefined);
        h.set("/b.ts", 2, "p", "b2");
        h.releaseSnapshot(2);
        h.check("after the new snapshot went first");
        assert.strictEqual(h.cache.getRetained("/a.ts" as Path, 1, "p") !== undefined, true);
        h.checkNothingLeaks();
    });

    test("a removed project is not inherited", () => {
        const h = new Harness();
        h.set("/a.ts", 1, "p", "a1");
        h.set("/x.ts", 1, "q", "x1");
        h.retainForSnapshot(2, 1, { removedProjects: ["q"] });
        h.check("before the release");
        h.releaseSnapshot(1);
        h.check("after the release");
        assert.strictEqual(h.cache.has("/x.ts" as Path), false, "the removed project's file was evicted");
        h.checkNothingLeaks();
    });

    test("deleted files are invalidated like changed ones", () => {
        const h = new Harness();
        h.set("/a.ts", 1, "p", "a1");
        h.set("/b.ts", 1, "p", "b1");
        h.retainForSnapshot(2, 1, { changedProjects: { p: { deletedFiles: ["/b.ts"] } } });
        h.releaseSnapshot(1);
        h.check("after a deletion");
        assert.strictEqual(h.cache.has("/b.ts" as Path), false);
        h.checkNothingLeaks();
    });

    test("a file two snapshots share is kept until both let go", () => {
        const h = new Harness();
        h.set("/a.ts", 1, "p", "a1");
        h.retainForSnapshot(2, 1, undefined);
        h.releaseSnapshot(1);
        h.retainForSnapshot(3, 2, undefined);
        h.check("three generations on");
        h.releaseSnapshot(2);
        h.check("after the middle generation went");
        assert.strictEqual(h.cache.has("/a.ts" as Path), true);
        h.checkNothingLeaks();
    });

    test("agrees with the model over a long run of random operations", () => {
        const paths = ["/a.ts", "/b.ts", "/c.ts", "/d.ts"];
        const projects = ["p", "q"];
        let seed = 12345;
        // a small deterministic generator, so a failure is reproducible
        const random = (n: number) => {
            seed = (seed * 1103515245 + 12345) & 0x7fffffff;
            return seed % n;
        };

        for (let run = 0; run < 40; run++) {
            const h = new Harness();
            let latest = 1;
            const live = [1];
            for (const projectId of projects) {
                for (const path of paths) h.set(path, latest, projectId, `${path}@0`);
            }
            for (let step = 0; step < 30; step++) {
                switch (random(4)) {
                    case 0: {
                        // a new snapshot inheriting from the latest, with a few changes
                        const changed = paths.filter(() => random(3) === 0);
                        const projectId = projects[random(projects.length)];
                        const previous = latest;
                        latest += 1;
                        h.retainForSnapshot(latest, previous, changed.length > 0 ? changedIn(projectId, ...changed) : undefined);
                        live.push(latest);
                        // half the time the previous snapshot goes straight away,
                        // which is the hand-over, and half the time it stays and the
                        // new snapshot fetches for itself first, which is the copy
                        if (random(2) === 0) {
                            h.releaseSnapshot(previous);
                            live.splice(live.indexOf(previous), 1);
                        }
                        // some of the changed files are fetched again for the new
                        // snapshot and some are only asked for later, which is where
                        // an invalidation that did not take hold shows up
                        for (const path of changed) {
                            if (random(2) === 0) h.set(path, latest, projectId, `${path}@${latest}`);
                        }
                        break;
                    }
                    case 1: {
                        // release some snapshot other than the latest
                        const releasable = live.filter(id => id !== latest);
                        if (releasable.length === 0) break;
                        const id = releasable[random(releasable.length)];
                        h.releaseSnapshot(id);
                        live.splice(live.indexOf(id), 1);
                        break;
                    }
                    case 2: {
                        // the latest snapshot fetches a file for itself
                        h.set(paths[random(paths.length)], latest, projects[random(projects.length)], `fresh@${step}`);
                        break;
                    }
                    default:
                        h.check(`run ${run} step ${step}`);
                }
                h.check(`run ${run} step ${step}`);
            }
            h.checkNothingLeaks();
        }
    });
});
