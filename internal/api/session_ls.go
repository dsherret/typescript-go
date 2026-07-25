package api

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/microsoft/typescript-go/internal/ast"
	"github.com/microsoft/typescript-go/internal/compiler"
	"github.com/microsoft/typescript-go/internal/core"
	"github.com/microsoft/typescript-go/internal/ls"
	"github.com/microsoft/typescript-go/internal/lsp/lsproto"
	"github.com/microsoft/typescript-go/internal/project"
)

// Language service handlers.
//
// The editing operations below (formatting, organize imports) are implemented
// in internal/ls for the LSP server. They are exposed here so API clients —
// which work in character offsets rather than LSP positions, and drive edits
// programmatically rather than through an editor — can use them too. Results are
// converted to the API's offset-based TextEdit.

// handleFormatDocument returns the edits that format an entire file.
func (s *Session) handleFormatDocument(ctx context.Context, params *FormatDocumentParams) ([]*TextEdit, error) {
	setup, err := s.setupLanguageServiceForFile(ctx, params.Snapshot, params.Project, params.File, false)
	if err != nil {
		return nil, err
	}
	defer setup.done()

	response, err := setup.langSvc.ProvideFormatDocument(ctx, setup.documentURI, toLSPFormattingOptions(params.Options))
	if err != nil {
		return nil, err
	}
	return s.toAPIEditsFromResponse(setup, response), nil
}

// handleFormatDocumentRange returns the edits that format a span of a file.
func (s *Session) handleFormatDocumentRange(ctx context.Context, params *FormatDocumentRangeParams) ([]*TextEdit, error) {
	setup, err := s.setupLanguageServiceForFile(ctx, params.Snapshot, params.Project, params.File, false)
	if err != nil {
		return nil, err
	}
	defer setup.done()

	converters := setup.snapshot.Converters()
	positionMap := setup.sourceFile.GetPositionMap()
	lspRange := converters.ToLSPRange(setup.sourceFile, core.NewTextRange(
		positionMap.UTF16ToUTF8(params.Pos),
		positionMap.UTF16ToUTF8(params.End),
	))

	response, err := setup.langSvc.ProvideFormatDocumentRange(ctx, setup.documentURI, toLSPFormattingOptions(params.Options), lspRange)
	if err != nil {
		return nil, err
	}
	return s.toAPIEditsFromResponse(setup, response), nil
}

// handleOrganizeImports returns the edits that sort, combine, and/or remove
// unused imports in a file, according to the requested mode.
func (s *Session) handleOrganizeImports(ctx context.Context, params *OrganizeImportsParams) ([]*TextEdit, error) {
	setup, err := s.setupLanguageServiceForFile(ctx, params.Snapshot, params.Project, params.File, false)
	if err != nil {
		return nil, err
	}
	defer setup.done()

	kind := lsproto.CodeActionKindSourceOrganizeImports
	switch params.Mode {
	case "", OrganizeImportsModeAll:
		kind = lsproto.CodeActionKindSourceOrganizeImports
	case OrganizeImportsModeSortAndCombine:
		kind = lsproto.CodeActionKindSourceSortImports
	case OrganizeImportsModeRemoveUnused:
		kind = lsproto.CodeActionKindSourceRemoveUnusedImports
	default:
		return nil, fmt.Errorf("%w: unknown organize imports mode %q", ErrClientError, params.Mode)
	}

	editsByFile := setup.langSvc.OrganizeImports(ctx, setup.sourceFile, setup.program, kind)
	return toAPITextEdits(setup.sourceFile, setup.snapshot.Converters(), editsByFile[setup.sourceFile.FileName()]), nil
}

