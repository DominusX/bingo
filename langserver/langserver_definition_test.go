package langserver

import (
	"context"
	"fmt"
	"github.com/sourcegraph/go-langserver/pkg/lspext"
	"log"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sourcegraph/go-langserver/langserver/util"

	"github.com/sourcegraph/go-langserver/pkg/lsp"
	"github.com/sourcegraph/jsonrpc2"
)

func TestDefinition(t *testing.T) {
	test := func(t *testing.T, pkgDir string, input string, output string) {
		testDefinition(t, &definitionTestCase{pkgDir: pkgDir, input: input, output: output})
	}

	t.Run("basic definition", func(t *testing.T) {
		test(t, basicPkgDir, "a.go:1:17", basicOutput("a.go:1:17-1:18"))
		test(t, basicPkgDir, "a.go:1:23", basicOutput("a.go:1:17-1:18"))
		test(t, basicPkgDir, "b.go:1:17", basicOutput("b.go:1:17-1:18"))
		test(t, basicPkgDir, "b.go:1:23", basicOutput("a.go:1:17-1:18"))
	})

	t.Run("subdirectory definition", func(t *testing.T) {
		test(t, subdirectoryPkgDir, "a.go:1:17", subdirectoryOutput("a.go:1:17-1:18"))
		test(t, subdirectoryPkgDir, "a.go:1:23", subdirectoryOutput("a.go:1:17-1:18"))
		test(t, subdirectoryPkgDir, "d2/b.go:1:98", subdirectoryOutput("d2/b.go:1:98-1:99"))
		test(t, subdirectoryPkgDir, "d2/b.go:1:106", subdirectoryOutput("a.go:1:17-1:18"))
		test(t, subdirectoryPkgDir, "d2/b.go:1:111", subdirectoryOutput("d2/b.go:1:98-1:99"))
	})

	t.Run("multiple packages in dir", func(t *testing.T) {
		test(t, multiplePkgDir, "a.go:1:17", multipleOutput("a.go:1:17-1:18"))
		test(t, multiplePkgDir, "a.go:1:23", multipleOutput("a.go:1:17-1:18"))
	})

	t.Run("go root", func(t *testing.T) {
		test(t, gorootPkgDir, "a.go:1:40", gorootOutput("src/fmt/print.go:263:6-263:13"))
	})

	t.Run("go project", func(t *testing.T) {
		test(t, goprojectPkgDir, "a/a.go:1:17", goprojectOutput("a/a.go:1:17-1:18"))
		test(t, goprojectPkgDir, "b/b.go:1:101", goprojectOutput("a/a.go:1:17-1:18"))
	})

	t.Run("go module", func(t *testing.T) {
		test(t, gomodulePkgDir, "a.go:1:57", gomoduleOutput("d.go:1:19-1:20"))
		test(t, gomodulePkgDir, "b.go:1:63", gomoduleOutput("subp/d.go:1:20-1:21"))
		test(t, gomodulePkgDir, "c.go:1:63", gomoduleOutput("dep1/d1.go:1:58-1:60"))
		test(t, gomodulePkgDir, "c.go:1:68", gomoduleOutput("dep2/d2.go:1:32-1:34"))
	})

	t.Run("type definition lookup", func(t *testing.T) {
		test(t, lookupPkgDir, "b/b.go:1:127", lookupOutput("b/b.go:1:107-1:108"))
	})
}

type definitionTestCase struct {
	pkgDir string
	input  string
	output string
}

func testDefinition(tb testing.TB, c *definitionTestCase) {
	tbRun(tb, fmt.Sprintf("definition-%s", strings.Replace(c.input, "/", "-", -1)), func(t testing.TB) {
		dir, err := filepath.Abs(c.pkgDir)
		if err != nil {
			log.Fatal("testDefinition", err)
		}
		doDefinitionTest(t, ctx, conn, util.PathToURI(dir), c.input, c.output)
	})
}

func doDefinitionTest(t testing.TB, ctx context.Context, conn *jsonrpc2.Conn, rootURI lsp.DocumentURI, pos, want string) {
	file, line, char, err := parsePos(pos)
	if err != nil {
		t.Fatal(err)
	}
	hover, err := callDefinition(ctx, conn, uriJoin(rootURI, file), line, char)
	if err != nil {
		t.Fatal(err)
	}
	if hover != want {
		t.Fatalf("got %q, want %q", hover, want)
	}
}

func callDefinition(ctx context.Context, c *jsonrpc2.Conn, uri lsp.DocumentURI, line, char int) (string, error) {
	var res locations
	err := c.Call(ctx, "textDocument/definition", lsp.TextDocumentPositionParams{
		TextDocument: lsp.TextDocumentIdentifier{URI: uri},
		Position:     lsp.Position{Line: line, Character: char},
	}, &res)
	if err != nil {
		return "", err
	}
	var str string
	for i, loc := range res {
		if loc.URI == "" {
			continue
		}
		if i != 0 {
			str += ", "
		}
		str += fmt.Sprintf("%s:%d:%d-%d:%d", loc.URI, loc.Range.Start.Line+1, loc.Range.Start.Character+1, loc.Range.End.Line+1, loc.Range.End.Character+1)
	}
	return str, nil
}

func callTypeDefinition(ctx context.Context, c *jsonrpc2.Conn, uri lsp.DocumentURI, line, char int) (string, error) {
	var res locations
	err := c.Call(ctx, "textDocument/typeDefinition", lsp.TextDocumentPositionParams{
		TextDocument: lsp.TextDocumentIdentifier{URI: uri},
		Position:     lsp.Position{Line: line, Character: char},
	}, &res)
	if err != nil {
		return "", err
	}
	var str string
	for i, loc := range res {
		if loc.URI == "" {
			continue
		}
		if i != 0 {
			str += ", "
		}
		str += fmt.Sprintf("%s:%d:%d-%d:%d", loc.URI, loc.Range.Start.Line+1, loc.Range.Start.Character+1, loc.Range.End.Line+1, loc.Range.End.Character+1)
	}
	return str, nil
}

func callXDefinition(ctx context.Context, c *jsonrpc2.Conn, uri lsp.DocumentURI, line, char int) (string, error) {
	var res []lspext.SymbolLocationInformation
	err := c.Call(ctx, "textDocument/xdefinition", lsp.TextDocumentPositionParams{
		TextDocument: lsp.TextDocumentIdentifier{URI: uri},
		Position:     lsp.Position{Line: line, Character: char},
	}, &res)
	if err != nil {
		return "", err
	}
	var str string
	for i, loc := range res {
		if loc.Location.URI == "" {
			continue
		}
		if i != 0 {
			str += ", "
		}
		str += fmt.Sprintf("%s:%d:%d %s", loc.Location.URI, loc.Location.Range.Start.Line+1, loc.Location.Range.Start.Character+1, loc.Symbol)
	}
	return str, nil
}
