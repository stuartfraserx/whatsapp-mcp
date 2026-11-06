// Package repotest holds repository-wide hygiene and security checks. It has
// no non-test code.
package repotest

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
)

var allowedDirectDeps = []string{
	"go.mau.fi/whatsmeow",
	"github.com/modelcontextprotocol/go-sdk",
	"modernc.org/sqlite",
	"github.com/skip2/go-qrcode",
	"golang.org/x/term",
	"golang.org/x/text",
	"google.golang.org/protobuf",
	"go.mau.fi/util",
}

const maxScannedFileSize = 10 << 20

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found in any parent of the test working directory")
		}
		dir = parent
	}
}

func requireGit(t *testing.T) string {
	t.Helper()
	bin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git binary not available")
	}
	return bin
}

func gitEnv() []string {
	return append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
	)
}

func runGit(t *testing.T, git, dir string, stdin []byte, args ...string) (stdout []byte, exitCode int) {
	t.Helper()
	cmd := exec.Command(git, args...)
	cmd.Dir = dir
	cmd.Env = gitEnv()
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	if err == nil {
		return out.Bytes(), 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if exitErr.ExitCode() == 1 {
			return out.Bytes(), 1
		}
		t.Fatalf("git %s failed (exit %d): %s", strings.Join(args, " "), exitErr.ExitCode(), errOut.String())
	}
	t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	return nil, -1
}

// isolatedGitignoreRepo returns a scratch repository holding a copy of the
// repository's root .gitignore, so check-ignore results are not influenced by
// the developer's global excludes, .git/info/exclude or nested ignore files.
func isolatedGitignoreRepo(t *testing.T, git, root string) string {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(root, ".gitignore"))
	if err != nil {
		t.Fatalf("repository root has no .gitignore: %v", err)
	}
	dir := t.TempDir()
	runGit(t, git, dir, nil, "init", "--quiet")
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func ignoredPaths(t *testing.T, git, repo string, paths []string) map[string]bool {
	t.Helper()
	input := []byte(strings.Join(paths, "\x00") + "\x00")
	out, _ := runGit(t, git, repo, input, "check-ignore", "--no-index", "-z", "--stdin")
	ignored := map[string]bool{}
	for _, p := range strings.Split(string(out), "\x00") {
		if p != "" {
			ignored[p] = true
		}
	}
	return ignored
}

func TestGitignoreCoversSensitiveArtifacts(t *testing.T) {
	git := requireGit(t)
	repo := isolatedGitignoreRepo(t, git, repoRoot(t))

	mustIgnore := []string{
		"whatsapp.db",
		"messages.db",
		"messages.db-wal",
		"messages.db-shm",
		"whatsapp.db-journal",
		"x.sqlite",
		"x.sqlite3",
		"x.sqlite3-wal",
		"pairing-qr.png",
		"qr.png",
		"whatsapp-pairing-qr.png",
		"data/whatsapp.db",
		"data/anything/at/all.txt",
		"downloads/a.jpg",
		"outbox/f.pdf",
		".env",
		".env.local",
		".env.production",
		"http.token",
		"secrets/api.token",
		"whatsapp-mcp-data/x",
		"whatsapp-mcp-data/whatsapp.db",
		"internal/store/messages.db",
		"internal/qr/pairing-qr.png",
		"cmd/serve/.env",
		"server.pem",
		"server.key",
	}
	ignored := ignoredPaths(t, git, repo, mustIgnore)
	for _, p := range mustIgnore {
		if !ignored[p] {
			t.Errorf(".gitignore does not ignore %q", p)
		}
	}

	mustTrack := []string{
		"go.mod",
		"go.sum",
		"README.md",
		"LICENSE",
		"NOTICE.md",
		"cmd/whatsapp-mcp/main.go",
		"internal/store/store.go",
		"internal/store/schema.go",
		"internal/store/store_test.go",
		"internal/qr/qr.go",
		"internal/datadir/datadir.go",
		"internal/config/config.go",
		"internal/wa/wa.go",
		"examples/mcp-config.stdio.json",
		".gitignore",
	}
	ignored = ignoredPaths(t, git, repo, mustTrack)
	for _, p := range mustTrack {
		if ignored[p] {
			t.Errorf(".gitignore is too broad: source file %q would be ignored", p)
		}
	}
}

