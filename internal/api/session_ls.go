package api

import (
	"context"
	"fmt"

	"github.com/microsoft/typescript-go/internal/ast"
	"github.com/microsoft/typescript-go/internal/compiler"
	"github.com/microsoft/typescript-go/internal/core"
	"github.com/microsoft/typescript-go/internal/ls"
	"github.com/microsoft/typescript-go/internal/lsp/lsproto"
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
	setup, err := s.setupLanguageServiceForFile(ctx, params.Snapshot, params.Project, params.File)
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
	setup, err := s.setupLanguageServiceForFile(ctx, params.Snapshot, params.Project, params.File)
	if err != nil {
		return nil, err
	}
	defer setup.done()

	converters := setup.sd.snapshot.Converters()
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
	setup, err := s.setupLanguageServiceForFile(ctx, params.Snapshot, params.Project, params.File)
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
	return toAPITextEdits(setup.sourceFile, setup.sd.snapshot.Converters(), editsByFile[setup.sourceFile.FileName()]), nil
}

// languageServiceSetup bundles the state a language service handler needs: the
// resolved snapshot/program/file plus the service itself.
type languageServiceSetup struct {
	sd          *snapshotData
	program     *compiler.Program
	sourceFile  *ast.SourceFile
	langSvc     *ls.LanguageService
	documentURI lsproto.DocumentUri
	done        func()
}

// setupLanguageServiceForFile resolves a snapshot, project, and file, and builds
// a language service scoped to that file.
func (s *Session) setupLanguageServiceForFile(ctx context.Context, snapshot SnapshotID, project ProjectID, file DocumentIdentifier) (*languageServiceSetup, error) {
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
	langSvc, err := s.setupLanguageService(sd, program, project, fileName)
	if err != nil {
		return nil, err
	}
	return &languageServiceSetup{
		sd:          sd,
		program:     program,
		sourceFile:  sourceFile,
		langSvc:     langSvc,
		documentURI: file.ToURI(s.projectSession.GetCurrentDirectory()),
		done:        func() {},
	}, nil
}

// toAPIEditsFromResponse converts an LSP formatting response into API edits.
func (s *Session) toAPIEditsFromResponse(setup *languageServiceSetup, response lsproto.TextEditsOrNull) []*TextEdit {
	if response.TextEdits == nil {
		return []*TextEdit{}
	}
	return toAPITextEdits(setup.sourceFile, setup.sd.snapshot.Converters(), *response.TextEdits)
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
