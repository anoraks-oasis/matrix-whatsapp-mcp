package mcpserver

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// stdoutException is the one file allowed to reach os.Stdout: the subcommand
// dispatch in cmd/server, which hands it to `rooms`/`doctor` and exits before
// any MCP server exists. Every other package writes to stderr or to an injected writer,
// because a stray byte on stdout corrupts the stdio JSON-RPC framing for every
// client and no Go test or smoke script would notice.
const stdoutException = "cmd/server/main.go"

// TestNoPackageWritesToStdout enforces the stdio invariant mechanically over
// the whole module: no package may reference os.Stdout, and no package may call
// fmt.Print/Printf/Println, which write there too. Test files are exempt (a
// test's stdout is the test's own), and fmt.Fprint/Fprintf to an explicit
// writer — what internal/cli does — is untouched.
func TestNoPackageWritesToStdout(t *testing.T) {
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
	checked := 0

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "vendor", "node_modules":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		checked++

		ast.Inspect(file, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.SelectorExpr:
				if isStdoutSelector(node) && rel != stdoutException {
					t.Errorf("%s writes to os.Stdout at %s; the server path must use stderr or an injected writer",
						rel, fset.Position(node.Pos()))
				}
			case *ast.CallExpr:
				if fn, ok := node.Fun.(*ast.SelectorExpr); ok && writesToStdout(fn) {
					t.Errorf("%s calls %s at %s; fmt.Print* writes to stdout, use an explicit writer",
						rel, fn.Sel.Name, fset.Position(node.Pos()))
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	if checked == 0 {
		t.Fatal("walked no Go files: the invariant was never actually checked")
	}
}

// isStdoutSelector reports whether node is the selector os.Stdout.
func isStdoutSelector(node *ast.SelectorExpr) bool {
	pkg, ok := node.X.(*ast.Ident)
	return ok && pkg.Name == "os" && node.Sel.Name == "Stdout"
}

// writesToStdout reports whether fn is one of fmt's stdout helpers.
func writesToStdout(fn *ast.SelectorExpr) bool {
	pkg, ok := fn.X.(*ast.Ident)
	if !ok || pkg.Name != "fmt" {
		return false
	}
	switch fn.Sel.Name {
	case "Print", "Printf", "Println":
		return true
	}
	return false
}