func candidateFiles(t *testing.T, git, root string, args ...string) []string {
	t.Helper()
	if _, err := os.Stat(filepath.Join(root, ".git")); err != nil {
		t.Skip("not a git checkout")
	}
	cmd := append([]string{"ls-files", "-z"}, args...)
	out, _ := runGit(t, git, root, nil, cmd...)
	var files []string
	for _, f := range strings.Split(string(out), "\x00") {
		if f != "" {
			files = append(files, f)
		}
	}
	return files
}

var sensitiveBaseGlobs = []string{
	"*.db", "*.db-*", "*.sqlite*", "*.token", ".env", ".env.*", "*qr*.png", "*.pem", "*.key",
}

var sensitiveDirNames = []string{"data", "downloads", "outbox", "whatsapp-mcp-data"}

func sensitiveReason(file string) string {
	slash := filepath.ToSlash(file)
	parts := strings.Split(slash, "/")
	for _, dir := range parts[:len(parts)-1] {
		if slices.Contains(sensitiveDirNames, dir) {
			return "lives in the runtime directory " + strconv.Quote(dir)
		}
	}
	base := parts[len(parts)-1]
	for _, glob := range sensitiveBaseGlobs {
		if ok, _ := path.Match(glob, base); ok {
			return "matches the sensitive pattern " + strconv.Quote(glob)
		}
	}
	return ""
}

func TestSensitiveFileClassifier(t *testing.T) {
	t.Parallel()
	for _, f := range []string{"whatsapp.db", "a/messages.db-wal", "x.sqlite3", "pairing-qr.png", "data/x.go", "a/outbox/f.pdf", ".env", ".env.local", "http.token", "k.pem", "k.key", "whatsapp-mcp-data/x"} {
		if sensitiveReason(f) == "" {
			t.Errorf("classifier misses %q", f)
		}
	}
	for _, f := range []string{"internal/store/store.go", "go.mod", "README.md", "internal/qr/qr.go", "env.go", "example-use.png", "dataset.go"} {
		if r := sensitiveReason(f); r != "" {
			t.Errorf("classifier flags %q: %s", f, r)
		}
	}
}

func TestNoSensitiveFilesAreTrackedOrPendingCommit(t *testing.T) {
	git := requireGit(t)
	root := repoRoot(t)

	tracked := candidateFiles(t, git, root, "--cached")
	for _, f := range tracked {
		if reason := sensitiveReason(f); reason != "" {
			t.Errorf("tracked file %q %s", f, reason)
		}
	}

	repo := isolatedGitignoreRepo(t, git, root)
	ignored := ignoredPaths(t, git, repo, tracked)
	for _, f := range tracked {
		if ignored[f] {
			t.Errorf("tracked file %q is matched by .gitignore; ignored files must not be committed", f)
		}
	}

	pending := candidateFiles(t, git, root, "--cached", "--others", "--exclude-standard")
	for _, f := range pending {
		if reason := sensitiveReason(f); reason != "" {
			t.Errorf("file %q %s and is not ignored; it would be committed", f, reason)
		}
	}
}

var privateKeyHeader = regexp.MustCompile("-----BEGIN [A-Z0-9 ]*" + "PRIVATE KEY-----")

func TestPrivateKeyHeaderPattern(t *testing.T) {
	t.Parallel()
	begin := "-----BEGIN "
	for _, s := range []string{
		begin + "PRIVATE KEY-----",
		begin + "RSA PRIVATE KEY-----",
		begin + "OPENSSH PRIVATE KEY-----",
		begin + "EC PRIVATE KEY-----",
		"prefix\n" + begin + "ENCRYPTED PRIVATE KEY-----\nMIIE",
	} {
		if !privateKeyHeader.MatchString(s) {
			t.Errorf("pattern misses %q", s)
		}
	}
	for _, s := range []string{begin + "CERTIFICATE-----", begin + "PUBLIC KEY-----", "PRIVATE KEY", ""} {
		if privateKeyHeader.MatchString(s) {
			t.Errorf("pattern flags %q", s)
		}
	}
}

