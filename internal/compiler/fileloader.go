package compiler

import (
	"cmp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/microsoft/typescript-go/internal/ast"
	"github.com/microsoft/typescript-go/internal/collections"
	"github.com/microsoft/typescript-go/internal/core"
	"github.com/microsoft/typescript-go/internal/diagnostics"
	"github.com/microsoft/typescript-go/internal/module"
	"github.com/microsoft/typescript-go/internal/tracing"
	"github.com/microsoft/typescript-go/internal/tsoptions"
	"github.com/microsoft/typescript-go/internal/tspath"
	"github.com/zeebo/xxh3"
)

type libResolution struct {
	libraryName string
	resolution  *module.ResolvedModule
	trace       []module.DiagAndArgs
}

type LibFile struct {
	Name     string
	path     string
	Replaced bool
}

type sourceFileFromReferenceDiagnostic struct {
	message *diagnostics.Message
	args    []any
}

type fileLoader struct {
	opts                                           ProgramOptions
	resolver                                       *module.Resolver
	defaultLibraryPath                             string
	comparePathsOptions                            tspath.ComparePathsOptions
	supportedExtensions                            [][]string
	supportedExtensionsWithJsonIfResolveJsonModule [][]string

	filesParser *filesParser
	rootTasks   []*parseTask

	// base is the program being added to, and is nil for a build from scratch. When
	// it is set the walk stops at every file the base already holds, and gives up
	// when it meets something the base cannot simply be extended with.
	base *processedFiles
	// baseFilesAfterRoots are the files base holds past the ones its root files
	// brought in, which is what an automatic type directive brought in. A rebuild
	// would place any of them an added root reaches among that root's own files
	// instead, so the walk gives up rather than leave one where it is.
	baseFilesAfterRoots collections.Set[tspath.Path]
	// removed names the root files the result is being built without, and is empty
	// for an addition that takes nothing away.
	removed *removedRoots
	// fileIncludeReasonsWithoutRemoved is base's include reasons with the removed
	// files' own entries dropped and the reasons they gave other files filtered out.
	// It is worked out before the walk, since it is also what says the removal can be
	// made at all — see canRemoveRoots.
	fileIncludeReasonsWithoutRemoved map[tspath.Path][]*FileIncludeReason
	// newFiles are the files this walk brought into the program, in the order the
	// result holds them. It is only set when the result extends an existing program.
	newFiles   []*ast.SourceFile
	gaveUp     atomic.Bool
	acquiredMu sync.Mutex
	acquired   []*ast.SourceFile

	totalFileCount atomic.Int32
	libFileCount   atomic.Int32

	factoryMu sync.Mutex
	factory   ast.NodeFactory

	projectReferenceFileMapper *projectReferenceFileMapper
	dtsDirectories             collections.Set[tspath.Path]

	pathForLibFileCache       collections.SyncMap[string, *LibFile]
	pathForLibFileResolutions collections.SyncMap[tspath.Path, *libResolution]
}

type redirectsFile struct {
	// Index of file at which this redirect file needs to be iterated
	index    int
	fileName string
	path     tspath.Path
	target   tspath.Path
}

type DuplicateSourceFile struct {
	ParseOptions ast.SourceFileParseOptions
	Hash         xxh3.Uint128
	ScriptKind   core.ScriptKind
}

var _ ast.HasFileName = (*redirectsFile)(nil)

func (r *redirectsFile) FileName() string {
	return r.fileName
}

func (r *redirectsFile) Path() tspath.Path {
	return r.path
}

type processedFiles struct {
	resolver *module.Resolver
	files    []*ast.SourceFile
	// duplicateSourceFiles tracks parsed files loaded during program construction
	// that were later dropped from the final program, such as losing filename
	// casing variants for the same path or files hidden behind package redirect
	// deduplication. Their parse-cache acquires still need to be balanced when
	// the program is disposed.
	duplicateSourceFiles          []*DuplicateSourceFile
	filesByPath                   map[tspath.Path]*ast.SourceFile
	projectReferenceFileMapper    *projectReferenceFileMapper
	missingFiles                  []string
	resolvedModules               map[tspath.Path]module.ModeAwareCache[*module.ResolvedModule]
	typeResolutionsInFile         map[tspath.Path]module.ModeAwareCache[*module.ResolvedTypeReferenceDirective]
	sourceFileMetaDatas           map[tspath.Path]ast.SourceFileMetaData
	jsxRuntimeImportSpecifiers    map[tspath.Path]*jsxRuntimeImportSpecifier
	importHelpersImportSpecifiers map[tspath.Path]*ast.StringLiteralNode
	libFiles                      map[tspath.Path]*LibFile
	// List of present unsupported extensions
	sourceFilesFoundSearchingNodeModules collections.Set[tspath.Path]
	includeProcessor                     *includeProcessor
	// if file was included using source file and its output is actually part of program
	// this contains mapping from output to source file
	outputFileToProjectReferenceSource map[tspath.Path]string
	// Key is a file path. Value is the list of files that redirect to it (same package, different install location)
	redirectTargetsMap map[tspath.Path][]string
	// filesByPath for redirect files
	redirectFilesByPath map[tspath.Path]*redirectsFile
	finishedProcessing  bool

	// libFileCount is how many of files are lib files. The parser puts them first.
	libFileCount int
	// rootFilesEnd is the index in files just past the last file a root file task
	// brought in, which is where the files an automatic type directive brought in
	// start. Roots added to an existing program are inserted there, so that the
	// order is the one a build from scratch with the same roots would have given.
	rootFilesEnd int
	// filesByLowerCasePath is only kept on a case sensitive file system, where two
	// files can differ in nothing but their casing and the parser says so.
	filesByLowerCasePath map[string]tspath.Path
}

