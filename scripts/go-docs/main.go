// Package main 检查 Git 候选中新增或修改的 Go 导出声明的注释。
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"slices"
	"strings"
	"unicode"
)

func main() {
	if err := run(os.Stdout, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

type declaration struct {
	name      string
	signature string
	doc       string
	line      int
}

func run(out io.Writer, args []string) error {
	flags := flag.NewFlagSet("go-docs", flag.ContinueOnError)
	baseRef := flags.String("base", "HEAD", "比较基线")
	staged := flags.Bool("staged", false, "检查暂存区")
	ref := flags.String("ref", "", "检查指定提交")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *staged && *ref != "" {
		return errors.New("--staged 与 --ref 只能指定一个")
	}
	root, err := git("", "rev-parse", "--show-toplevel")
	if err != nil {
		return err
	}
	repo := strings.TrimSpace(string(root))
	base, err := tree(repo, *baseRef)
	if err != nil {
		// 初始提交尚无 HEAD，直接审查新增声明，无需向对象库写空 tree。
		if *baseRef != "HEAD" {
			return err
		}
		if _, branchErr := git(repo, "symbolic-ref", "-q", "HEAD"); branchErr != nil {
			return err
		}
	}
	var target string
	if *ref != "" {
		target, err = tree(repo, *ref)
		if err != nil {
			return err
		}
	}
	paths, err := candidatePaths(repo, base, target, *staged, flags.Args())
	if err != nil {
		return err
	}
	var findings []error
	var checked int
	for _, name := range paths {
		before, err := version(repo, base, name, false)
		if err != nil {
			return err
		}
		selector := target
		if *staged {
			selector = ":"
		}
		after, err := version(repo, selector, name, !*staged && target == "")
		if err != nil {
			return err
		}
		if after == nil {
			continue
		}
		checked++
		findings = append(findings, checkFile(name, before, after))
	}
	if err := errors.Join(findings...); err != nil {
		return err
	}
	fmt.Fprintf(out, "Go 注释检查通过，共比较 %d 个候选文件\n", checked)
	return nil
}

func git(repo string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = repo
	output, err := cmd.Output()
	if exit, ok := errors.AsType[*exec.ExitError](err); ok {
		return nil, fmt.Errorf("git %s: %w\n%s", args[0], err, strings.TrimSpace(string(exit.Stderr)))
	}
	return output, err
}

func tree(repo, ref string) (string, error) {
	output, err := git(repo, "rev-parse", "--verify", "--end-of-options", ref+"^{tree}")
	return strings.TrimSpace(string(output)), err
}

func candidatePaths(repo, base, target string, staged bool, filters []string) ([]string, error) {
	var args []string
	switch {
	case base == "" && target != "":
		args = []string{"ls-tree", "-r", "-z", "--name-only", target}
	case base == "" && !staged:
		args = []string{"ls-files", "-z", "--cached"}
	default:
		args = []string{"diff", "--name-only", "-z", "--no-renames", "--diff-filter=AMT"}
		if staged {
			args = append(args, "--cached")
		}
		if base != "" {
			args = append(args, base)
		}
		if target != "" {
			args = append(args, target)
		}
	}
	list, err := git(repo, append(append(args, "--"), filters...)...)
	if err != nil {
		return nil, err
	}
	if target == "" && !staged {
		untracked, err := git(repo, append([]string{"ls-files", "--others", "--exclude-standard", "-z", "--"}, filters...)...)
		if err != nil {
			return nil, err
		}
		list = append(list, untracked...)
	}
	seen := make(map[string]bool)
	var paths []string
	for _, name := range strings.Split(string(list), "\x00") {
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || seen[name] {
			continue
		}
		seen[name] = true
		paths = append(paths, name)
	}
	return paths, nil
}

func version(repo, rev, name string, working bool) ([]byte, error) {
	if working {
		content, err := fs.ReadFile(os.DirFS(repo), name)
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return content, err
	}
	if rev == "" {
		return nil, nil
	}
	if rev == ":" {
		rev = ""
	}
	object := rev + ":" + name
	if _, err := git(repo, "cat-file", "-e", object); err != nil {
		if _, ok := errors.AsType[*exec.ExitError](err); ok {
			return nil, nil
		}
		return nil, err
	}
	return git(repo, "cat-file", "blob", object)
}

func checkFile(name string, oldSource, newSource []byte) error {
	current, err := collect(name, newSource)
	if err != nil || len(current) == 0 {
		return err
	}
	previous, err := collect(name, oldSource)
	if err != nil {
		return err
	}
	var findings []error
	for _, key := range slices.Sorted(maps.Keys(current)) {
		now := current[key]
		old := previous[key]
		if now.signature == old.signature && now.doc == old.doc {
			continue
		}
		if key == "package" && now.doc == "" && old.doc == "" {
			continue
		}
		if now.doc == "" {
			findings = append(findings, fmt.Errorf("%s:%d 缺少 %s 的文档注释", name, now.line, now.name))
			continue
		}
		suffix, found := strings.CutPrefix(now.doc, now.name)
		first := strings.IndexFunc(suffix, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) })
		if !found || first <= 0 {
			findings = append(findings, fmt.Errorf("%s:%d 文档注释应以 %s 开头并说明契约", name, now.line, now.name))
		} else if !strings.ContainsFunc(now.doc, func(r rune) bool { return unicode.Is(unicode.Han, r) }) {
			findings = append(findings, fmt.Errorf("%s:%d %s 的文档注释默认使用中文", name, now.line, now.name))
		}
	}
	return errors.Join(findings...)
}

