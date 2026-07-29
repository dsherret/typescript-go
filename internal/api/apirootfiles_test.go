package api

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/microsoft/typescript-go/internal/bundled"
	"github.com/microsoft/typescript-go/internal/lsp/lsproto"
	"github.com/microsoft/typescript-go/internal/project"
	"github.com/microsoft/typescript-go/internal/testutil/projecttestutil"
	"gotest.tools/v3/assert"
)

// TestAPIRootsMatchAProjectOpenedWithThem runs every shape TestAddedRootsMatchAProjectOpenedWithThem
// does, with the roots named for the project over the API rather than written into its
// config. The addition has to be made — or refused — in exactly the same places: what
// decides that is the file the addition reaches, not how the client named it.
func TestAPIRootsMatchAProjectOpenedWithThem(t *testing.T) {
	t.Parallel()
	if !bundled.Embedded {
		t.Skip("bundled files are not embedded")
	}

	for _, testCase := range addRootsTestCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			runAddRootsTestCase(t, testCase, newAPIRootsSession, newAPIRootsSessionWithRoots)
		})
	}
}

// TestAPIRootsHoldTheSameFilesAsAConfigList checks the two ways of naming roots against
// each other: the same files, in the same order, with the same diagnostics. Only the
// reason a file is in the program differs, since a root the config named literally says
// so and one the client named is a plain root file.
func TestAPIRootsHoldTheSameFilesAsAConfigList(t *testing.T) {
	t.Parallel()
	if !bundled.Embedded {
		t.Skip("bundled files are not embedded")
	}

	files := map[string]string{
		"/p/a.ts":     `import { b } from "./b"; export const a = b;`,
		"/p/b.ts":     `export const b = 2;`,
		"/p/sub/c.ts": `import { a } from "../a"; export const c = a;`,
	}
	roots := []string{"/p/a.ts", "/p/b.ts", "/p/sub/c.ts"}

	viaConfig := newAddRootsSessionWithRoots(t, files, roots)
	defer viaConfig.close()
	viaAPI := newAPIRootsSessionWithRoots(t, files, roots)
	defer viaAPI.close()

	assert.DeepEqual(t, projectFileNames(t, viaAPI), projectFileNames(t, viaConfig))
	assert.Equal(t, programDiagnostics(t, viaAPI.program(t)), programDiagnostics(t, viaConfig.program(t)))
	assert.DeepEqual(t, viaAPI.program(t).GetSourceFileByPath("/p/a.ts").Text(), files["/p/a.ts"])
}

// TestAPIRootsAreKeptWhenTheConfigIsRewritten is the trap in this design: changing a
// compiler option rewrites the config, which re-parses it and hands the project a new
// command line. The roots the client named belong to the project rather than to its
// config, so they have to survive that — a project that dropped them would silently
// empty out.
func TestAPIRootsAreKeptWhenTheConfigIsRewritten(t *testing.T) {
	t.Parallel()
	if !bundled.Embedded {
		t.Skip("bundled files are not embedded")
	}

	s := newAPIRootsSession(t, map[string]string{
		"/p/a.ts": `export const a = 1;`,
		"/p/b.ts": `import { a } from "./a"; export const b = a;`,
	})
	defer s.close()
	assert.DeepEqual(t, projectFileNames(t, s), []string{"/p/a.ts", "/p/b.ts"})

	text, _ := json.Marshal(map[string]any{
		"compilerOptions": map[string]any{"allowJs": true, "strict": true},
		"files":           []string{},
	})
	s.write(addRootsConfigFileName, string(text))
	s.update(t, &APIFileChanges{Changed: identifiers(addRootsConfigFileName)})

	assert.DeepEqual(t, projectFileNames(t, s), []string{"/p/a.ts", "/p/b.ts"})
	assert.Assert(t, s.program(t).Options().Strict.IsTrue())
}

// TestAPIRootsCanBeRemoved checks that a root the client drops leaves the program, and
// that the ones around it stay where they were.
func TestAPIRootsCanBeRemoved(t *testing.T) {
	t.Parallel()
	if !bundled.Embedded {
		t.Skip("bundled files are not embedded")
	}

	s := newAPIRootsSession(t, map[string]string{
		"/p/a.ts": `export const a = 1;`,
		"/p/b.ts": `export const b = 2;`,
		"/p/c.ts": `export const c = 3;`,
	})
	defer s.close()

	s.removedRoots = []string{"/p/b.ts"}
	s.roots = slices.DeleteFunc(s.roots, func(name string) bool { return name == "/p/b.ts" })
	s.update(t, nil)

	assert.DeepEqual(t, projectFileNames(t, s), []string{"/p/a.ts", "/p/c.ts"})

	// and comes back where a root added last belongs
	s.pendingRoots = []string{"/p/b.ts"}
	s.update(t, nil)
	assert.DeepEqual(t, projectFileNames(t, s), []string{"/p/a.ts", "/p/c.ts", "/p/b.ts"})
}

