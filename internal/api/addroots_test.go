package api

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/microsoft/typescript-go/internal/bundled"
	"github.com/microsoft/typescript-go/internal/compiler"
	"github.com/microsoft/typescript-go/internal/locale"
	"github.com/microsoft/typescript-go/internal/lsp/lsproto"
	"github.com/microsoft/typescript-go/internal/project"
	"github.com/microsoft/typescript-go/internal/testutil/projecttestutil"
	"github.com/microsoft/typescript-go/internal/tspath"
	"gotest.tools/v3/assert"
)

const addRootsConfigFileName = "/p/tsconfig.json"

// TestAddedRootsMatchAProjectOpenedWithThem drives the session the way ts-morph does
// — write a file, rewrite the config naming every file, ask for a snapshot — and
// checks the project against one opened with every file already named. Whether the
// program was added to or built again, the two have to say the same things.
//
// The case that has to be got right is a file something was already looking for: the
// addition cannot be made then, because the file that was looking resolves elsewhere
// now, and only building the program again finds that out.
func TestAddedRootsMatchAProjectOpenedWithThem(t *testing.T) {
	t.Parallel()
	if !bundled.Embedded {
		t.Skip("bundled files are not embedded")
	}

	testCases := []struct {
		name    string
		initial map[string]string
		added   []map[string]string
		// reusesProgram is whether the last addition was made without rebuilding.
		reusesProgram bool
	}{
		{
			name:          "a file nothing was waiting for",
			initial:       map[string]string{"/p/a.ts": `export const a = 1;`},
			added:         []map[string]string{{"/p/b.ts": `export const b = 2;`}},
			reusesProgram: true,
		},
		{
			name:          "a file an import was waiting for",
			initial:       map[string]string{"/p/a.ts": `import { b } from "./b"; export const a = b;`},
			added:         []map[string]string{{"/p/b.ts": `export const b = 2;`}},
			reusesProgram: false,
		},
		{
			name:    "a file a triple slash reference was waiting for",
			initial: map[string]string{"/p/a.ts": "/// <reference path=\"./b.ts\" />\nexport const a = 1;"},
			added:   []map[string]string{{"/p/b.ts": `declare const b: number;`}},
			// the reference probed for the file, so the addition has to be refused
			reusesProgram: false,
		},
		{
			name:          "a file that imports one already in the project",
			initial:       map[string]string{"/p/a.ts": `export const a = 1;`},
			added:         []map[string]string{{"/p/b.ts": `import { a } from "./a"; export const b = a;`}},
			reusesProgram: true,
		},
		{
			name:    "several files in a row, each importing the last",
			initial: map[string]string{"/p/a.ts": `export const a = 1;`},
			added: []map[string]string{
				{"/p/b.ts": `import { a } from "./a"; export const b = a;`},
				{"/p/c.ts": `import { b } from "./b"; export const c = b;`},
				{"/p/d.ts": `import { c } from "./c"; export const d = c;`},
			},
			reusesProgram: true,
		},
		{
			name:          "a declaration file beside a source file",
			initial:       map[string]string{"/p/a.ts": `export const a = 1;`},
			added:         []map[string]string{{"/p/a.d.ts": `export declare const a: number;`}},
			reusesProgram: true,
		},
		{
			name:          "a file in a directory the project has not seen",
			initial:       map[string]string{"/p/a.ts": `export const a = 1;`},
			added:         []map[string]string{{"/p/sub/b.ts": `export const b = 2;`}},
			reusesProgram: true,
		},
		{
			name:          "a file with an import that resolves to nothing",
			initial:       map[string]string{"/p/a.ts": `export const a = 1;`},
			added:         []map[string]string{{"/p/b.ts": `import { z } from "./z"; export const b = z;`}},
			reusesProgram: true,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			incremental := newAddRootsSession(t, testCase.initial)
			defer incremental.close()
			for _, batch := range testCase.added {
				incremental.addRoots(t, batch)
			}
			assert.Equal(t, incremental.reusedProgram, testCase.reusesProgram, "reused the program")

			all := map[string]string{}
			for name, text := range testCase.initial {
				all[name] = text
			}
			for _, batch := range testCase.added {
				for name, text := range batch {
					all[name] = text
				}
			}
			// the same roots in the same order, since the order of the list is what
			// decides the order of the files in the program
			atOnce := newAddRootsSessionWithRoots(t, all, incremental.roots)
			defer atOnce.close()

			assert.Equal(t, explainProgram(incremental.program(t)), explainProgram(atOnce.program(t)), "explained files")
			assert.Equal(t, programDiagnostics(t, incremental.program(t)), programDiagnostics(t, atOnce.program(t)), "diagnostics")
		})
	}
}