// handleRename returns the edits that rename the symbol at a position, grouped
// by file. An empty result means the element cannot be renamed.
func (s *Session) handleRename(ctx context.Context, params *RenameParams) ([]*FileTextEdits, error) {
	setup, err := s.setupLanguageServiceForFile(ctx, params.Snapshot, params.Project, params.File, false)
	if err != nil {
		return nil, err
	}
	defer setup.done()

	converters := setup.snapshot.Converters()

	// A nil orchestrator selects the single-project path: renames are resolved
	// against this project's program only, which is the API's model.
	response, err := setup.langSvc.ProvideRename(ctx, &lsproto.RenameParams{
		TextDocument: lsproto.TextDocumentIdentifier{Uri: setup.documentURI},
		Position:     setup.toLSPPosition(params.Position),
		NewName:      params.NewName,
	}, nil)
	if err != nil {
		return nil, err
	}
	if response.WorkspaceEdit == nil || response.WorkspaceEdit.Changes == nil {
		return []*FileTextEdits{}, nil
	}

	result := make([]*FileTextEdits, 0, len(*response.WorkspaceEdit.Changes))
	for uri, edits := range *response.WorkspaceEdit.Changes {
		fileName := uri.FileName()
		sourceFile := setup.program.GetSourceFile(fileName)
		if sourceFile == nil {
			continue
		}
		result = append(result, &FileTextEdits{
			FileName: fileName,
			Edits:    toAPITextEdits(sourceFile, converters, edits),
		})
	}
	// The wire order of a map is unspecified; sort so results are deterministic.
	slices.SortFunc(result, func(a, b *FileTextEdits) int {
		return strings.Compare(a.FileName, b.FileName)
	})
	return result, nil
}

// handleGetDefinition returns the locations that define the symbol at a position.
func (s *Session) handleGetDefinition(ctx context.Context, params *FilePositionParams) ([]*FileSpan, error) {
	setup, err := s.setupLanguageServiceForFile(ctx, params.Snapshot, params.Project, params.File, false)
	if err != nil {
		return nil, err
	}
	defer setup.done()

	response, err := setup.langSvc.ProvideDefinition(ctx, setup.documentURI, setup.toLSPPosition(params.Position))
	if err != nil {
		return nil, err
	}
	return setup.toAPIFileSpans(response), nil
}

// handleGetImplementations returns the locations that implement the symbol at a
// position.
func (s *Session) handleGetImplementations(ctx context.Context, params *FilePositionParams) ([]*FileSpan, error) {
	setup, err := s.setupLanguageServiceForFile(ctx, params.Snapshot, params.Project, params.File, false)
	if err != nil {
		return nil, err
	}
	defer setup.done()

	// As with rename, a nil orchestrator selects the single-project path.
	response, err := setup.langSvc.ProvideImplementations(ctx, &lsproto.ImplementationParams{
		TextDocument: lsproto.TextDocumentIdentifier{Uri: setup.documentURI},
		Position:     setup.toLSPPosition(params.Position),
	}, nil)
	if err != nil {
		return nil, err
	}
	return setup.toAPIFileSpans(response), nil
}