type jsxRuntimeImportSpecifier struct {
	moduleReference string
	specifier       *ast.StringLiteralNode
}

// processRootFileChanges rebuilds base with root files dropped from and appended to
// the root file list it was built from, and reports whether the result is the one a
// build from scratch would have produced. A false return means the caller has to build
// one.
//
// Only the added roots and whatever they reach that is not already in base is walked,
// so the work is proportional to what changed rather than to the size of the program.
// Everything else about the result — the order of the files, why each was included,
// what each import resolved to — is base's, unchanged.
//
// A removal is the harder of the two, because a file leaving can change what other
// files resolve to where a file arriving at the end cannot. It is only made when the
// removed roots are ones the rest of the program never asked for: see canRemoveRoots
// for the whole of that argument.
func processRootFileChanges(
	opts ProgramOptions,
	base *processedFiles,
	removed *removedRoots,
	addedRootFiles []string,
	singleThreaded bool,
) (files processedFiles, acquired []*ast.SourceFile, newFiles []*ast.SourceFile, ok bool) {
	loader := newFileLoader(opts, len(addedRootFiles), singleThreaded)
	loader.base = base
	loader.removed = removed
	for _, file := range base.files[base.rootFilesEnd:] {
		loader.baseFilesAfterRoots.Add(file.Path())
	}
	if !removed.isEmpty() {
		reasons, canRemove := canRemoveRoots(base, removed)
		if !canRemove {
			return processedFiles{}, nil, nil, false
		}
		loader.fileIncludeReasonsWithoutRemoved = reasons
	}
	loader.filesParser.incremental = true
	loader.addProjectReferenceTasks(singleThreaded)
	loader.resolver = module.NewResolver(loader.projectReferenceFileMapper.host, opts.Config.CompilerOptions(), opts.TypingsLocation, opts.ProjectName)
	for _, rootFile := range addedRootFiles {
		loader.addRootFileTask(rootFile, nil, &FileIncludeReason{kind: fileIncludeKindRootFile, data: rootFile})
	}

	loader.filesParser.parse(&loader, loader.rootTasks)

	loader.projectReferenceFileMapper.loader = nil
	loader.projectReferenceFileMapper.host = nil

	if loader.gaveUp.Load() {
		return processedFiles{}, loader.acquired, nil, false
	}
	files = loader.filesParser.getProcessedFiles(&loader)
	return files, loader.acquired, loader.newFiles, !loader.gaveUp.Load()
}

// removedRoots names the root files a program is being built without, by the names the
// config gave them and by the paths those name. It is empty for an addition that takes
// nothing away.
type removedRoots struct {
	names collections.Set[string]
	paths collections.Set[tspath.Path]
}

func (r *removedRoots) isEmpty() bool {
	return r == nil || r.paths.Len() == 0
}

func (r *removedRoots) hasPath(path tspath.Path) bool {
	return r != nil && r.paths.Has(path)
}

// canRemoveRoots reports whether base is the program a build from scratch would give
// once the named roots are simply left out of it, and returns what each file's include
// reasons become when it is. A false return leaves the caller to build the program.
//
// What makes a removal harder than an addition is that a file leaving can change what
// other files resolve to: a file that resolved to it has to resolve again, and may land
// on a copy under node_modules, on a file sharing its stem, or on nothing. It can also
// move files that are staying, since where a file sits in the program is decided by the
// first thing that reached it, and a removed file may have been that.
//
// Both of those are the same condition, and it is checked rather than reasoned about:
// nothing that is staying may owe its place to something that is going. Concretely,
//
//   - every reason each removed root is in the program is a root file reason for one of
//     the roots being removed, so no file resolved to it, referenced it, or named it as
//     a type library — a removal cannot make a lookup that failed succeed, so nothing
//     else has to resolve again; and
//   - no file that is staying has a removed file as the *first* of its include reasons,
//     which is the reason that placed it. A removed root's walk therefore brought
//     nothing into the program but itself, and every other file keeps the place a
//     rebuild would give it.
//
// The rest are the ways a file is more than its place in the list: a lib file, which is
// sorted ahead of everything else; a file the base only found by searching node_modules;
// a file the base holds under a different name; a duplicate, whose parse cache
// accounting is worked out over the whole walk; and a diagnostic the parse produced
// about a file that is going.
func canRemoveRoots(base *processedFiles, removed *removedRoots) (map[tspath.Path][]*FileIncludeReason, bool) {
	if len(base.duplicateSourceFiles) > 0 {
		return nil, false
	}
	for path := range removed.paths.Keys() {
		existing, ok := base.filesByPath[path]
		if !ok {
			return nil, false
		}
		if _, isLib := base.libFiles[path]; isLib {
			return nil, false
		}
		if base.sourceFilesFoundSearchingNodeModules.Has(path) {
			return nil, false
		}
		if base.filesByLowerCasePath != nil {
			// on a case sensitive file system the index holds the first path of each
			// casing, so a removal that is not the one indexed would take another
			// file's entry with it
			if indexed, ok := base.filesByLowerCasePath[tspath.ToFileNameLowerCase(string(path))]; !ok || indexed != path {
				return nil, false
			}
		}
		if !removed.names.Has(existing.FileName()) {
			// the program holds it under a name none of the removed roots gave it,
			// so something else means it to be here under that name
			return nil, false
		}
		for _, reason := range base.includeProcessor.fileIncludeReasons[path] {
			if reason.kind != fileIncludeKindRootFile || !removed.names.Has(reason.asRootFileName()) {
				return nil, false
			}
		}
	}
	for _, diagnostic := range base.includeProcessor.processingDiagnostics {
		if diagnostic.mentionsAnyOf(removed) {
			return nil, false
		}
	}
	reasons := make(map[tspath.Path][]*FileIncludeReason, len(base.includeProcessor.fileIncludeReasons))
	for path, fileReasons := range base.includeProcessor.fileIncludeReasons {
		if removed.hasPath(path) {
			continue
		}
		kept := fileReasons
		for i, reason := range fileReasons {
			if !reason.isReferencedFile() || !removed.hasPath(reason.asReferencedFileData().file) {
				continue
			}
			if i == 0 {
				// a removed file is what put this one in the program, so a rebuild
				// would reach it from somewhere else and could place it elsewhere
				return nil, false
			}
			if len(kept) == len(fileReasons) {
				kept = slices.Clip(fileReasons[:i])
			}
		}
		if len(kept) != len(fileReasons) {
			for _, reason := range fileReasons[len(kept):] {
				if !reason.isReferencedFile() || !removed.hasPath(reason.asReferencedFileData().file) {
					kept = append(kept, reason)
				}
			}
		}
		reasons[path] = kept
	}
	return reasons, true
}

