package langserver

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"

	"github.com/saibing/bingo/langserver/internal/cache"
	"github.com/saibing/bingo/langserver/internal/source"
	"github.com/sourcegraph/go-lsp"
	"github.com/sourcegraph/jsonrpc2"
)

func (h *LangHandler) handleTextDocumentReferences(ctx context.Context, conn jsonrpc2.JSONRPC2, req *jsonrpc2.Request, params lsp.ReferenceParams) ([]lsp.Location, error) {
	locs, err := h.doHandleTextDocumentReferences(ctx, conn, req, params)
	if err != nil {
		// fix https://github.com/saibing/bingo/issues/32
		params.Position.Character--
		locs, err = h.doHandleTextDocumentReferences(ctx, conn, req, params)
	}
	return locs, err
}

func (h *LangHandler) doHandleTextDocumentReferences(ctx context.Context, conn jsonrpc2.JSONRPC2, req *jsonrpc2.Request, params lsp.ReferenceParams) ([]lsp.Location, error) {
	pkg, pos, err := h.typeCheck(ctx, params.TextDocument.URI, params.Position)
	if err != nil {
		// Invalid nodes means we tried to click on something which is
		// not an ident (eg comment/string/etc). Return no information.
		if _, ok := err.(*source.InvalidNodeError); ok {
			return []lsp.Location{}, nil
		}
		return nil, err
	}

	pathNodes, err := source.GetPathNodes(pkg, pkg.GetFileSet(), pos, pos)
	if err != nil {
		return nil, err
	}

	var ident *ast.Ident
	firstNode := pathNodes[0]
	switch node := firstNode.(type) {
	case *ast.Ident:
		ident = node
	case *ast.FuncDecl:
		ident = node.Name
	default:
		return nil, source.NewInvalidNodeError(pkg.GetFileSet(), firstNode)
	}

	// NOTICE: Code adapted from golang.org/x/tools/cmd/guru
	// referrers.go.
	obj := source.FindIdentObject(pkg, ident)
	if obj == nil {
		return nil, errors.New("references object not found")
	}

	if obj.Pkg() == nil {
		if _, builtin := obj.(*types.Builtin); !builtin {
			return nil, fmt.Errorf("no package found for object %s", obj)
		}
	}

	refs, err := h.findReferences(ctx, obj)
	if err != nil {
		// If we are canceled, cancel loop early
		return nil, err
	}

	if params.Context.IncludeDeclaration {
		refs = append(refs, &ast.Ident{NamePos: obj.Pos(), Name: obj.Name()})
	}

	return refStreamAndCollect(pkg.GetFileSet(), refs, params.Context.XLimit), nil
}

// refStreamAndCollect returns all refs read in from chan until it is
// closed. While it is reading, it will also occasionally stream out updates of
// the refs received so far.
func refStreamAndCollect(fset *token.FileSet, refs []*ast.Ident, limit int) []lsp.Location {
	if limit == 0 {
		// If we don't have a limit, just set it to a value we should never exceed
		limit = len(refs)
	}

	l := len(refs)
	if limit < l {
		l = limit
	}

	var locs []lsp.Location

	seen := map[string]bool{}
	for i := 0; i < l; i++ {
		n := refs[i]
		loc := goRangeToLSPLocation(fset, n.Pos(), n.Name)
		if loc.URI == "" {
			continue
		}

		// remove duplicate results because they contain uses of the xtest package
		locStr := formatLocation(loc)
		if seen[locStr] {
			continue
		}
		seen[locStr] = true
		locs = append(locs, loc)
	}

	return locs
}

func formatLocation(loc lsp.Location) string {
	return fmt.Sprintf("%s:%s", loc.URI, loc.Range)
}

// findReferences will find all references to obj. It will only return
// references from packages in pkg.Imports.
func (h *LangHandler) findReferences(ctx context.Context, queryObj types.Object) ([]*ast.Ident, error) {
	// Bail out early if the context is canceled
	var refs []*ast.Ident
	var defPkgPath string
	if queryObj.Pkg() != nil {
		defPkgPath = queryObj.Pkg().Path()
	} else {
		defPkgPath = cache.BuiltinPkg
	}

	f := func(pkg source.Package) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		if defPkgPath != cache.BuiltinPkg {
			if p := pkg.GetImport(defPkgPath); p == nil && pkg.GetPkgPath() != defPkgPath {
				return nil
			}
		}

		if pkg.GetTypesInfo() == nil {
			return nil
		}

		for id, obj := range pkg.GetTypesInfo().Uses {
			if sameObj(queryObj, obj) {
				refs = append(refs, id)
			}
		}

		return nil
	}

	err := h.project.Search(f)
	if err != nil {
		return nil, err
	}

	return refs, nil
}

// same reports whether x and y are identical, or both are PkgNames
// that import the same Package.
func sameObj(x, y types.Object) bool {
	if x == y {
		return true
	}

	if x.Pkg() != nil &&
		y.Pkg() != nil &&
		x.Pkg().Path() == y.Pkg().Path() &&
		x.Name() == y.Name() &&
		x.Exported() &&
		y.Exported() {
		// enable find the xtest pakcage's uses, but this will product some duplicate results
		return true
	}

	// builtin package symbol
	if x.Pkg() == nil &&
		y.Pkg() == nil &&
		x.Name() == y.Name() {
		return true
	}

	if x, ok := x.(*types.PkgName); ok {
		if y, ok := y.(*types.PkgName); ok {
			return x.Imported() == y.Imported()
		}
	}
	return false
}
