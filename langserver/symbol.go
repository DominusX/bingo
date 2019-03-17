package langserver

import (
	"context"
	"fmt"
	"go/ast"
	"go/token"
	"log"
	"path"
	"sort"
	"strings"
	"sync"

	"github.com/saibing/bingo/langserver/internal/source"
	"github.com/saibing/bingo/langserver/internal/util"
	"github.com/sourcegraph/go-lsp"
	"github.com/sourcegraph/go-lsp/lspext"
	"github.com/sourcegraph/jsonrpc2"
)

// Query is a structured representation that is parsed from the user's
// raw query string.
type Query struct {
	Kind      lsp.SymbolKind
	Filter    FilterType
	File, Dir string
	Tokens    []string

	Symbol lspext.SymbolDescriptor
}

// String converts the query back into a logically equivalent, but not strictly
// byte-wise equal, query string. It is useful for converting a modified query
// structure back into a query string.
func (q Query) String() string {
	s := ""
	switch q.Filter {
	case FilterExported:
		s = queryJoin(s, "is:exported")
	case FilterDir:
		s = queryJoin(s, fmt.Sprintf("%s:%s", q.Filter, q.Dir))
	default:
		// no filter.
	}
	if q.Kind != 0 {
		for kwd, kind := range keywords {
			if kind == q.Kind {
				s = queryJoin(s, kwd)
			}
		}
	}
	for _, token := range q.Tokens {
		s = queryJoin(s, token)
	}
	return s
}

// queryJoin joins the strings into "<s><space><e>" ensuring there is no
// trailing or leading whitespace at the end of the string.
func queryJoin(s, e string) string {
	return strings.TrimSpace(s + " " + e)
}

// ParseQuery parses a user's raw query string and returns a
// structured representation of the query.
func ParseQuery(q string) (qu Query) {
	// All queries are case insensitive.
	q = strings.ToLower(q)

	// Split the query into space-delimited fields.
	for _, field := range strings.Fields(q) {
		// Check if the field is a filter like `is:exported`.
		if strings.HasPrefix(field, "dir:") {
			qu.Filter = FilterDir
			qu.Dir = strings.TrimPrefix(field, "dir:")
			continue
		}
		if field == "is:exported" {
			qu.Filter = FilterExported
			continue
		}

		// Each field is split into tokens, delimited by periods or slashes.
		tokens := strings.FieldsFunc(field, func(c rune) bool {
			return c == '.' || c == '/'
		})
		for _, tok := range tokens {
			if kind, isKeyword := keywords[tok]; isKeyword {
				qu.Kind = kind
				continue
			}
			qu.Tokens = append(qu.Tokens, tok)
		}
	}
	return qu
}

type FilterType string

const (
	FilterExported FilterType = "exported"
	FilterDir      FilterType = "dir"
)

// keywords are keyword tokens that will be interpreted as symbol kind
// filters in the search query.
var keywords = map[string]lsp.SymbolKind{
	"package": lsp.SKPackage,
	"type":    lsp.SKClass,
	"method":  lsp.SKMethod,
	"field":   lsp.SKField,
	"func":    lsp.SKFunction,
	"var":     lsp.SKVariable,
	"const":   lsp.SKConstant,
}

type symbolPair struct {
	lsp.SymbolInformation
	desc symbolDescriptor
}

// resultSorter is a utility struct for collecting, filtering, and
// sorting symbol results.
type resultSorter struct {
	Query
	results   []scoredSymbol
	resultsMu sync.Mutex
}

// scoredSymbol is a symbol with an attached search relevancy score.
// It is used internally by resultSorter.
type scoredSymbol struct {
	score int
	symbolPair
}

/*
 * sort.Interface methods
 */
func (s *resultSorter) Len() int { return len(s.results) }
func (s *resultSorter) Less(i, j int) bool {
	iscore, jscore := s.results[i].score, s.results[j].score
	if iscore == jscore {
		if s.results[i].ContainerName == s.results[j].ContainerName {
			if s.results[i].Name == s.results[j].Name {
				return s.results[i].Location.URI < s.results[j].Location.URI
			}
			return s.results[i].Name < s.results[j].Name
		}
		return s.results[i].ContainerName < s.results[j].ContainerName
	}
	return iscore > jscore
}
func (s *resultSorter) Swap(i, j int) {
	s.results[i], s.results[j] = s.results[j], s.results[i]
}