func processAllProgramFiles(
	opts ProgramOptions,
	singleThreaded bool,
) processedFiles {
	compilerOptions := opts.Config.CompilerOptions()
	rootFiles := opts.Config.FileNames()
	loader := newFileLoader(opts, len(rootFiles)+len(compilerOptions.Lib), singleThreaded)
	loader.addProjectReferenceTasks(singleThreaded)
	loader.resolver = module.NewResolver(loader.projectReferenceFileMapper.host, compilerOptions, opts.TypingsLocation, opts.ProjectName)
	if opts.Tracing != nil {
		defer opts.Tracing.Push(tracing.PhaseProgram, "processRootFiles", map[string]any{"count": len(rootFiles)}, false)()
	}
	for _, rootFile := range rootFiles {
		loader.addRootFileTask(rootFile, nil, &FileIncludeReason{kind: fileIncludeKindRootFile, data: rootFile})
	}
	if len(rootFiles) > 0 && compilerOptions.NoLib.IsFalseOrUnknown() {
		if compilerOptions.Lib == nil {
			name := tsoptions.GetDefaultLibFileName(compilerOptions)
			libFile := loader.pathForLibFile(name)
			loader.addRootTask(libFile.path, libFile, &FileIncludeReason{kind: fileIncludeKindLibFile})

		} else {
			for index, lib := range compilerOptions.Lib {
				if name, ok := tsoptions.GetLibFileName(lib); ok {
					libFile := loader.pathForLibFile(name)
					loader.addRootTask(libFile.path, libFile, &FileIncludeReason{kind: fileIncludeKindLibFile, data: index})
				}
				// !!! error on unknown name
			}
		}
	}

	if len(rootFiles) > 0 {
		loader.addAutomaticTypeDirectiveTasks()
	}

	loader.filesParser.parse(&loader, loader.rootTasks)

	// Clear out loader and host to ensure its not used post program creation
	loader.projectReferenceFileMapper.loader = nil
	loader.projectReferenceFileMapper.host = nil

	return loader.filesParser.getProcessedFiles(&loader)
}

func newFileLoader(opts ProgramOptions, rootTaskCapacity int, singleThreaded bool) fileLoader {
	compilerOptions := opts.Config.CompilerOptions()
	supportedExtensions := tsoptions.GetSupportedExtensions(compilerOptions, nil /*extraFileExtensions*/)
	var maxNodeModuleJsDepth int
	if p := compilerOptions.MaxNodeModuleJsDepth; p != nil {
		maxNodeModuleJsDepth = *p
	}
	return fileLoader{
		opts:               opts,
		defaultLibraryPath: tspath.GetNormalizedAbsolutePath(opts.Host.DefaultLibraryPath(), opts.Host.GetCurrentDirectory()),
		comparePathsOptions: tspath.ComparePathsOptions{
			UseCaseSensitiveFileNames: opts.Host.FS().UseCaseSensitiveFileNames(),
			CurrentDirectory:          opts.Host.GetCurrentDirectory(),
		},
		filesParser: &filesParser{
			wg:       core.NewWorkGroup(singleThreaded),
			maxDepth: maxNodeModuleJsDepth,
		},
		rootTasks:           make([]*parseTask, 0, rootTaskCapacity),
		supportedExtensions: supportedExtensions,
		supportedExtensionsWithJsonIfResolveJsonModule: tsoptions.GetSupportedExtensionsWithJsonIfResolveJsonModule(compilerOptions, supportedExtensions),
	}
}

// giveUp records that the walk has met something an incremental root addition cannot
// reproduce, so its caller must build a program from scratch instead.
func (p *fileLoader) giveUp() {
	p.gaveUp.Store(true)
}

func (p *fileLoader) toPath(file string) tspath.Path {
	return tspath.ToPath(file, p.opts.Host.GetCurrentDirectory(), p.opts.Host.FS().UseCaseSensitiveFileNames())
}

func (p *fileLoader) addRootTask(fileName string, libFile *LibFile, includeReason *FileIncludeReason) {
	absPath := tspath.GetNormalizedAbsolutePath(fileName, p.opts.Host.GetCurrentDirectory())
	if p.opts.Config.CompilerOptions().AllowNonTsExtensions.IsTrue() || tspath.HasExtension(absPath) {
		p.rootTasks = append(p.rootTasks, &parseTask{
			normalizedFilePath: absPath,
			libFile:            libFile,
			includeReason:      includeReason,
		})
	}
}