// handleGetCodeFixes returns the quick fixes available for a span of a file.
//
// The LSP model derives fixes from diagnostics the client already holds, while
// API clients ask by error code, so the diagnostics are computed here, filtered
// to the requested span (and error codes, when given), and handed back as the
// code action context.
func (s *Session) handleGetCodeFixes(ctx context.Context, params *GetCodeFixesParams) ([]*CodeFixAction, error) {
	setup, err := s.setupLanguageServiceForFile(ctx, params.Snapshot, params.Project, params.File, true)
	if err != nil {
		return nil, err
	}
	defer setup.done()

	diagnosticsResponse, err := setup.langSvc.ProvideDiagnostics(ctx, setup.documentURI)
	if err != nil {
		return nil, err
	}
	if diagnosticsResponse.FullDocumentDiagnosticReport == nil {
		return []*CodeFixAction{}, nil
	}

	start, end := params.Pos, params.End
	if end < start {
		start, end = end, start
	}
	converters := setup.snapshot.Converters()
	positionMap := setup.sourceFile.GetPositionMap()

	var relevant []*lsproto.Diagnostic
	for _, diagnostic := range diagnosticsResponse.FullDocumentDiagnosticReport.Items {
		if diagnostic == nil {
			continue
		}
		if len(params.ErrorCodes) > 0 {
			if diagnostic.Code == nil || diagnostic.Code.Integer == nil ||
				!slices.Contains(params.ErrorCodes, int(*diagnostic.Code.Integer)) {
				continue
			}
		}
		diagStart := positionMap.UTF8ToUTF16(int(converters.LineAndCharacterToPosition(setup.sourceFile, diagnostic.Range.Start)))
		diagEnd := positionMap.UTF8ToUTF16(int(converters.LineAndCharacterToPosition(setup.sourceFile, diagnostic.Range.End)))
		if diagEnd < start || diagStart > end {
			continue
		}
		relevant = append(relevant, diagnostic)
	}
	if len(relevant) == 0 {
		return []*CodeFixAction{}, nil
	}

	only := []lsproto.CodeActionKind{lsproto.CodeActionKindQuickFix}
	response, err := setup.langSvc.ProvideCodeActions(ctx, &lsproto.CodeActionParams{
		TextDocument: lsproto.TextDocumentIdentifier{Uri: setup.documentURI},
		Range: converters.ToLSPRange(setup.sourceFile, core.NewTextRange(
			positionMap.UTF16ToUTF8(start),
			positionMap.UTF16ToUTF8(end),
		)),
		Context: &lsproto.CodeActionContext{Diagnostics: relevant, Only: &only},
	})
	if err != nil {
		return nil, err
	}
	if response.CommandOrCodeActionArray == nil {
		return []*CodeFixAction{}, nil
	}

	result := make([]*CodeFixAction, 0, len(*response.CommandOrCodeActionArray))
	for _, entry := range *response.CommandOrCodeActionArray {
		action := entry.CodeAction
		if action == nil || action.Edit == nil || action.Edit.Changes == nil {
			continue
		}
		fix := &CodeFixAction{Description: action.Title}
		for uri, edits := range *action.Edit.Changes {
			fileName := uri.FileName()
			sourceFile := setup.program.GetSourceFile(fileName)
			if sourceFile == nil {
				continue
			}
			fix.Changes = append(fix.Changes, &FileTextEdits{
				FileName: fileName,
				Edits:    toAPITextEdits(sourceFile, converters, edits),
			})
		}
		slices.SortFunc(fix.Changes, func(a, b *FileTextEdits) int {
			return strings.Compare(a.FileName, b.FileName)
		})
		if len(fix.Changes) > 0 {
			result = append(result, fix)
		}
	}
	return result, nil
}

// languageServiceSetup bundles the state a language service handler needs: the
// resolved snapshot/program/file plus the service itself.
type languageServiceSetup struct {
	sd          *snapshotData
	snapshot    *project.Snapshot
	program     *compiler.Program
	sourceFile  *ast.SourceFile
	langSvc     *ls.LanguageService
	documentURI lsproto.DocumentUri
	done        func()
}

// setupLanguageServiceForFile resolves a snapshot, project, and file, and builds
// a language service scoped to that file.
//
// Fixes that add imports need the snapshot's auto-import registry prepared for
// the file; autoImports requests that, at the cost of building the registry.
// Callers must call done() to release the prepared snapshot.
func (s *Session) setupLanguageServiceForFile(ctx context.Context, snapshot SnapshotID, project ProjectID, file DocumentIdentifier, autoImports bool) (*languageServiceSetup, error) {
	sd, err := s.getSnapshotData(snapshot)
	if err != nil {
		return nil, err
	}
	program, err := sd.getProgram(project)
	if err != nil {
		return nil, err
	}
	fileName := file.ToFileName()
	sourceFile := program.GetSourceFile(fileName)
	if sourceFile == nil {
		return nil, fmt.Errorf("%w: source file not found: %v", ErrClientError, file)
	}

	documentURI := file.ToURI(s.projectSession.GetCurrentDirectory())
	projectPath := parseProjectHandle(project)
	workingSnapshot := sd.snapshot
	done := func() {}

	if autoImports {
		if registry := workingSnapshot.AutoImportRegistry(); registry == nil ||
			!registry.IsPreparedForImportingFile(sourceFile.FileName(), projectPath, workingSnapshot.UserPreferences()) {
			prepared := s.projectSession.GetSnapshotWithAutoImports(ctx, workingSnapshot, documentURI)
			done = func() { prepared.Deref(s.projectSession) }
			workingSnapshot = prepared

			proj := workingSnapshot.ProjectCollection.GetProjectByPath(projectPath)
			if proj == nil {
				done()
				return nil, fmt.Errorf("%w: project %s not found", ErrClientError, projectPath)
			}
			program = proj.GetProgram()
			if program == nil {
				done()
				return nil, fmt.Errorf("%w: project has no program", ErrClientError)
			}
			sourceFile = program.GetSourceFile(fileName)
			if sourceFile == nil {
				done()
				return nil, fmt.Errorf("%w: source file not found: %v", ErrClientError, file)
			}
		}
	}

	proj := workingSnapshot.ProjectCollection.GetProjectByPath(projectPath)
	if proj == nil {
		done()
		return nil, fmt.Errorf("%w: project %s not found", ErrClientError, projectPath)
	}

	return &languageServiceSetup{
		sd:          sd,
		snapshot:    workingSnapshot,
		program:     program,
		sourceFile:  sourceFile,
		langSvc:     ls.NewLanguageService(proj.ID(), program, workingSnapshot, fileName),
		documentURI: documentURI,
		done:        done,
	}, nil
}