func TestNoPrivateKeysInCommittableFiles(t *testing.T) {
	git := requireGit(t)
	root := repoRoot(t)
	files := candidateFiles(t, git, root, "--cached", "--others", "--exclude-standard")
	scanned := 0
	for _, f := range files {
		full := filepath.Join(root, f)
		info, err := os.Lstat(full)
		if err != nil || !info.Mode().IsRegular() || info.Size() > maxScannedFileSize {
			continue
		}
		data, err := os.ReadFile(full)
		if err != nil {
			t.Errorf("read %s: %v", f, err)
			continue
		}
		scanned++
		if loc := privateKeyHeader.FindIndex(data); loc != nil {
			line := bytes.Count(data[:loc[0]], []byte("\n")) + 1
			t.Errorf("%s:%d contains a private key header; never commit key material", f, line)
		}
	}
	if scanned == 0 {
		t.Fatal("no files were scanned; the private key check would be vacuous")
	}
}

var forbiddenHTTPSelectors = map[string]bool{
	"Get": true, "Post": true, "PostForm": true, "Head": true,
	"DefaultClient": true, "DefaultTransport": true, "Client": true, "Transport": true,
	"NewRequest": true, "NewRequestWithContext": true,
}

func isHTTPImport(p string) bool { return p == "net/http" || strings.HasPrefix(p, "net/http/") }

func isServerPackage(rel string) bool {
	return strings.HasPrefix(rel, "internal/server/")
}

// scanGoSource returns the policy violations of one non-test Go file whose
// slash-separated path relative to the module root is rel.
func scanGoSource(rel string, src any) ([]string, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, rel, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	var violations []string
	report := func(pos token.Pos, format string, args ...any) {
		violations = append(violations, fmt.Sprintf("%s: %s", fset.Position(pos), fmt.Sprintf(format, args...)))
	}

	httpName := ""
	for _, imp := range file.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			return nil, err
		}
		switch {
		case p == "C":
			report(imp.Pos(), `imports "C"; the module must build with CGO_ENABLED=0`)
		case isHTTPImport(p) && !isServerPackage(rel) && rel != "internal/notify/notify.go":
			where := "outside internal/server"
			if strings.HasPrefix(rel, "cmd/") {
				where = "in a cmd/ package; only internal/server may use net/http"
			}
			report(imp.Pos(), "imports %q %s", p, where)
		}
		if p != "net/http" {
			continue
		}
		name := "http"
		if imp.Name != nil {
			name = imp.Name.Name
		}
		switch name {
		case ".":
			report(imp.Pos(), `dot-importing "net/http" hides client usage from review`)
		case "_":
		default:
			httpName = name
		}
	}
	if rel == "internal/notify/notify.go" || rel == "internal/server/proxy.go" {
		return violations, nil
	}
	if httpName == "" {
		return violations, nil
	}

	shadowed := shadowedNames(file, httpName)
	ast.Inspect(file, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		id, ok := sel.X.(*ast.Ident)
		if !ok || id.Name != httpName || !forbiddenHTTPSelectors[sel.Sel.Name] {
			return true
		}
		if shadowed[id] {
			return true
		}
		report(sel.Pos(), "uses net/http client API %s.%s; this server must never make outbound HTTP requests", httpName, sel.Sel.Name)
		return true
	})
	return violations, nil
}