func (p *fileLoader) addRootFileTask(fileName string, libFile *LibFile, includeReason *FileIncludeReason) {
	currDir := p.opts.Host.GetCurrentDirectory()
	absPath := tspath.GetNormalizedAbsolutePath(fileName, currDir)
	containingFile := currDir
	if p.opts.Config.ConfigFile != nil {
		containingFile = tspath.GetNormalizedAbsolutePath(p.opts.Config.ConfigFile.SourceFile.FileName(), currDir)
	}
	resolvedFile, diagnostic := p.getSourceFileFromReference(absPath, fileName, containingFile, includeReason)
	rootTask := &parseTask{
		normalizedFilePath: resolvedFile,
		libFile:            libFile,
		includeReason:      includeReason,
	}
	if diagnostic != nil {
		rootTask.normalizedFilePath = absPath
		rootTask.processingDiagnostics = []*processingDiagnostic{{
			kind: processingDiagnosticKindExplainingFileInclude,
			data: &includeExplainingDiagnostic{
				diagnosticReason: includeReason,
				message:          diagnostic.message,
				args:             diagnostic.args,
			},
		}}
	}
	p.rootTasks = append(p.rootTasks, rootTask)
}

func (p *fileLoader) addAutomaticTypeDirectiveTasks() {
	var containingDirectory string
	compilerOptions := p.opts.Config.CompilerOptions()
	if compilerOptions.ConfigFilePath != "" {
		containingDirectory = tspath.GetDirectoryPath(compilerOptions.ConfigFilePath)
	} else {
		containingDirectory = p.opts.Host.GetCurrentDirectory()
	}
	containingFileName := tspath.CombinePaths(containingDirectory, module.InferredTypesContainingFile)
	p.rootTasks = append(p.rootTasks, &parseTask{
		normalizedFilePath:          containingFileName,
		isForAutomaticTypeDirective: true,
	})
}

func (p *fileLoader) resolveAutomaticTypeDirectives(containingFileName string) (
	toParse []resolvedRef,
	typeResolutionsInFile module.ModeAwareCache[*module.ResolvedTypeReferenceDirective],
	typeResolutionsTrace []module.DiagAndArgs,
	pDiagnostics []*processingDiagnostic,
) {
	automaticTypeDirectiveNames := module.GetAutomaticTypeDirectiveNames(p.opts.Config.CompilerOptions(), p.opts.Host)
	if len(automaticTypeDirectiveNames) != 0 {
		toParse = make([]resolvedRef, 0, len(automaticTypeDirectiveNames))
		typeResolutionsInFile = make(module.ModeAwareCache[*module.ResolvedTypeReferenceDirective], len(automaticTypeDirectiveNames))
		for _, name := range automaticTypeDirectiveNames {
			// Under node16/nodenext module resolution, load `types`/ata include names as cjs resolution results by passing an `undefined` mode.
			// Under bundler module resolution, this also triggers the "import" condition to be used.
			resolutionMode := core.ResolutionModeNone
			resolved, trace := p.resolver.ResolveTypeReferenceDirective(name, containingFileName, resolutionMode, nil)
			var traceDone func()
			if p.opts.Tracing != nil {
				traceDone = p.opts.Tracing.Push(tracing.PhaseProgram, "processTypeReferenceDirective", map[string]any{"directive": name, "hasResolved": resolved.IsResolved(), "refKind": int(fileIncludeKindAutomaticTypeDirectiveFile)}, false)
			}
			typeResolutionsInFile[module.ModeAwareCacheKey{Name: name, Mode: resolutionMode}] = resolved
			typeResolutionsTrace = append(typeResolutionsTrace, trace...)
			if resolved.IsResolved() {
				toParse = append(toParse, resolvedRef{
					fileName:      resolved.ResolvedFileName,
					increaseDepth: resolved.IsExternalLibraryImport,
					elideOnDepth:  false,
					includeReason: &FileIncludeReason{
						kind: fileIncludeKindAutomaticTypeDirectiveFile,
						data: &automaticTypeDirectiveFileData{name, resolved.PackageId},
					},
					packageId: resolved.PackageId,
				})
			} else {
				pDiagnostics = append(pDiagnostics, &processingDiagnostic{
					kind: processingDiagnosticKindExplainingFileInclude,
					data: &includeExplainingDiagnostic{
						diagnosticReason: &FileIncludeReason{
							kind: fileIncludeKindAutomaticTypeDirectiveFile,
							data: &automaticTypeDirectiveFileData{typeReference: name},
						},
						message: diagnostics.Cannot_find_type_definition_file_for_0,
						args:    []any{name},
					},
				})
			}
			if traceDone != nil {
				traceDone()
			}
		}
	}
	return toParse, typeResolutionsInFile, typeResolutionsTrace, pDiagnostics
}

func (p *fileLoader) addProjectReferenceTasks(singleThreaded bool) {
	p.projectReferenceFileMapper = &projectReferenceFileMapper{
		opts: p.opts,
		host: p.opts.Host,
	}
	projectReferences := p.opts.Config.ResolvedProjectReferencePaths()
	if len(projectReferences) == 0 {
		return
	}

	parser := &projectReferenceParser{
		loader: p,
		wg:     core.NewWorkGroup(singleThreaded),
	}
	rootTasks := createProjectReferenceParseTasks(projectReferences)
	parser.parse(rootTasks)
}

func (p *fileLoader) sortLibs(libFiles []*ast.SourceFile) {
	slices.SortFunc(libFiles, func(f1 *ast.SourceFile, f2 *ast.SourceFile) int {
		return cmp.Compare(p.getDefaultLibFilePriority(f1), p.getDefaultLibFilePriority(f2))
	})
}