func collect(name string, source []byte) (map[string]declaration, error) {
	if source == nil {
		return nil, nil
	}
	positions := token.NewFileSet()
	file, err := parser.ParseFile(positions, name, source, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil || ast.IsGenerated(file) {
		return nil, err
	}
	line := func(pos token.Pos) int { return positions.PositionFor(pos, false).Line }
	decls := map[string]declaration{
		"package": {name: "Package " + file.Name.Name, signature: file.Name.Name, doc: comment(file.Doc), line: line(file.Package)},
	}
	for _, raw := range file.Decls {
		switch item := raw.(type) {
		case *ast.FuncDecl:
			if !item.Name.IsExported() {
				continue
			}
			key := "func " + item.Name.Name
			if item.Recv != nil {
				key += " " + receiver(item.Recv.List[0].Type)
			}
			signature := *item
			signature.Doc, signature.Body = nil, nil
			decls[key] = declaration{item.Name.Name, formatted(&signature), comment(item.Doc), line(item.Pos())}
		case *ast.GenDecl:
			if item.Tok != token.TYPE {
				continue
			}
			for _, node := range item.Specs {
				spec := node.(*ast.TypeSpec)
				if !spec.Name.IsExported() {
					continue
				}
				doc := spec.Doc
				if item.Lparen == token.NoPos {
					doc = item.Doc
				}
				signature := *spec
				signature.Doc, signature.Comment = nil, nil
				decls["type "+spec.Name.Name] = declaration{spec.Name.Name, formatted(&signature), comment(doc), line(spec.Pos())}
			}
		}
	}
	return decls, nil
}

func comment(doc *ast.CommentGroup) string {
	return strings.Join(strings.Fields(doc.Text()), " ")
}

func formatted(node ast.Node) string {
	var output bytes.Buffer
	// 已解析的 AST 仅用于比较声明，忽略版式、行号及函数体。
	_ = format.Node(&output, token.NewFileSet(), node)
	return output.String()
}

func receiver(expr ast.Expr) string {
	switch item := expr.(type) {
	case *ast.StarExpr:
		return receiver(item.X)
	case *ast.IndexExpr:
		return receiver(item.X)
	case *ast.IndexListExpr:
		return receiver(item.X)
	case *ast.ParenExpr:
		return receiver(item.X)
	default:
		return formatted(expr)
	}
}
