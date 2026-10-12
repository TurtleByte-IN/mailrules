// Command querycover fails when a statement of the store was never run by the tests behind a
// coverage profile. `make test-postgres` runs every package's tests on Postgres with the
// store's coverage recorded, then this, so no query reaches Postgres untested: SQL the two
// dialects read differently shows up as a failing test, not in production.
//
// A statement is a call of ExecContext, QueryContext or QueryRowContext in the store's
// non-test files; it counts as run when the coverage block holding its line ran.
//
//	go run ./internal/store/querycover -dir internal/store -pkg github.com/TurtleByte-IN/mailrules/internal/store cover.out
package main

import (
	"bufio"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

func main() {
	dir := flag.String("dir", "internal/store", "the store's source directory")
	pkg := flag.String("pkg", "github.com/TurtleByte-IN/mailrules/internal/store", "its import path, as the profile names it")
	flag.Parse()
	if flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: querycover [-dir d] [-pkg p] cover.out")
		os.Exit(2)
	}
	calls, err := statements(*dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "querycover:", err)
		os.Exit(2)
	}
	ran, err := covered(flag.Arg(0), *pkg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "querycover:", err)
		os.Exit(2)
	}
	var missing []string
	for _, c := range calls {
		if !ran.has(c.file, c.line) {
			missing = append(missing, filepath.Join(*dir, c.file)+":"+strconv.Itoa(c.line))
		}
	}
	if len(missing) > 0 {
		fmt.Printf("querycover: %d of %d store statements never ran on this database:\n", len(missing), len(calls))
		for _, m := range missing {
			fmt.Println("  " + m)
		}
		os.Exit(1)
	}
	fmt.Printf("querycover: all %d store statements ran\n", len(calls))
}

type call struct {
	file string
	line int
}

// statements lists the store's statement calls, by file and line.
func statements(dir string) ([]call, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	var out []call
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return nil, err
		}
		ast.Inspect(f, func(n ast.Node) bool {
			c, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := c.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			switch sel.Sel.Name {
			case "ExecContext", "QueryContext", "QueryRowContext":
				out = append(out, call{file: filepath.Base(path), line: fset.Position(c.Pos()).Line})
			}
			return true
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].file != out[j].file {
			return out[i].file < out[j].file
		}
		return out[i].line < out[j].line
	})
	return out, nil
}

type block struct{ from, to int }

type profile map[string][]block // file to the blocks that ran

func (p profile) has(file string, line int) bool {
	for _, b := range p[file] {
		if b.from <= line && line <= b.to {
			return true
		}
	}
	return false
}

// covered reads a coverage profile: the blocks of pkg's files that ran at least once.
func covered(path, pkg string) (profile, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := profile{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		// name.go:12.34,15.2 3 1
		line := sc.Text()
		if strings.HasPrefix(line, "mode:") {
			continue
		}
		name, rest, ok := strings.Cut(line, ":")
		if !ok || filepath.Dir(name) != pkg {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) != 3 {
			return nil, fmt.Errorf("profile line %q", line)
		}
		if n, err := strconv.Atoi(fields[2]); err != nil || n == 0 {
			continue
		}
		span := strings.Split(fields[0], ",")
		if len(span) != 2 {
			return nil, fmt.Errorf("profile line %q", line)
		}
		from, err1 := strconv.Atoi(strings.Split(span[0], ".")[0])
		to, err2 := strconv.Atoi(strings.Split(span[1], ".")[0])
		if err1 != nil || err2 != nil {
			return nil, fmt.Errorf("profile line %q", line)
		}
		base := filepath.Base(name)
		out[base] = append(out[base], block{from, to})
	}
	return out, sc.Err()
}