func (p *fileLoader) getDefaultLibFilePriority(a *ast.SourceFile) int {
	// defaultLibraryPath and a.FileName() are absolute and normalized; a prefix check should suffice.
	defaultLibraryPath := tspath.RemoveTrailingDirectorySeparator(p.defaultLibraryPath)
	aFileName := a.FileName()

	if strings.HasPrefix(aFileName, defaultLibraryPath) && len(aFileName) > len(defaultLibraryPath) && aFileName[len(defaultLibraryPath)] == tspath.DirectorySeparator {
		// avoid tspath.GetBaseFileName; we know these paths are already absolute and normalized.
		basename := aFileName[strings.LastIndexByte(aFileName, tspath.DirectorySeparator)+1:]
		if basename == "lib.d.ts" || basename == "lib.es6.d.ts" {
			return 0
		}
		name := strings.TrimSuffix(strings.TrimPrefix(basename, "lib."), ".d.ts")
		index := slices.Index(tsoptions.Libs, name)
		if index != -1 {
			return index + 1
		}
	}
	return len(tsoptions.Libs) + 2
}

func (p *fileLoader) loadSourceFileMetaData(fileName string) ast.SourceFileMetaData {
	packageJsonScope := p.resolver.GetPackageScopeForPath(tspath.GetDirectoryPath(fileName))
	moduleResolutionKind := p.opts.Config.CompilerOptions().GetModuleResolutionKind()

	var packageJsonType, packageJsonDirectory string
	if packageJsonScope.Exists() {
		packageJsonDirectory = packageJsonScope.PackageDirectory
		if value, ok := packageJsonScope.Contents.Type.GetValue(); ok {
			if !tspath.FileExtensionIsOneOf(fileName, []string{tspath.ExtensionMts, tspath.ExtensionCts, tspath.ExtensionMjs, tspath.ExtensionCjs}) &&
				core.ModuleResolutionKindNode16 <= moduleResolutionKind && moduleResolutionKind <= core.ModuleResolutionKindNodeNext || strings.Contains(fileName, "/node_modules/") {
				packageJsonType = value
			}
		}
	}

	impliedNodeFormat := ast.GetImpliedNodeFormatForFile(fileName, packageJsonType)
	return ast.SourceFileMetaData{
		PackageJsonType:      packageJsonType,
		PackageJsonDirectory: packageJsonDirectory,
		ImpliedNodeFormat:    impliedNodeFormat,
	}
}

func (p *fileLoader) parseSourceFile(t *parseTask) *ast.SourceFile {
	if p.opts.Tracing != nil {
		defer p.opts.Tracing.Push(tracing.PhaseParse, "createSourceFile", map[string]any{"path": t.normalizedFilePath}, true)()
	}
	path := p.toPath(t.normalizedFilePath)
	options := p.projectReferenceFileMapper.getCompilerOptionsForFile(t)
	sourceFile := p.opts.Host.GetSourceFile(ast.SourceFileParseOptions{
		FileName:                       t.normalizedFilePath,
		Path:                           path,
		ExternalModuleIndicatorOptions: ast.GetExternalModuleIndicatorOptions(t.normalizedFilePath, options, t.metadata),
	})
	if p.base != nil && sourceFile != nil {
		// an incremental root addition has to give these back if it turns out it
		// cannot be made, since nothing else will own them
		p.acquiredMu.Lock()
		p.acquired = append(p.acquired, sourceFile)
		p.acquiredMu.Unlock()
	}
	return sourceFile
}

func (p *fileLoader) isSupportedExtension(canonicalFileName string) bool {
	for _, group := range p.supportedExtensionsWithJsonIfResolveJsonModule {
		if tspath.FileExtensionIsOneOf(canonicalFileName, group) {
			return true
		}
	}
	return false
}

func (p *fileLoader) getSourceFileFromReference(
	fileName string,
	referenceText string,
	containingFile string,
	includeReason *FileIncludeReason,
) (string, *sourceFileFromReferenceDiagnostic) {
	options := p.opts.Config.CompilerOptions()
	allowNonTsExtensions := options.AllowNonTsExtensions.IsTrue()
	diagnosticFileName := tspath.NormalizeSlashes(referenceText)

	if tspath.HasExtension(fileName) {
		canonicalFileName := tspath.GetCanonicalFileName(fileName, p.opts.Host.FS().UseCaseSensitiveFileNames())
		if !allowNonTsExtensions && !p.isSupportedExtension(canonicalFileName) {
			if tspath.HasJSFileExtension(canonicalFileName) {
				return "", &sourceFileFromReferenceDiagnostic{message: diagnostics.File_0_is_a_JavaScript_file_Did_you_mean_to_enable_the_allowJs_option, args: []any{diagnosticFileName}}
			}
			return "", &sourceFileFromReferenceDiagnostic{message: diagnostics.File_0_has_an_unsupported_extension_The_only_supported_extensions_are_1, args: []any{diagnosticFileName, "'" + strings.Join(core.Flatten(p.supportedExtensions), "', '") + "'"}}
		}

		if !p.opts.Host.FS().FileExists(fileName) {
			return "", &sourceFileFromReferenceDiagnostic{message: diagnostics.File_0_not_found, args: []any{diagnosticFileName}}
		}

		if includeReason.isReferencedFile() && tspath.GetCanonicalFileName(containingFile, p.opts.Host.FS().UseCaseSensitiveFileNames()) == canonicalFileName {
			return "", &sourceFileFromReferenceDiagnostic{message: diagnostics.A_file_cannot_have_a_reference_to_itself}
		}
		return fileName, nil
	}

	if allowNonTsExtensions && p.opts.Host.FS().FileExists(fileName) {
		return fileName, nil
	}

	if allowNonTsExtensions {
		return "", &sourceFileFromReferenceDiagnostic{message: diagnostics.File_0_not_found, args: []any{diagnosticFileName}}
	}

	for _, ext := range p.supportedExtensions[0] {
		candidate := fileName + ext
		if p.opts.Host.FS().FileExists(candidate) {
			return candidate, nil
		}
	}

	return "", &sourceFileFromReferenceDiagnostic{message: diagnostics.Could_not_resolve_the_path_0_with_the_extensions_Colon_1, args: []any{diagnosticFileName, "'" + strings.Join(core.Flatten(p.supportedExtensions), "', '") + "'"}}
}