// TestAddedRootWithAnEditedFile is the shape ts-morph's create-and-manipulate loop
// makes: a new root arrives in the same snapshot as an edit to a file already there.
func TestAddedRootWithAnEditedFile(t *testing.T) {
	t.Parallel()
	if !bundled.Embedded {
		t.Skip("bundled files are not embedded")
	}

	edited := "export const a = 1;\n\nclass C {\n}\n"
	session := newAddRootsSession(t, map[string]string{"/p/a.ts": `export const a = 1;`})
	defer session.close()
	session.write("/p/a.ts", edited)
	session.addRoots(t, map[string]string{"/p/b.ts": `export const b = 2;`}, "/p/a.ts")
	assert.Assert(t, session.reusedProgram)

	program := session.program(t)
	assert.Equal(t, program.GetSourceFileByPath("/p/a.ts").Text(), edited)

	atOnce := newAddRootsSessionWithRoots(t, map[string]string{"/p/a.ts": edited, "/p/b.ts": `export const b = 2;`}, []string{"/p/a.ts", "/p/b.ts"})
	defer atOnce.close()
	assert.Equal(t, explainProgram(program), explainProgram(atOnce.program(t)))
	assert.Equal(t, programDiagnostics(t, program), programDiagnostics(t, atOnce.program(t)))
}

// TestAddedRootWithADeletedFile checks that a deletion arriving with an addition
// rebuilds, since what the deleted file was answering for has to be worked out again.
func TestAddedRootWithADeletedFile(t *testing.T) {
	t.Parallel()
	if !bundled.Embedded {
		t.Skip("bundled files are not embedded")
	}

	session := newAddRootsSession(t, map[string]string{
		"/p/a.ts":   `import { dep } from "./dep"; export const a = dep;`,
		"/p/dep.ts": `export const dep = 3;`,
	})
	defer session.close()

	_ = session.utils.FS().Remove("/p/dep.ts")
	session.roots = slices.DeleteFunc(session.roots, func(name string) bool { return name == "/p/dep.ts" })
	session.write("/p/b.ts", `export const b = 2;`)
	session.roots = append(session.roots, "/p/b.ts")
	session.writeConfig()
	session.update(t, &APIFileChanges{
		Created: identifiers("/p/b.ts"),
		Changed: identifiers(addRootsConfigFileName),
		Deleted: identifiers("/p/dep.ts"),
	})
	assert.Assert(t, !session.reusedProgram)
	assert.Assert(t, session.program(t).GetSourceFileByPath("/p/dep.ts") == nil)
}

type addRootsSession struct {
	session       *Session
	project       *project.Session
	utils         *projecttestutil.SessionUtils
	roots         []string
	reusedProgram bool
	lastReasons   map[tspath.Path][]*compiler.FileIncludeReason
}

func newAddRootsSession(t *testing.T, files map[string]string) *addRootsSession {
	t.Helper()
	roots := make([]string, 0, len(files))
	for name := range files {
		roots = append(roots, name)
	}
	slices.Sort(roots)
	return newAddRootsSessionWithRoots(t, files, roots)
}

func newAddRootsSessionWithRoots(t *testing.T, files map[string]string, roots []string) *addRootsSession {
	t.Helper()
	initial := map[string]any{}
	for name, text := range files {
		initial[name] = text
	}
	s := &addRootsSession{roots: slices.Clone(roots)}
	initial[addRootsConfigFileName] = addRootsConfigText(s.roots)
	s.project, s.utils = projecttestutil.SetupWithOptions(initial, &project.SessionOptions{
		CurrentDirectory:   "/",
		DefaultLibraryPath: bundled.LibPath(),
		PositionEncoding:   lsproto.PositionEncodingKindUTF8,
	})
	s.session = NewSession(s.project, nil)
	s.update(t, nil)
	return s
}