// shadowedNames returns the identifiers named name that refer to a local
// declaration rather than the imported package. Declarations it does not model
// (such as an if-statement initializer) are not treated as shadowing, so the
// scan errs on the side of reporting.
func shadowedNames(file *ast.File, name string) map[*ast.Ident]bool {
	shadowed := map[*ast.Ident]bool{}
	var scopes []map[string]bool
	declared := func() bool {
		for _, s := range scopes {
			if s[name] {
				return true
			}
		}
		return false
	}
	push := func(scope map[string]bool) { scopes = append(scopes, scope) }
	pop := func() { scopes = scopes[:len(scopes)-1] }
	check := func(n ast.Node) {
		if sel, ok := n.(*ast.SelectorExpr); ok {
			if id, ok := sel.X.(*ast.Ident); ok && id.Name == name && declared() {
				shadowed[id] = true
			}
		}
	}

	var walk func(n ast.Node)
	walk = func(n ast.Node) {
		switch x := n.(type) {
		case *ast.BlockStmt:
			push(map[string]bool{})
			for _, st := range x.List {
				walk(st)
				recordDecl(st, scopes[len(scopes)-1])
			}
			pop()
		case *ast.FuncDecl:
			scope := map[string]bool{}
			collectFieldNames(x.Recv, scope)
			collectFieldNames(x.Type.Params, scope)
			collectFieldNames(x.Type.Results, scope)
			push(scope)
			if x.Body != nil {
				walk(x.Body)
			}
			pop()
		case *ast.FuncLit:
			scope := map[string]bool{}
			collectFieldNames(x.Type.Params, scope)
			collectFieldNames(x.Type.Results, scope)
			push(scope)
			walk(x.Body)
			pop()
		default:
			check(n)
			ast.Inspect(n, func(c ast.Node) bool {
				if c == nil || c == n {
					return true
				}
				switch c.(type) {
				case *ast.BlockStmt, *ast.FuncDecl, *ast.FuncLit:
					walk(c)
					return false
				}
				check(c)
				return true
			})
		}
	}
	for _, decl := range file.Decls {
		walk(decl)
	}
	return shadowed
}

func collectFieldNames(fl *ast.FieldList, into map[string]bool) {
	if fl == nil {
		return
	}
	for _, f := range fl.List {
		for _, n := range f.Names {
			into[n.Name] = true
		}
	}
}

func recordDecl(st ast.Stmt, into map[string]bool) {
	switch s := st.(type) {
	case *ast.AssignStmt:
		if s.Tok == token.DEFINE {
			for _, l := range s.Lhs {
				if id, ok := l.(*ast.Ident); ok {
					into[id.Name] = true
				}
			}
		}
	case *ast.DeclStmt:
		if gd, ok := s.Decl.(*ast.GenDecl); ok {
			for _, spec := range gd.Specs {
				if vs, ok := spec.(*ast.ValueSpec); ok {
					for _, n := range vs.Names {
						into[n.Name] = true
					}
				}
			}
		}
	}
}