func (p *fileLoader) resolveTripleslashPathReference(moduleName string, containingFile string, index int) (*resolvedRef, *processingDiagnostic) {
	basePath := tspath.GetDirectoryPath(containingFile)
	referencedFileName := moduleName

	if !tspath.IsRootedDiskPath(moduleName) {
		referencedFileName = tspath.CombinePaths(basePath, moduleName)
	}
	normalizedFileName := tspath.NormalizePath(referencedFileName)
	includeReason := &FileIncludeReason{
		kind: fileIncludeKindReferenceFile,
		data: &referencedFileData{
			file:  p.toPath(containingFile),
			index: index,
		},
	}

	resolvedFileName, diagnostic := p.getSourceFileFromReference(
		normalizedFileName,
		moduleName,
		containingFile,
		includeReason,
	)
	if diagnostic != nil {
		return nil, &processingDiagnostic{
			kind: processingDiagnosticKindExplainingFileInclude,
			data: &includeExplainingDiagnostic{
				diagnosticReason: includeReason,
				message:          diagnostic.message,
				args:             diagnostic.args,
			},
		}
	}

	return &resolvedRef{
		fileName:      resolvedFileName,
		includeReason: includeReason,
	}, nil
}

func (p *fileLoader) resolveTypeReferenceDirectives(t *parseTask) {
	file := t.file
	if len(file.TypeReferenceDirectives) == 0 {
		return
	}
	if p.opts.Tracing != nil {
		defer p.opts.Tracing.Push(tracing.PhaseProgram, "resolveTypeReferenceDirectiveNamesWorker", map[string]any{"containingFileName": file.FileName()}, false)()
	}
	meta := t.metadata

	typeResolutionsInFile := make(module.ModeAwareCache[*module.ResolvedTypeReferenceDirective], len(file.TypeReferenceDirectives))
	var typeResolutionsTrace []module.DiagAndArgs
	for index, ref := range file.TypeReferenceDirectives {
		redirect, fileName := p.projectReferenceFileMapper.getRedirectForResolution(file)
		resolutionMode := getModeForTypeReferenceDirectiveInFile(ref, file, meta, module.GetCompilerOptionsWithRedirect(p.opts.Config.CompilerOptions(), redirect))
		resolved, trace := p.resolver.ResolveTypeReferenceDirective(ref.FileName, fileName, resolutionMode, redirect)
		var traceDone func()
		if p.opts.Tracing != nil {
			traceDone = p.opts.Tracing.Push(tracing.PhaseProgram, "processTypeReferenceDirective", map[string]any{"directive": ref.FileName, "hasResolved": resolved.IsResolved(), "refKind": int(fileIncludeKindTypeReferenceDirective), "refPath": string(t.path)}, false)
		}
		typeResolutionsInFile[module.ModeAwareCacheKey{Name: ref.FileName, Mode: resolutionMode}] = resolved
		includeReason := &FileIncludeReason{
			kind: fileIncludeKindTypeReferenceDirective,
			data: &referencedFileData{
				file:  t.path,
				index: index,
			},
		}
		typeResolutionsTrace = append(typeResolutionsTrace, trace...)

		if resolved.IsResolved() {
			t.addSubTask(resolvedRef{
				fileName:      resolved.ResolvedFileName,
				increaseDepth: resolved.IsExternalLibraryImport,
				elideOnDepth:  false,
				includeReason: includeReason,
				packageId:     resolved.PackageId,
			}, nil)
		} else {
			t.processingDiagnostics = append(t.processingDiagnostics, &processingDiagnostic{
				kind: processingDiagnosticKindUnknownReference,
				data: includeReason,
			})
		}
		if traceDone != nil {
			traceDone()
		}
	}

	t.typeResolutionsInFile = typeResolutionsInFile
	t.typeResolutionsTrace = typeResolutionsTrace
}

const externalHelpersModuleNameText = "tslib" // TODO(jakebailey): dedupe