func addRootsConfigText(roots []string) string {
	text, _ := json.Marshal(map[string]any{
		"compilerOptions": map[string]any{"allowJs": true},
		"files":           roots,
	})
	return string(text)
}

func (s *addRootsSession) close() {
	s.session.Close()
	s.project.Close()
}

func (s *addRootsSession) write(fileName string, text string) {
	_ = s.utils.FS().WriteFile(fileName, text)
}

func (s *addRootsSession) writeConfig() {
	s.write(addRootsConfigFileName, addRootsConfigText(s.roots))
}

// addRoots writes each file, names it in the config, and asks for a snapshot, which
// is exactly what ts-morph's document registry does when a file is created.
func (s *addRootsSession) addRoots(t *testing.T, files map[string]string, alsoChanged ...string) {
	t.Helper()
	created := make([]string, 0, len(files))
	for name, text := range files {
		s.write(name, text)
		s.roots = append(s.roots, name)
		created = append(created, name)
	}
	slices.Sort(created)
	s.writeConfig()
	s.update(t, &APIFileChanges{
		Created: identifiers(created...),
		Changed: identifiers(append([]string{addRootsConfigFileName}, alsoChanged...)...),
	})
}

func (s *addRootsSession) update(t *testing.T, changes *APIFileChanges) {
	t.Helper()
	before := s.lastReasons
	_, err := s.session.handleUpdateSnapshot(context.Background(), &UpdateSnapshotParams{
		FileChanges:  changes,
		OpenProjects: []DocumentIdentifier{{FileName: addRootsConfigFileName}},
	})
	assert.NilError(t, err)
	reasons := s.program(t).GetIncludeReasons()
	// a program that was added to carries the reasons the one before it worked out;
	// one built again works them all out afresh, so none of the objects are shared
	s.reusedProgram = false
	for path, reason := range before {
		if now, ok := reasons[path]; ok && len(now) > 0 && len(reason) > 0 && now[0] == reason[0] {
			s.reusedProgram = true
			break
		}
	}
	s.lastReasons = reasons
}

func (s *addRootsSession) program(t *testing.T) *compiler.Program {
	t.Helper()
	configured := s.project.Snapshot().ProjectCollection.ConfiguredProject(tspath.Path(addRootsConfigFileName))
	assert.Assert(t, configured != nil)
	return configured.GetProgram()
}

func identifiers(fileNames ...string) []DocumentIdentifier {
	identifiers := make([]DocumentIdentifier, 0, len(fileNames))
	for _, fileName := range fileNames {
		identifiers = append(identifiers, DocumentIdentifier{FileName: fileName})
	}
	return identifiers
}

func explainProgram(program *compiler.Program) string {
	var b strings.Builder
	program.ExplainFiles(&b, locale.Default)
	return b.String()
}

func programDiagnostics(t *testing.T, program *compiler.Program) string {
	t.Helper()
	var lines []string
	for _, file := range program.GetSourceFiles() {
		if program.IsSourceFileDefaultLibrary(file.Path()) {
			continue
		}
		for _, diagnostic := range program.GetSemanticDiagnostics(context.Background(), file) {
			lines = append(lines, fmt.Sprintf("%s(%d): %s", file.FileName(), diagnostic.Pos(), diagnostic.Localize(locale.Default)))
		}
		for _, diagnostic := range program.GetIncludeProcessorDiagnostics(file) {
			lines = append(lines, fmt.Sprintf("%s(%d) include: %s", file.FileName(), diagnostic.Pos(), diagnostic.Localize(locale.Default)))
		}
	}
	for _, diagnostic := range program.GetProgramDiagnostics() {
		lines = append(lines, fmt.Sprintf("program: %s", diagnostic.Localize(locale.Default)))
	}
	slices.Sort(lines)
	return strings.Join(lines, "\n")
}