// Collect is a thread-safe method that will record the passed-in
// symbol in the list of results if its score > 0.
func (s *resultSorter) Collect(si symbolPair) {
	s.resultsMu.Lock()
	score := score(s.Query, si)
	if score > 0 {
		sc := scoredSymbol{score, si}
		s.results = append(s.results, sc)
	}
	s.resultsMu.Unlock()
}

// Results returns the ranked list of SymbolInformation values.
func (s *resultSorter) Results() []lsp.SymbolInformation {
	res := make([]lsp.SymbolInformation, len(s.results))
	for i, s := range s.results {
		res[i] = s.SymbolInformation
	}
	return res
}

// score returns 0 for results that aren't matches. Results that are matches are assigned
// a positive score, which should be used for ranking purposes.
func score(q Query, s symbolPair) (scor int) {
	if q.Kind != 0 {
		if q.Kind != s.Kind {
			return 0
		}
	}
	if q.Symbol != nil && !s.desc.Contains(q.Symbol) {
		return -1
	}
	name, container := strings.ToLower(s.Name), strings.ToLower(s.ContainerName)
	if !util.IsURI(s.Location.URI) {
		log.Printf("unexpectedly saw symbol defined at a non-file URI: %q", s.Location.URI)
		return 0
	}
	filename := util.UriToPath(s.Location.URI)
	//if q.Filter == FilterExported {
	//	// is:exported excludes vendor symbols always.
	//	return 0
	//}
	if q.File != "" && filename != q.File {
		// We're restricting results to a single file, and this isn't it.
		return 0
	}
	if len(q.Tokens) == 0 { // early return if empty query
		return 2
	}
	for i, tok := range q.Tokens {
		tok := strings.ToLower(tok)
		if strings.HasPrefix(container, tok) {
			scor += 2
		}
		if strings.HasPrefix(name, tok) {
			scor += 3
		}
		if strings.Contains(filename, tok) && len(tok) >= 3 {
			scor++
		}
		if strings.HasPrefix(path.Base(filename), tok) && len(tok) >= 3 {
			scor += 2
		}
		if tok == name {
			if i == len(q.Tokens)-1 {
				scor += 50
			} else {
				scor += 5
			}
		}
		if tok == container {
			scor += 3
		}
	}
	if scor > 0 && !(strings.HasPrefix(filename, "vendor/") || strings.Contains(filename, "/vendor/")) {
		// boost for non-vendor symbols
		scor += 5
	}
	if scor > 0 && ast.IsExported(s.Name) {
		// boost for exported symbols
		scor++
	}
	return scor
}

// toSym returns a SymbolInformation value derived from values we get
// from visiting the Go ast.
func toSym(name string, pkg source.Package, container string, recv string, kind lsp.SymbolKind, fs *token.FileSet, pos token.Pos) symbolPair {
	var id string
	if container == "" {
		id = fmt.Sprintf("%s/-/%s", path.Clean(pkg.GetPkgPath()), name)
	} else {
		id = fmt.Sprintf("%s/-/%s/%s", path.Clean(pkg.GetPkgPath()), container, name)
	}

	return symbolPair{
		SymbolInformation: lsp.SymbolInformation{
			Name:          name,
			Kind:          kind,
			Location:      goRangeToLSPLocation(fs, pos, name),
			ContainerName: container,
		},
		// NOTE: fields must be kept in sync with workspace_refs.go:defSymbolDescriptor
		desc: symbolDescriptor{
			Vendor:      false,
			Package:     path.Clean(pkg.GetPkgPath()),
			PackageName: pkg.GetName(),
			Recv:        recv,
			Name:        name,
			ID:          id,
		},
	}
}