func (p *fileLoader) resolveImportsAndModuleAugmentations(t *parseTask) {
	if p.opts.Tracing != nil {
		defer p.opts.Tracing.Push(tracing.PhaseProgram, "resolveModuleNamesWorker", map[string]any{"containingFileName": t.file.FileName()}, false)()
	}
	file := t.file
	meta := t.metadata

	moduleNames := make([]*ast.Node, 0, len(file.Imports())+len(file.ModuleAugmentations)+2)

	isJavaScriptFile := ast.IsSourceFileJS(file)
	isExternalModuleFile := ast.IsExternalModule(file)

	redirect, fileName := p.projectReferenceFileMapper.getRedirectForResolution(file)
	optionsForFile := module.GetCompilerOptionsWithRedirect(p.opts.Config.CompilerOptions(), redirect)
	if isJavaScriptFile || (!file.IsDeclarationFile && (optionsForFile.GetIsolatedModules() || isExternalModuleFile)) {
		if optionsForFile.ImportHelpers.IsTrue() {
			specifier := p.createSyntheticImport(externalHelpersModuleNameText, file)
			moduleNames = append(moduleNames, specifier)
			t.importHelpersImportSpecifier = specifier
		}
	}

	if isJavaScriptFile || file.ScriptKind == core.ScriptKindTSX {
		jsxImport := ast.GetJSXRuntimeImport(ast.GetJSXImplicitImportBase(optionsForFile, file), optionsForFile)
		if jsxImport != "" {
			specifier := p.createSyntheticImport(jsxImport, file)
			moduleNames = append(moduleNames, specifier)
			t.jsxRuntimeImportSpecifier = &jsxRuntimeImportSpecifier{
				moduleReference: jsxImport,
				specifier:       specifier,
			}
		}
	}

	importsStart := len(moduleNames)

	moduleNames = append(moduleNames, file.Imports()...)
	for _, imp := range file.ModuleAugmentations {
		if imp.Kind == ast.KindStringLiteral {
			moduleNames = append(moduleNames, imp)
		}
		// Do nothing if it's an Identifier; we don't need to do module resolution for `declare global`.
	}

	if len(moduleNames) != 0 {
		resolutionsInFile := make(module.ModeAwareCache[*module.ResolvedModule], len(moduleNames))
		var resolutionsTrace []module.DiagAndArgs

		for index, entry := range moduleNames {
			moduleName := entry.Text()
			if moduleName == "" {
				continue
			}

			mode := getModeForUsageLocation(file.FileName(), meta, entry, optionsForFile)
			resolvedModule, trace := p.resolver.ResolveModuleName(moduleName, fileName, mode, redirect)
			resolutionsInFile[module.ModeAwareCacheKey{Name: moduleName, Mode: mode}] = resolvedModule
			resolutionsTrace = append(resolutionsTrace, trace...)

			if !resolvedModule.IsResolved() {
				continue
			}

			resolvedFileName := resolvedModule.ResolvedFileName
			isFromNodeModulesSearch := resolvedModule.IsExternalLibraryImport
			// Don't treat redirected files as JS files.
			isJsFile := !tspath.FileExtensionIsOneOf(resolvedFileName, tspath.SupportedTSExtensionsWithJsonFlat) && p.projectReferenceFileMapper.getRedirectParsedCommandLineForResolution(ast.NewHasFileName(resolvedFileName, p.toPath(resolvedFileName))) == nil
			isJsFileFromNodeModules := isFromNodeModulesSearch && isJsFile && strings.Contains(resolvedFileName, "/node_modules/")

			// add file to program only if:
			// - resolution was successful
			// - noResolve is falsy
			// - module name comes from the list of imports
			// - it's not a top level JavaScript module that exceeded the search max

			importIndex := index - importsStart

			shouldAddFile := moduleName != "" &&
				module.GetResolutionDiagnostic(optionsForFile, resolvedModule, file) == nil &&
				!optionsForFile.NoResolve.IsTrue() &&
				!(isJsFile && !optionsForFile.GetAllowJS()) &&
				(importIndex < 0 || (importIndex < len(file.Imports()) && (ast.IsInJSFile(file.Imports()[importIndex]) || file.Imports()[importIndex].Flags&ast.NodeFlagsJSDoc == 0)))

			if shouldAddFile {
				t.addSubTask(resolvedRef{
					fileName:      resolvedFileName,
					increaseDepth: resolvedModule.IsExternalLibraryImport,
					elideOnDepth:  isJsFileFromNodeModules,
					includeReason: &FileIncludeReason{
						kind: fileIncludeKindImport,
						data: &referencedFileData{
							file:      t.path,
							index:     importIndex,
							synthetic: core.IfElse(importIndex < 0, entry, nil),
						},
					},
					packageId: resolvedModule.PackageId,
				}, nil)
			}
		}

		t.resolutionsInFile = resolutionsInFile
		t.resolutionsTrace = resolutionsTrace
	}
}

func (p *fileLoader) createSyntheticImport(text string, file *ast.SourceFile) *ast.StringLiteralNode {
	p.factoryMu.Lock()
	defer p.factoryMu.Unlock()
	externalHelpersModuleReference := p.factory.NewStringLiteral(text, ast.TokenFlagsNone)
	importDecl := p.factory.NewImportDeclaration(nil, nil, externalHelpersModuleReference, nil)
	externalHelpersModuleReference.Parent = importDecl
	importDecl.Parent = file.AsNode()
	return externalHelpersModuleReference
}

func (p *fileLoader) pathForLibFile(name string) *LibFile {
	if cached, ok := p.pathForLibFileCache.Load(name); ok {
		return cached
	}

	path := tspath.CombinePaths(p.defaultLibraryPath, name)
	replaced := false
	if p.opts.Config.CompilerOptions().LibReplacement.IsTrue() && name != "lib.d.ts" {
		libraryName := getLibraryNameFromLibFileName(name)
		resolveFrom := getInferredLibraryNameResolveFrom(p.opts.Config.CompilerOptions(), p.opts.Host.GetCurrentDirectory(), name)
		resolution, trace := p.resolveLibrary(libraryName, resolveFrom)
		if resolution.IsResolved() {
			path = resolution.ResolvedFileName
			replaced = true
		}
		p.pathForLibFileResolutions.LoadOrStore(p.toPath(resolveFrom), &libResolution{
			libraryName: libraryName,
			resolution:  resolution,
			trace:       trace,
		})
	}

	libPath, _ := p.pathForLibFileCache.LoadOrStore(name, &LibFile{name, path, replaced})
	return libPath
}

func (p *fileLoader) resolveLibrary(libraryName, resolveFrom string) (*module.ResolvedModule, []module.DiagAndArgs) {
	if tr := p.opts.Tracing; tr != nil {
		defer tr.Push(tracing.PhaseProgram, "resolveLibrary", map[string]any{"resolveFrom": resolveFrom}, false)()
	}
	return p.resolver.ResolveModuleName(libraryName, resolveFrom, core.ModuleKindCommonJS, nil)
}