// TestAPIRootsReportTheConfigsOwnDiagnostics checks that the option validation a config
// file does is still done and still reported. It is the reason the project keeps a
// config at all rather than being handed an options object.
func TestAPIRootsReportTheConfigsOwnDiagnostics(t *testing.T) {
	t.Parallel()
	if !bundled.Embedded {
		t.Skip("bundled files are not embedded")
	}

	text, _ := json.Marshal(map[string]any{
		"compilerOptions": map[string]any{"allowJs": true, "lib": []string{"not-a-lib"}},
		"files":           []string{},
	})
	s := newAPIRootsSessionWithConfig(t, map[string]string{"/p/a.ts": `export const a = 1;`}, string(text))
	defer s.close()

	var codes []int32
	for _, d := range s.program(t).GetConfigFileParsingDiagnostics() {
		codes = append(codes, d.Code())
	}
	// 6046: the argument for --lib must be one of the known libraries
	assert.Assert(t, slices.Contains(codes, int32(6046)), "config file parsing diagnostics: %v", codes)
	// 18002: the `files` list is empty. It is, in the config — but not in the command
	// line the program was built from, which is what this reports on, and leaving it in
	// would stop a project with noEmitOnError from ever emitting.
	assert.Assert(t, !slices.Contains(codes, int32(18002)), "config file parsing diagnostics: %v", codes)
}

// TestAPIRootsKeepFilesThatShareAStem is ts-morph's contract, which a wildcard config
// cannot keep: a file the caller named is in the project whatever else shares its stem.
// Roots named over the API never pass through the include globs or the extension
// priority that decides what a wildcard matches, so every one of these is a root.
func TestAPIRootsKeepFilesThatShareAStem(t *testing.T) {
	t.Parallel()
	if !bundled.Embedded {
		t.Skip("bundled files are not embedded")
	}

	files := map[string]string{
		"/p/a.ts":   `export const a = 1;`,
		"/p/a.d.ts": `export declare const a: number;`,
		"/p/a.js":   `export const a = 1;`,
		"/p/b.tsx":  `export const b = 1;`,
		"/p/b.jsx":  `export const b = 1;`,
		"/p/c.mts":  `export const c = 1;`,
		"/p/c.mjs":  `export const c = 1;`,
		"/p/d.cts":  `export const d = 1;`,
		"/p/d.cjs":  `export const d = 1;`,
	}
	// added one at a time, which is the path a create loop takes
	s := newAPIRootsSession(t, map[string]string{"/p/seed.ts": `export const seed = 1;`})
	defer s.close()
	for _, name := range sortedRootNames(files) {
		s.addRoots(t, map[string]string{name: files[name]})
	}

	held := projectFileNames(t, s)
	for name := range files {
		assert.Assert(t, slices.Contains(held, name), "%s is in the project: %v", name, held)
	}
}

// TestAPIRootsAreReportedByGetProjectRootFiles checks the one place the list is still
// readable: a project reports the roots it was asked to hold, whichever way they were
// named.
func TestAPIRootsAreReportedByGetProjectRootFiles(t *testing.T) {
	t.Parallel()
	if !bundled.Embedded {
		t.Skip("bundled files are not embedded")
	}

	s := newAPIRootsSession(t, map[string]string{
		"/p/a.ts": `export const a = 1;`,
		"/p/b.ts": `export const b = 2;`,
	})
	defer s.close()

	proj := s.project.Snapshot().ProjectCollection.ConfiguredProject(addRootsConfigFileName)
	assert.Assert(t, proj != nil)
	assert.DeepEqual(t, proj.RootFileNames(), []string{"/p/a.ts", "/p/b.ts"})
}

// newAPIRootsSessionWithConfig is newAPIRootsSession with a config the caller wrote.
func newAPIRootsSessionWithConfig(t *testing.T, files map[string]string, configText string) *addRootsSession {
	t.Helper()
	initial := map[string]any{addRootsConfigFileName: configText}
	for name, text := range files {
		initial[name] = text
	}
	s := &addRootsSession{roots: sortedRootNames(files), rootsViaAPI: true}
	s.pendingRoots = slices.Clone(s.roots)
	s.project, s.utils = projecttestutil.SetupWithOptions(initial, &project.SessionOptions{
		CurrentDirectory:   "/",
		DefaultLibraryPath: bundled.LibPath(),
		PositionEncoding:   lsproto.PositionEncodingKindUTF8,
	})
	s.session = NewSession(s.project, nil)
	s.update(t, nil)
	return s
}

// projectFileNames are the files the project holds that the test put there, in the
// order the program holds them.
func projectFileNames(t *testing.T, s *addRootsSession) []string {
	t.Helper()
	var names []string
	for _, file := range s.program(t).GetSourceFiles() {
		if strings.HasPrefix(file.FileName(), "/p/") && file.FileName() != addRootsConfigFileName {
			names = append(names, file.FileName())
		}
	}
	return names
}
