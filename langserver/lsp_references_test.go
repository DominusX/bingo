package langserver

import (
	"context"
	"fmt"
	"log"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/saibing/bingo/langserver/internal/cache"
	"github.com/saibing/bingo/langserver/internal/util"

	"github.com/sourcegraph/go-lsp"
	"github.com/sourcegraph/jsonrpc2"
)

var referencesContext = newTestContext(cache.Always)

func TestReferences(t *testing.T) {
	t.Parallel()

	referencesContext.setup(t)

	test := func(t *testing.T, input string, output []string) {
		testReferences(t, &referencesTestCase{input: input, output: output})
	}

	t.Run("basic", func(t *testing.T) {
		test(t, "basic/a.go:1:17", []string{"basic/a.go:1:17", "basic/a.go:1:23", "basic/b.go:1:23"})
		test(t, "basic/a.go:1:23", []string{"basic/a.go:1:17", "basic/a.go:1:23", "basic/b.go:1:23"})
		test(t, "basic/b.go:1:17", []string{"basic/b.go:1:17"})
		test(t, "basic/b.go:1:23", []string{"basic/a.go:1:17", "basic/a.go:1:23", "basic/b.go:1:23"})
	})

	t.Run("builtin", func(t *testing.T) {
		test(t, "builtin/a.go:1:26", []string{"builtin/a.go:1:23"})
	})

	t.Run("xtest", func(t *testing.T) {
		test(t, "xtest/a.go:1:16", []string{"xtest/a.go:1:16", "xtest/a_test.go:1:20", "xtest/x_test.go:1:88"})
		test(t, "xtest/x_test.go:1:88", []string{"xtest/a.go:1:16", "xtest/a_test.go:1:20", "xtest/x_test.go:1:88"})
		test(t, "xtest/x_test.go:1:82", []string{"xtest/x_test.go:1:82", "xtest/y_test.go:1:39"})
		test(t, "xtest/a_test.go:1:20", []string{"xtest/a.go:1:16", "xtest/a_test.go:1:20", "xtest/x_test.go:1:88"})
		test(t, "xtest/a_test.go:1:16", []string{"xtest/a_test.go:1:16", "xtest/b_test.go:1:34"})
	})

	t.Run("test", func(t *testing.T) {
		test(t, "test/a_test.go:1:102", []string{"test/a_test.go:1:102", "test/b/b.go:1:16", "test/b/b.go:1:45", "test/c/c.go:1:84"})
		test(t, "test/a_test.go:1:100", []string{"test/a_test.go:1:100", "test/a_test.go:1:37"})
		test(t, "test/a_test.go:1:110", []string{"test/a_test.go:1:110"})
	})

	t.Run("go project", func(t *testing.T) {
		test(t, "goproject/a/a.go:1:17", []string{"goproject/a/a.go:1:17", "goproject/b/b.go:1:89"})
		test(t, "goproject/b/b.go:1:89", []string{"goproject/a/a.go:1:17", "goproject/b/b.go:1:89"})
		test(t, "goproject/b/b.go:1:87", []string{"goproject/b/b.go:1:19", "goproject/b/b.go:1:87"})
	})

	t.Run("go module", func(t *testing.T) {
		test(t, "gomodule/a.go:1:57", []string{"gomodule/a.go:1:57", "gomodule/a.go:1:72", githubModule + "/d.go:1:19", githubModule + "/d.go:1:35"})
	})

	t.Run("unexpected paths", func(t *testing.T) {
		test(t, "unexpected_paths/a.go:1:17", []string{"unexpected_paths/a.go:1:17", "unexpected_paths/a.go:1:23"})
	})
}

type referencesTestCase struct {
	input  string
	output []string
}

func testReferences(tb testing.TB, c *referencesTestCase) {
	tbRun(tb, fmt.Sprintf("references-%s", strings.Replace(c.input, "/", "-", -1)), func(t testing.TB) {
		dir, err := filepath.Abs(referencesContext.root())
		if err != nil {
			log.Fatal("testReferences", err)
		}
		doReferencesTest(t, referencesContext.ctx, referencesContext.conn, util.PathToURI(dir), c.input, c.output)
	})
}

func doReferencesTest(t testing.TB, ctx context.Context, c *jsonrpc2.Conn, rootURI lsp.DocumentURI, pos string, want []string) {
	file, line, char, err := parsePos(pos)
	if err != nil {
		t.Fatal(err)
	}
	references, err := callReferences(ctx, c, uriJoin(rootURI, file), line, char)
	if err != nil {
		t.Fatal(err)
	}

	var results []string
	for i := range references {
		if strings.Contains(references[i], "go-build") {
			continue
		}

		if strings.Contains(references[i], "go/src") {
			continue
		}

		results = append(results, filepath.ToSlash(util.UriToRealPath(lsp.DocumentURI(references[i]))))
	}

	for i := range want {
		if strings.HasPrefix(want[i], githubModule) {
			want[i] = makePath(gopathDir, want[i])
		} else {
			want[i] = makePath(referencesContext.root(), want[i])
		}
	}
	sort.Strings(results)
	sort.Strings(want)
	if !reflect.DeepEqual(results, want) {
		t.Errorf("\ngot\n\t%q\nwant\n\t%q", results, want)
	}
}

func callReferences(ctx context.Context, c *jsonrpc2.Conn, uri lsp.DocumentURI, line, char int) ([]string, error) {
	var res locations
	err := c.Call(ctx, "textDocument/references", lsp.ReferenceParams{
		Context: lsp.ReferenceContext{IncludeDeclaration: true},
		TextDocumentPositionParams: lsp.TextDocumentPositionParams{
			TextDocument: lsp.TextDocumentIdentifier{URI: uri},
			Position:     lsp.Position{Line: line, Character: char},
		},
	}, &res)
	if err != nil {
		return nil, err
	}
	str := make([]string, len(res))
	for i, loc := range res {
		str[i] = fmt.Sprintf("%s:%d:%d", loc.URI, loc.Range.Start.Line+1, loc.Range.Start.Character+1)
	}
	return str, nil
}