// toLSPPosition converts a character offset in the setup's file to an LSP position.
func (setup *languageServiceSetup) toLSPPosition(position int) lsproto.Position {
	return setup.snapshot.Converters().PositionToLineAndCharacter(
		setup.sourceFile,
		core.TextPos(setup.sourceFile.GetPositionMap().UTF16ToUTF8(position)),
	)
}

// toAPIFileSpans flattens an LSP location response into offset-based spans.
// Definition-style responses are a union of one location, many locations, or
// links, so all three shapes are normalized here.
func (setup *languageServiceSetup) toAPIFileSpans(response lsproto.LocationOrLocationsOrDefinitionLinksOrNull) []*FileSpan {
	var locations []lsproto.Location
	switch {
	case response.Location != nil:
		locations = []lsproto.Location{*response.Location}
	case response.Locations != nil:
		locations = *response.Locations
	case response.DefinitionLinks != nil:
		for _, link := range *response.DefinitionLinks {
			if link == nil {
				continue
			}
			locations = append(locations, lsproto.Location{Uri: link.TargetUri, Range: link.TargetSelectionRange})
		}
	}

	converters := setup.snapshot.Converters()
	result := make([]*FileSpan, 0, len(locations))
	for _, location := range locations {
		fileName := location.Uri.FileName()
		sourceFile := setup.program.GetSourceFile(fileName)
		if sourceFile == nil {
			continue
		}
		positionMap := sourceFile.GetPositionMap()
		result = append(result, &FileSpan{
			FileName: fileName,
			Pos:      positionMap.UTF8ToUTF16(int(converters.LineAndCharacterToPosition(sourceFile, location.Range.Start))),
			End:      positionMap.UTF8ToUTF16(int(converters.LineAndCharacterToPosition(sourceFile, location.Range.End))),
		})
	}
	return result
}

// toAPIEditsFromResponse converts an LSP formatting response into API edits.
func (s *Session) toAPIEditsFromResponse(setup *languageServiceSetup, response lsproto.TextEditsOrNull) []*TextEdit {
	if response.TextEdits == nil {
		return []*TextEdit{}
	}
	return toAPITextEdits(setup.sourceFile, setup.snapshot.Converters(), *response.TextEdits)
}

// toLSPFormattingOptions converts the API's formatting options into the LSP
// shape. The result is never nil: the formatter dereferences it, and omitted
// fields fall back to the defaults below.
func toLSPFormattingOptions(options *FormattingOptions) *lsproto.FormattingOptions {
	result := &lsproto.FormattingOptions{
		TabSize:      4,
		InsertSpaces: true,
	}
	if options == nil {
		return result
	}
	if options.TabSize != nil {
		result.TabSize = uint32(*options.TabSize)
	}
	if options.InsertSpaces != nil {
		result.InsertSpaces = *options.InsertSpaces
	}
	if options.TrimTrailingWhitespace != nil {
		result.TrimTrailingWhitespace = options.TrimTrailingWhitespace
	}
	return result
}