// handleTextDocumentSymbol handles `textDocument/documentSymbol` requests for
// the Go language server.
func (h *LangHandler) handleTextDocumentSymbol(ctx context.Context, conn jsonrpc2.JSONRPC2, req *jsonrpc2.Request, params lsp.DocumentSymbolParams) ([]lsp.SymbolInformation, error) {
	pkg, astFile, err := h.loadPackageAndAst(ctx, params.TextDocument.URI)
	if err != nil {
		return nil, err
	}

	symbols := astFileToSymbols(pkg, astFile)
	res := make([]lsp.SymbolInformation, len(symbols))
	for i, s := range symbols {
		res[i] = s.SymbolInformation
	}
	return res, nil
}

// handleSymbol handles `workspace/symbol` requests for the Go
// language server.
func (h *LangHandler) handleWorkspaceSymbol(ctx context.Context, conn jsonrpc2.JSONRPC2, req *jsonrpc2.Request, params lspext.WorkspaceSymbolParams) ([]lsp.SymbolInformation, error) {
	q := ParseQuery(params.Query)
	q.Symbol = params.Symbol
	if q.Filter == FilterDir {
		q.Dir = path.Join(h.init.RootImportPath, q.Dir)
	}
	if id, ok := q.Symbol["id"]; ok {
		// id implicitly contains a dir hint. We can use that to
		// reduce the number of files we have to parse.
		q.Dir = strings.SplitN(id.(string), "/-/", 2)[0]
		q.Filter = FilterDir
	}
	if params.Limit == 0 {
		// If no limit is specified, default to a reasonable number
		// for a user to look at. If they want more, they should
		// refine the query.
		params.Limit = 50
	}
	return h.handleSymbol(ctx, conn, req, q, params.Limit)
}

func (h *LangHandler) handleSymbol(ctx context.Context, conn jsonrpc2.JSONRPC2, req *jsonrpc2.Request, query Query, limit int) ([]lsp.SymbolInformation, error) {
	results := resultSorter{Query: query, results: make([]scoredSymbol, 0)}

	f := func(pkg source.Package) error {
		// If the context is cancelled, breaking the loop here
		// will allow us to return partial results, and
		// avoiding starting new computations.
		if ctx.Err() != nil {
			return ctx.Err()
		}

		if results.Query.File != "" {
			found := false
			for _, file := range pkg.GetFilenames() {
				if util.PathEqual(file, results.Query.File) {
					found = true
					break
				}
			}

			if !found {
				return nil
			}
		}

		if results.Query.Filter == FilterDir && !util.PathEqual(pkg.GetPkgPath(), results.Query.Dir) {
			return nil
		}

		if len(results.results) >= limit {
			return nil
		}

		h.collectFromPkg(pkg, &results)

		return nil
	}

	err := h.project.Search(f)
	if err != nil {
		return nil, err
	}

	sort.Sort(&results)
	if len(results.results) > limit && limit > 0 {
		results.results = results.results[:limit]
	}

	return results.Results(), nil
}

// collectFromPkg collects all the symbols from the specified package
// into the results. It uses LangHandler's package symbol cache to
// speed up repeated calls.
func (h *LangHandler) collectFromPkg(pkg source.Package, results *resultSorter) {
	symbols := astPkgToSymbols(pkg)
	if symbols == nil {
		return
	}

	for _, sym := range symbols {
		if results.Query.Filter == FilterExported && !isExported(&sym) {
			continue
		}
		results.Collect(sym)
	}
}

// SymbolCollector stores symbol information for an AST
type SymbolCollector struct {
	pkgSyms []symbolPair
	pkg     source.Package
	fs      *token.FileSet
}

func recvString(recv ast.Expr) string {
	switch t := recv.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		return "*" + recvString(t.X)
	}
	return "BADRECV"
}

func specNames(specs []ast.Spec) []string {
	names := make([]string, 0, len(specs))
	for _, s := range specs {
		// s guaranteed to be an *ast.ValueSpec by readValue
		for _, ident := range s.(*ast.ValueSpec).Names {
			names = append(names, ident.Name)
		}
	}
	return names
}