func TestGoSourceScannerDetectsViolations(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		rel  string
		src  string
		want []string
	}{
		{
			name: "http.Get in server",
			rel:  "internal/server/x.go",
			src:  "package server\nimport \"net/http\"\nfunc f() { _, _ = http.Get(\"u\") }\n",
			want: []string{"http.Get"},
		},
		{
			name: "each forbidden selector",
			rel:  "internal/server/x.go",
			src: "package server\nimport \"net/http\"\nvar _ = http.Post\nvar _ = http.PostForm\nvar _ = http.Head\n" +
				"var _ = http.DefaultClient\nvar _ = http.DefaultTransport\nvar _ = &http.Client{}\nvar _ = &http.Transport{}\n" +
				"var _ = http.NewRequest\nvar _ = http.NewRequestWithContext\n",
			want: []string{"http.Post", "http.PostForm", "http.Head", "http.DefaultClient", "http.DefaultTransport", "http.Client", "http.Transport", "http.NewRequest", "http.NewRequestWithContext"},
		},
		{
			name: "aliased import",
			rel:  "internal/server/x.go",
			src:  "package server\nimport h \"net/http\"\nfunc f() { _, _ = h.Post(\"u\", \"t\", nil) }\n",
			want: []string{"h.Post"},
		},
		{
			name: "server APIs are fine",
			rel:  "internal/server/x.go",
			src: "package server\nimport \"net/http\"\nfunc f(w http.ResponseWriter, r *http.Request) http.Handler {\n" +
				"w.WriteHeader(http.StatusOK)\nhttp.Error(w, \"x\", http.StatusTeapot)\n_ = http.NewServeMux()\n_ = &http.Server{}\nreturn http.HandlerFunc(nil)\n}\n",
		},
		{
			name: "local variable named http is not the package",
			rel:  "internal/server/x.go",
			src: "package server\nimport h \"net/http\"\nvar _ = h.StatusOK\ntype t struct{ Get func() }\n" +
				"func f() { http := t{}; http.Get() }\nfunc g(http t) { http.Get() }\n",
		},
		{
			name: "local variables shadowing the package name",
			rel:  "internal/server/x.go",
			src: "package server\nimport \"net/http\"\nvar _ = http.StatusOK\ntype t struct{ Get func() }\n" +
				"func f() { http := t{}; http.Get() }\nfunc g(http t) { http.Get() }\nfunc h() { func(http t) { http.Get() }(t{}) }\n",
		},
		{
			name: "package use after a shadowing function",
			rel:  "internal/server/x.go",
			src: "package server\nimport \"net/http\"\ntype t struct{ Get func() }\n" +
				"func f(http t) { http.Get() }\nfunc g() { _, _ = http.Get(\"u\") }\n",
			want: []string{"http.Get"},
		},
		{
			name: "net/http outside server",
			rel:  "internal/tools/x.go",
			src:  "package tools\nimport \"net/http\"\nvar _ = http.StatusOK\n",
			want: []string{`imports "net/http" outside internal/server`},
		},
		{
			name: "net/http subpackage outside server",
			rel:  "internal/session/x.go",
			src:  "package session\nimport _ \"net/http/pprof\"\n",
			want: []string{`imports "net/http/pprof" outside internal/server`},
		},
		{
			name: "net/http in cmd",
			rel:  "cmd/whatsapp-mcp/main.go",
			src:  "package main\nimport \"net/http\"\nvar _ = http.StatusOK\nfunc main() {}\n",
			want: []string{`imports "net/http" in a cmd/ package`},
		},
		{
			name: "blank import outside server",
			rel:  "internal/wa/x.go",
			src:  "package wa\nimport _ \"net/http\"\n",
			want: []string{`imports "net/http" outside internal/server`},
		},
		{
			name: "dot import in server",
			rel:  "internal/server/x.go",
			src:  "package server\nimport . \"net/http\"\nvar _ = StatusOK\n",
			want: []string{"dot-importing"},
		},
		{
			name: "cgo",
			rel:  "internal/store/x.go",
			src:  "package store\n// #include <stdio.h>\nimport \"C\"\n",
			want: []string{`imports "C"`},
		},
		{
			name: "no net/http at all",
			rel:  "internal/wa/x.go",
			src:  "package wa\nimport \"net\"\nvar _ = net.IPv4len\ntype http struct{ Get int }\nvar _ = http{}.Get\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := scanGoSource(tt.rel, tt.src)
			if err != nil {
				t.Fatalf("scan: %v", err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("violations = %q, want %d matching %q", got, len(tt.want), tt.want)
			}
			for i, w := range tt.want {
				if !strings.Contains(got[i], w) {
					t.Errorf("violation %d = %q, want it to contain %q", i, got[i], w)
				}
			}
		})
	}
}