func getLibraryNameFromLibFileName(libFileName string) string {
	// Support resolving to lib.dom.d.ts -> @typescript/lib-dom, and
	//                      lib.dom.iterable.d.ts -> @typescript/lib-dom/iterable
	//                      lib.es2015.symbol.wellknown.d.ts -> @typescript/lib-es2015/symbol-wellknown
	components := strings.Split(libFileName, ".")
	var path strings.Builder
	path.WriteString("@typescript/lib-")
	if len(components) > 1 {
		path.WriteString(components[1])
	}
	i := 2
	for i < len(components) && components[i] != "" && components[i] != "d" {
		if i == 2 {
			path.WriteByte('/')
		} else {
			path.WriteByte('-')
		}
		path.WriteString(components[i])
		i++
	}
	return path.String()
}

func getInferredLibraryNameResolveFrom(options *core.CompilerOptions, currentDirectory string, libFileName string) string {
	var containingDirectory string
	if options.ConfigFilePath != "" {
		containingDirectory = tspath.GetDirectoryPath(options.ConfigFilePath)
	} else {
		containingDirectory = currentDirectory
	}
	return tspath.CombinePaths(containingDirectory, "__lib_node_modules_lookup_"+libFileName+"__.ts")
}

func getModeForTypeReferenceDirectiveInFile(ref *ast.FileReference, file *ast.SourceFile, meta ast.SourceFileMetaData, options *core.CompilerOptions) core.ResolutionMode {
	if ref.ResolutionMode != core.ResolutionModeNone {
		return ref.ResolutionMode
	} else {
		return getDefaultResolutionModeForFile(file.FileName(), meta, options)
	}
}

func getDefaultResolutionModeForFile(fileName string, meta ast.SourceFileMetaData, options *core.CompilerOptions) core.ResolutionMode {
	if importSyntaxAffectsModuleResolution(options) {
		return ast.GetImpliedNodeFormatForEmitWorker(fileName, options.GetEmitModuleKind(), meta)
	} else {
		return core.ResolutionModeNone
	}
}

func getModeForUsageLocation(fileName string, meta ast.SourceFileMetaData, usage *ast.StringLiteralLike, options *core.CompilerOptions) core.ResolutionMode {
	if ast.IsImportDeclaration(usage.Parent) || usage.Parent.Kind == ast.KindJSImportDeclaration || ast.IsExportDeclaration(usage.Parent) || ast.IsJSDocImportTag(usage.Parent) {
		isTypeOnly := ast.IsExclusivelyTypeOnlyImportOrExport(usage.Parent)
		if isTypeOnly {
			var override core.ResolutionMode
			var ok bool
			switch usage.Parent.Kind {
			case ast.KindImportDeclaration, ast.KindJSImportDeclaration:
				override, ok = usage.Parent.AsImportDeclaration().Attributes.GetResolutionModeOverride()
			case ast.KindExportDeclaration:
				override, ok = usage.Parent.AsExportDeclaration().Attributes.GetResolutionModeOverride()
			case ast.KindJSDocImportTag:
				override, ok = usage.Parent.AsJSDocImportTag().Attributes.GetResolutionModeOverride()
			}
			if ok {
				return override
			}
		}
	}
	if ast.IsLiteralTypeNode(usage.Parent) && ast.IsImportTypeNode(usage.Parent.Parent) {
		if override, ok := usage.Parent.Parent.AsImportTypeNode().Attributes.GetResolutionModeOverride(); ok {
			return override
		}
	}

	if options != nil && importSyntaxAffectsModuleResolution(options) {
		return getEmitSyntaxForUsageLocationWorker(fileName, meta, usage, options)
	}

	return core.ResolutionModeNone
}

func importSyntaxAffectsModuleResolution(options *core.CompilerOptions) bool {
	moduleResolution := options.GetModuleResolutionKind()
	return core.ModuleResolutionKindNode16 <= moduleResolution && moduleResolution <= core.ModuleResolutionKindNodeNext ||
		options.GetResolvePackageJsonExports() || options.GetResolvePackageJsonImports()
}

func getEmitSyntaxForUsageLocationWorker(fileName string, meta ast.SourceFileMetaData, usage *ast.Node, options *core.CompilerOptions) core.ResolutionMode {
	if ast.IsRequireCall(usage.Parent, false /*requireStringLiteralLikeArgument*/) || ast.IsExternalModuleReference(usage.Parent) && ast.IsImportEqualsDeclaration(usage.Parent.Parent) {
		return core.ModuleKindCommonJS
	}
	fileEmitMode := ast.GetEmitModuleFormatOfFileWorker(fileName, options, meta)
	if ast.IsImportCall(ast.WalkUpParenthesizedExpressions(usage.Parent)) {
		if ast.ShouldTransformImportCall(fileName, options, fileEmitMode) {
			return core.ModuleKindCommonJS
		} else {
			return core.ModuleKindESNext
		}
	}
	// If we're in --module preserve on an input file, we know that an import
	// is an import. But if this is a declaration file, we'd prefer to use the
	// impliedNodeFormat. Since we want things to be consistent between the two,
	// we need to issue errors when the user writes ESM syntax in a definitely-CJS
	// file, until/unless declaration emit can indicate a true ESM import. On the
	// other hand, writing CJS syntax in a definitely-ESM file is fine, since declaration
	// emit preserves the CJS syntax.
	if fileEmitMode == core.ModuleKindCommonJS {
		return core.ModuleKindCommonJS
	} else {
		if fileEmitMode.IsNonNodeESM() || fileEmitMode == core.ModuleKindPreserve {
			return core.ModuleKindESNext
		}
	}
	return core.ModuleKindNone
}