func (c *SymbolCollector) addSymbol(name string, recv string, container string, kind lsp.SymbolKind, pos token.Pos) {
	c.pkgSyms = append(c.pkgSyms, toSym(name, c.pkg, recv, container, kind, c.fs, pos))
}

func (c *SymbolCollector) addFuncDecl(fun *ast.FuncDecl) {
	if fun.Recv != nil {
		// methods
		recvTypeName := ""
		var typ ast.Expr
		if list := fun.Recv.List; len(list) == 1 {
			typ = list[0].Type
		}
		recvTypeName = recvString(typ)
		c.addSymbol(fun.Name.Name, recvTypeName, recvTypeName, lsp.SKMethod, fun.Name.NamePos)
		return
	}
	// ordinary function
	c.addSymbol(fun.Name.Name, "", "", lsp.SKFunction, fun.Name.NamePos)
	return
}

func (c *SymbolCollector) addContainer(containerName string, fields *ast.FieldList, containerKind lsp.SymbolKind, containerPos token.Pos) {
	if fields.List != nil {
		for _, field := range fields.List {
			if field.Names != nil {
				for _, fieldName := range field.Names {
					c.addSymbol(fieldName.Name, containerName, "", lsp.SKField, fieldName.NamePos)
				}
			}
		}
	}
	c.addSymbol(containerName, "", "", containerKind, containerPos)
}

// Visit visits AST nodes and collects symbol information
func (c *SymbolCollector) Visit(n ast.Node) (w ast.Visitor) {
	switch t := n.(type) {
	case *ast.TypeSpec:
		if t.Name.Name != "_" {
			switch term := t.Type.(type) {
			case *ast.StructType:
				c.addContainer(t.Name.Name, term.Fields, lsp.SKClass, t.Name.NamePos)
			case *ast.InterfaceType:
				c.addContainer(t.Name.Name, term.Methods, lsp.SKInterface, t.Name.NamePos)
			default:
				c.addSymbol(t.Name.Name, "", "", lsp.SKClass, t.Name.NamePos)
			}
		}
	case *ast.GenDecl:
		switch t.Tok {
		case token.CONST:
			names := specNames(t.Specs)
			for _, name := range names {
				c.addSymbol(name, "", "", lsp.SKConstant, declNamePos(t, name))
			}
		case token.VAR:
			names := specNames(t.Specs)
			for _, name := range names {
				if name != "_" {
					c.addSymbol(name, "", "", lsp.SKVariable, declNamePos(t, name))
				}
			}
		}
	case *ast.FuncDecl:
		c.addFuncDecl(t)
	}
	return c
}

func astPkgToSymbols(pkg source.Package) []symbolPair {
	var pkgSyms []symbolPair
	symbolCollector := &SymbolCollector{pkgSyms, pkg, pkg.GetFileSet()}

	for _, src := range pkg.GetSyntax() {
		ast.Walk(symbolCollector, src)
	}

	return symbolCollector.pkgSyms
}

func astFileToSymbols(pkg source.Package, astFile *ast.File) []symbolPair {
	var pkgSymbols []symbolPair
	symbolCollector := &SymbolCollector{pkgSymbols, pkg, pkg.GetFileSet()}
	ast.Walk(symbolCollector, astFile)
	return symbolCollector.pkgSyms
}

func declNamePos(decl *ast.GenDecl, name string) token.Pos {
	for _, spec := range decl.Specs {
		switch spec := spec.(type) {
		case *ast.ImportSpec:
			if spec.Name != nil {
				return spec.Name.Pos()
			}
			return spec.Path.Pos()
		case *ast.ValueSpec:
			for _, specName := range spec.Names {
				if specName.Name == name {
					return specName.NamePos
				}
			}
		case *ast.TypeSpec:
			return spec.Name.Pos()
		}
	}
	return decl.TokPos
}

func isExported(sym *symbolPair) bool {
	if sym.ContainerName == "" {
		return ast.IsExported(sym.Name)
	}
	return ast.IsExported(sym.ContainerName) && ast.IsExported(sym.Name)
}
