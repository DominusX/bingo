package langserver

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/types"

	"github.com/saibing/bingo/langserver/internal/goast"
	"github.com/saibing/bingo/langserver/internal/refs"
	"github.com/sourcegraph/go-lsp"
	"github.com/sourcegraph/jsonrpc2"
	"golang.org/x/tools/go/packages"
)

func (h *LangHandler) handleDefinition(ctx context.Context, conn jsonrpc2.JSONRPC2, req *jsonrpc2.Request, params lsp.TextDocumentPositionParams) ([]lsp.Location, error) {
	res, err := h.handleXDefinition(ctx, conn, req, params)
	if err != nil {
		return nil, err
	}
	locs := make([]lsp.Location, 0, len(res))
	for _, li := range res {
		locs = append(locs, li.Location)
	}
	return locs, nil
}

func (h *LangHandler) handleTypeDefinition(ctx context.Context, conn jsonrpc2.JSONRPC2, req *jsonrpc2.Request, params lsp.TextDocumentPositionParams) ([]lsp.Location, error) {
	res, err := h.handleXDefinition(ctx, conn, req, params)
	if err != nil {
		return nil, err
	}
	locs := make([]lsp.Location, 0, len(res))
	for _, li := range res {
		// not everything we find a definition for also has a type definition
		if li.TypeLocation.URI != "" {
			locs = append(locs, li.TypeLocation)
		}
	}
	return locs, nil
}

var testOSToVFSPath func(osPath string) string

type foundNode struct {
	ident *ast.Ident      // the lookup in Uses[] or Defs[]
	typ   *types.TypeName // the object for a named type, if present
}

func (h *LangHandler) handleXDefinition(ctx context.Context, conn jsonrpc2.JSONRPC2, req *jsonrpc2.Request, params lsp.TextDocumentPositionParams) ([]symbolLocationInformation, error) {
	symbols, err := h.doHandleXDefinition(ctx, conn, req, params)
	if err != nil {
		params.Position.Character--
		symbols, err = h.doHandleXDefinition(ctx, conn, req, params)
	}

	return symbols, err
}

func (h *LangHandler) doHandleXDefinition(ctx context.Context, conn jsonrpc2.JSONRPC2, req *jsonrpc2.Request, params lsp.TextDocumentPositionParams) ([]symbolLocationInformation, error) {
	pkg, pos, err := h.typeCheck(ctx, params.TextDocument.URI, params.Position)
	if err != nil {
		// Invalid nodes means we tried to click on something which is
		// not an ident (eg comment/string/etc). Return no locations.
		if _, ok := err.(*goast.InvalidNodeError); ok {
			return []symbolLocationInformation{}, nil
		}
		return nil, err
	}

	pathNodes, err := goast.GetPathNodes(pkg, pos, pos)
	if err != nil {
		return nil, err
	}

	firstNode := pathNodes[0]
	switch node := firstNode.(type) {
	case *ast.Ident:
		return h.lookupIdentDefinition(ctx, conn, pkg, pathNodes, node)
	case *ast.TypeSpec:
		return h.lookupIdentDefinition(ctx, conn, pkg, pathNodes, node.Name)
	case *ast.CallExpr:
		return h.lookupCallExprDefinition(ctx, conn, pkg, pathNodes, node)
	case *ast.SelectorExpr:
		return h.lookupIdentDefinition(ctx, conn, pkg, pathNodes, node.Sel)
	default:
		return nil, goast.NewInvalidNodeError(pkg, firstNode)
	}
}

func (h *LangHandler) lookupCallExprDefinition(ctx context.Context, conn jsonrpc2.JSONRPC2, pkg *packages.Package, pathNodes []ast.Node, call *ast.CallExpr) ([]symbolLocationInformation, error) {
	if ident, ok := call.Fun.(*ast.Ident); ok {
		return h.lookupIdentDefinition(ctx, conn, pkg, pathNodes, ident)
	}

	if selExpr, ok := call.Fun.(*ast.SelectorExpr); ok {
		return h.lookupIdentDefinition(ctx, conn, pkg, pathNodes, selExpr.Sel)
	}

	return nil, goast.NewInvalidNodeError(pkg, pathNodes[0])
}

func (h *LangHandler) lookupIdentDefinition(ctx context.Context, conn jsonrpc2.JSONRPC2, pkg *packages.Package, pathNodes []ast.Node, ident *ast.Ident) ([]symbolLocationInformation, error) {
	var nodes []foundNode
	obj := goast.FindIdentObject(pkg, ident)
	if obj != nil {
		if typeVar, ok := obj.(*types.Var); ok && typeVar.Embedded() {
			if t, ok := typeVar.Type().(*types.Named); ok {
				obj = t.Obj()
			}
		}

		pos := obj.Pos()
		isBuiltIn := !pos.IsValid()
		if !isBuiltIn {
			nodes = append(nodes, foundNode{
				ident: &ast.Ident{NamePos: pos, Name: obj.Name()},
				typ:   goast.TypeLookup(pkg.TypesInfo.TypeOf(ident)),
			})
		} else {
			// Builtins have an invalid Pos. Just don't emit a definition for
			// them, for now. It's not that valuable to jump to their def.
			//
			// TODO(sqs): find a way to actually emit builtin locations
			// (pointing to builtin/builtin.go).
			pkg = h.project.GetBuiltinPackage()
			if pkg == nil {
				return []symbolLocationInformation{}, nil
			}
			obj = goast.FindObject(pkg, obj)
			if obj == nil {
				return []symbolLocationInformation{}, nil
			}

			// re-look up position in `builtin` package
			pos = obj.Pos()
			nodes = append(nodes, foundNode{
				ident: &ast.Ident{NamePos: pos, Name: obj.Name()},
				typ:   goast.TypeLookup(obj.Type()),
			})
			pathNodes, _, _ = goast.GetObjectPathNode(pkg, obj)
		}
	}
	if len(nodes) == 0 {
		return nil, errors.New("definition not found")
	}
	findPackage := h.getFindPackageFunc()
	locs := make([]symbolLocationInformation, 0, len(nodes))
	for _, found := range nodes {
		// Determine location information for the ident.
		l := symbolLocationInformation{
			Location: goRangeToLSPLocation(pkg.Fset, found.ident.Pos(), found.ident.Name),
		}
		if found.typ != nil {
			// We don't get an end position, but we can assume it's comparable to
			// the length of the name, I hope.
			l.TypeLocation = goRangeToLSPLocation(pkg.Fset, found.typ.Pos(), found.typ.Name())
		}

		// Determine metadata information for the ident.
		if def, err := refs.DefInfo(pkg.Types, pkg.TypesInfo, pathNodes, found.ident.Pos()); err == nil {
			symDesc, err := defSymbolDescriptor(pkg, h.project, *def, findPackage)
			if err != nil {
				// TODO: tracing
				//log.Println("refs.DefInfo:", err)
				h.notifyLog(fmt.Sprintf("refs.DelInfo: %s", err))
			} else {
				l.Symbol = symDesc
			}
		} else {
			// TODO: tracing
			h.notifyLog(fmt.Sprintf("refs.DelInfo: %s", err))
		}
		locs = append(locs, l)
	}
	return locs, nil
}