func goSourceFiles(t *testing.T, root string) []string {
	t.Helper()
	skipDirs := map[string]bool{".git": true, "vendor": true, "reference": true, "testdata": true, "node_modules": true}
	var files []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p == root {
				return nil
			}
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			if _, err := os.Stat(filepath.Join(p, "go.mod")); err == nil {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(d.Name(), ".go") && !strings.HasSuffix(d.Name(), "_test.go") {
			rel, err := filepath.Rel(root, p)
			if err != nil {
				return err
			}
			files = append(files, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(files)
	return files
}

func TestNoOutboundHTTPClientOrCgoInModule(t *testing.T) {
	root := repoRoot(t)
	files := goSourceFiles(t, root)
	for _, must := range []string{"internal/config/config.go", "internal/store/store.go", "internal/wa/wa.go"} {
		if !slices.Contains(files, must) {
			t.Fatalf("scan did not visit %s; the walk is broken (visited %d files)", must, len(files))
		}
	}
	for _, rel := range files {
		src, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Errorf("read %s: %v", rel, err)
			continue
		}
		violations, err := scanGoSource(rel, src)
		if err != nil {
			t.Errorf("parse %s: %v", rel, err)
			continue
		}
		for _, v := range violations {
			t.Error(v)
		}
	}
}

type goModRequire struct {
	Path     string
	Indirect bool
}

func parseGoModRequires(text string) []goModRequire {
	var reqs []goModRequire
	block := ""
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		comment := ""
		if i := strings.Index(line, "//"); i >= 0 {
			comment = line[i+2:]
			line = strings.TrimSpace(line[:i])
		}
		if line == "" {
			continue
		}
		if block != "" {
			if line == ")" {
				block = ""
				continue
			}
			if block == "require" {
				reqs = append(reqs, requireFromFields(strings.Fields(line), comment))
			}
			continue
		}
		fields := strings.Fields(line)
		switch {
		case len(fields) == 2 && fields[1] == "(":
			block = fields[0]
		case fields[0] == "require" && len(fields) >= 3:
			reqs = append(reqs, requireFromFields(fields[1:], comment))
		}
	}
	return reqs
}

func requireFromFields(fields []string, comment string) goModRequire {
	return goModRequire{
		Path:     strings.Trim(fields[0], `"`),
		Indirect: strings.Contains(comment, "indirect"),
	}
}

func TestGoModParser(t *testing.T) {
	t.Parallel()
	text := `module example.com/m

go 1.26.0

require github.com/direct/one v1.0.0

require (
	github.com/direct/two v1.2.3
	github.com/indirect/three v0.1.0 // indirect
	"quoted.example/four" v2.0.0
)

replace github.com/direct/two => ../two

exclude (
	github.com/excluded/five v1.0.0
)

require github.com/indirect/six v1.0.0 // indirect
`
	got := parseGoModRequires(text)
	want := []goModRequire{
		{"github.com/direct/one", false},
		{"github.com/direct/two", false},
		{"github.com/indirect/three", true},
		{"quoted.example/four", false},
		{"github.com/indirect/six", true},
	}
	if !slices.Equal(got, want) {
		t.Errorf("parsed requires = %+v, want %+v", got, want)
	}
}

func TestDirectDependenciesAreAllowlisted(t *testing.T) {
	root := repoRoot(t)
	data, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	reqs := parseGoModRequires(string(data))
	if len(reqs) == 0 {
		t.Fatal("no requirements parsed from go.mod; the dependency check would be vacuous")
	}
	for _, r := range reqs {
		if r.Indirect {
			continue
		}
		if !slices.Contains(allowedDirectDeps, r.Path) {
			t.Errorf("direct dependency %q is not on the allowlist. A new dependency can bring network-capable code "+
				"(HTTP clients, telemetry, update checks) into a server that handles WhatsApp traffic: review it, then add it to "+
				"allowedDirectDeps in internal/repotest/hygiene_test.go", r.Path)
		}
	}
}

func TestGoModStaysPureGoWithoutToolchainLine(t *testing.T) {
	root := repoRoot(t)
	data, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range parseGoModRequires(string(data)) {
		if r.Path == "github.com/mattn/go-sqlite3" {
			t.Errorf("go.mod requires the CGO sqlite driver %q; use modernc.org/sqlite", r.Path)
		}
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "toolchain ") {
			t.Errorf("go.mod has a %q line; remove it (go directive alone pins the minimum version)", strings.TrimSpace(line))
		}
	}
}
