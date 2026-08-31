//go:build unit

package observability_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestExportedBoundaryTakesNoDefinedLoggerType is the regression test for the
// defect class this module's v4 exists to eliminate.
//
// # The defect
//
// Go matches the types inside a method or function signature NOMINALLY. A
// parameter declared as log.Level or log.Field binds the caller to THIS
// module's version of that type: v3/log.Level and v4/log.Level are different
// types even when the source is byte-for-byte identical. Any consumer that
// wanted to hand us a logger therefore had to import us, and importing us,
// inherited our major version. A Fiber upgrade in middleware/ then renamed
// log.Level along with everything else and rewrote hundreds of files across
// the fleet that had nothing to do with Fiber.
//
// # The rule
//
// A parameter that accepts a logger or a metrics recorder must be declared
// with universal types only - predeclared types, stdlib types, or a local
// interface whose own methods use universal types. It must never name a type
// DEFINED by this module.
//
// This test walks every exported function, method and interface method in the
// module and fails if a logger- or recorder-shaped parameter names one of the
// module's own defined types. It is deliberately shaped as a denylist of
// parameter names and defined types rather than a signature snapshot, so it
// keeps working as the API grows.
func TestExportedBoundaryTakesNoDefinedLoggerType(t *testing.T) {
	t.Parallel()

	// A logger/recorder parameter may name a locally-declared INTERFACE, as
	// long as that interface's own method signatures are themselves built
	// from universal types only - that is the whole shape of the fix
	// (runtime.Logger, assert.Logger, metrics.Recorder, log.Universal). What
	// it may never name is a defined NON-interface type, or an interface that
	// smuggles a defined type back in through one of its methods.
	//
	// declared maps every type name declared anywhere in this module to its
	// declaration, so the checker can tell those two cases apart instead of
	// guessing from the name.
	root, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}

	declared := collectDeclaredTypes(t, root)

	// Parameter names that indicate a logger or recorder is being accepted.
	loggerish := []string{"logger", "log", "l", "factory", "recorder", "metricsfactory", "metrics"}

	fset := token.NewFileSet()
	violations := 0

	walkErr := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if entry.IsDir() {
			if name := entry.Name(); name != "." && (strings.HasPrefix(name, ".") || name == "scripts" || name == "docs") {
				return filepath.SkipDir
			}

			return nil
		}

		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		file, parseErr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if parseErr != nil {
			return parseErr
		}

		rel, _ := filepath.Rel(root, path)
		scope := newPkgScope(declared, file)

		ast.Inspect(file, func(node ast.Node) bool {
			switch typed := node.(type) {
			case *ast.FuncDecl:
				if !typed.Name.IsExported() || !exportedReceiver(typed) {
					return true
				}

				violations += checkParams(t, fset, rel, typed.Name.Name, typed.Type.Params, loggerish, scope)
			case *ast.InterfaceType:
				for _, method := range typed.Methods.List {
					fn, ok := method.Type.(*ast.FuncType)
					if !ok || len(method.Names) == 0 || !method.Names[0].IsExported() {
						continue
					}

					violations += checkParams(t, fset, rel, method.Names[0].Name, fn.Params, loggerish, scope)
				}
			}

			return true
		})

		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk: %v", walkErr)
	}

	if violations > 0 {
		t.Fatalf("%d exported logger/recorder parameter(s) name a type defined by this module; "+
			"each one re-couples every consumer to this module's major version", violations)
	}
}

// exportedReceiver reports whether a method is reachable from outside the
// module: a plain function, or a method on an exported type.
func exportedReceiver(fn *ast.FuncDecl) bool {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return true
	}

	name := receiverTypeName(fn.Recv.List[0].Type)

	return name != "" && ast.IsExported(name)
}

func receiverTypeName(expr ast.Expr) string {
	switch typed := expr.(type) {
	case *ast.StarExpr:
		return receiverTypeName(typed.X)
	case *ast.Ident:
		return typed.Name
	case *ast.IndexExpr:
		return receiverTypeName(typed.X)
	default:
		return ""
	}
}

// checkParams reports every logger- or recorder-shaped parameter that is not
// universal, and returns how many it found.
func checkParams(
	t *testing.T,
	fset *token.FileSet,
	file, fnName string,
	params *ast.FieldList,
	loggerish []string,
	scope *pkgScope,
) int {
	t.Helper()

	if params == nil {
		return 0
	}

	found := 0

	for _, param := range params.List {
		// The name heuristic is a convenience for defined types a consumer
		// never has to name (Level and PanicPolicy are reachable through
		// untyped constants, Metric through a composite literal it may
		// legitimately want). It must NOT gate interfaces: a parameter whose
		// type is an interface declared by this module forces every consumer
		// to import the module in order to IMPLEMENT it, whatever the
		// parameter happens to be called.
		if !isLoggerish(param.Names, loggerish) && !namesLocalInterface(param.Type, scope) {
			continue
		}

		if reason := universalityViolation(param.Type, scope, 0); reason != "" {
			t.Errorf("%s:%d: %s takes parameter %q which is not universal.\n\t%s",
				file, fset.Position(param.Pos()).Line, fnName, paramNames(param.Names), reason)

			found++
		}
	}

	return found
}

// universalityViolation returns a non-empty explanation if expr names a type
// DEFINED by this module in a way a consumer cannot reproduce in its own
// package, or "" if the type is universal.
//
// The recursion is what makes the rule precise rather than name-based:
//
//   - a predeclared or stdlib type (int, string, any, context.Context) is
//     universal;
//   - a locally-declared INTERFACE is universal if every one of its own method
//     signatures is universal, and NOT universal if it has a self-returning
//     method, which a consumer has no way to name;
//   - any other locally-declared type - a struct, a named uint8 - is not
//     universal: its identity is nominal and version-bound.
func universalityViolation(expr ast.Expr, scope *pkgScope, depth int) string {
	if depth > maxTypeDepth {
		return ""
	}

	for _, name := range scope.qualify(expr) {
		decl, isLocal := scope.declared[name]
		if !isLocal {
			// Predeclared (int, string, any) or from another module (otel,
			// context). Either way its identity is not ours to propagate.
			continue
		}

		iface, isInterface := decl.expr.(*ast.InterfaceType)
		if !isInterface {
			return "it names " + name + ", a non-interface type defined by this module. " +
				"Its identity is nominal, so every consumer that names it inherits this module's " +
				"major version. Accept a local interface built from universal types instead " +
				"(see log.Universal, metrics.Recorder)."
		}

		for _, method := range iface.Methods.List {
			// Method signatures must be resolved in the scope of the file
			// that DECLARED the interface, not the file that accepts it as a
			// parameter: the two need not import this module under the same
			// alias, or at all.
			inner := scope.at(decl)

			fn, ok := method.Type.(*ast.FuncType)
			if !ok {
				// An embedded interface, not a method. Embedding carries the
				// embedded interface's whole method set, so embedding a
				// self-returning or otherwise non-universal interface couples
				// the consumer exactly as naming it directly would.
				if reason := universalityViolation(method.Type, inner, depth+1); reason != "" {
					return "it names " + name + ", whose embedded interface is not universal: " + reason
				}

				continue
			}

			for _, results := range fieldTypes(fn.Results) {
				for _, resultName := range inner.qualify(results) {
					if resultName == name {
						return "it names " + name + ", an interface with a self-returning method. " +
							"A consumer cannot declare that interface in its own package - it has no " +
							"way to name the return type - so it must import this module. Accept a " +
							"non-self-returning interface (see log.Universal) and convert with log.Adapt."
					}
				}

				// A result that is not self-referential can still be a defined
				// type, which binds an implementer just as tightly.
				if reason := universalityViolation(results, inner, depth+1); reason != "" {
					return "it names " + name + ", whose method " + methodName(method) +
						" returns a non-universal type: " + reason
				}
			}

			for _, param := range fieldTypes(fn.Params) {
				if reason := universalityViolation(param, inner, depth+1); reason != "" {
					return "it names " + name + ", whose method " + methodName(method) +
						" is not universal: " + reason
				}
			}
		}
	}

	return ""
}

// maxTypeDepth bounds the interface-method recursion against a cyclic
// declaration.
const maxTypeDepth = 8

// collectDeclaredTypes maps every type declared in this module to the
// expression it is declared as, keyed "package.TypeName".
//
// The key MUST be package-qualified. Keying by bare name would conflate
// log.Logger - the rich, self-returning interface a consumer cannot declare -
// with runtime.Logger and assert.Logger, which are the one-method universal
// interfaces that are the correct thing to accept. Those are the two cases
// this test exists to tell apart.
func collectDeclaredTypes(t *testing.T, root string) map[string]declaration {
	t.Helper()

	declared := make(map[string]declaration)
	fset := token.NewFileSet()

	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if entry.IsDir() {
			if name := entry.Name(); name != "." && strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}

			return nil
		}

		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		file, parseErr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if parseErr != nil {
			return parseErr
		}

		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.TYPE {
				continue
			}

			for _, spec := range gen.Specs {
				typeSpec, ok := spec.(*ast.TypeSpec)
				if !ok || typeSpec.Assign.IsValid() {
					// An ALIAS (type X = Y) is not a distinct type and so
					// never propagates identity. Skip it.
					continue
				}

				declared[file.Name.Name+"."+typeSpec.Name.Name] = declaration{
					expr:    typeSpec.Type,
					pkg:     file.Name.Name,
					aliases: importAliases(file),
				}
			}
		}

		return nil
	})
	if err != nil {
		t.Fatalf("collect declared types: %v", err)
	}

	return declared
}

func fieldTypes(list *ast.FieldList) []ast.Expr {
	if list == nil {
		return nil
	}

	out := make([]ast.Expr, 0, len(list.List))
	for _, field := range list.List {
		out = append(out, field.Type)
	}

	return out
}

func methodName(field *ast.Field) string {
	if len(field.Names) == 0 {
		return "<embedded>"
	}

	return field.Names[0].Name
}

// isLoggerish reports whether any of the parameter's names looks like a
// logger or a metrics recorder.
func isLoggerish(names []*ast.Ident, loggerish []string) bool {
	for _, name := range names {
		lowered := strings.ToLower(name.Name)
		for _, candidate := range loggerish {
			if lowered == candidate {
				return true
			}
		}
	}

	return false
}

// typeIdents collects the identifier names appearing in a type expression,
// unwrapping pointers, slices, variadics and package qualifiers.
func typeIdents(expr ast.Expr) []string {
	switch typed := expr.(type) {
	case *ast.Ident:
		return []string{typed.Name}
	case *ast.StarExpr:
		return typeIdents(typed.X)
	case *ast.Ellipsis:
		return typeIdents(typed.Elt)
	case *ast.ArrayType:
		return typeIdents(typed.Elt)
	case *ast.SelectorExpr:
		return []string{typed.Sel.Name}
	default:
		return nil
	}
}

func paramNames(names []*ast.Ident) string {
	out := make([]string, 0, len(names))
	for _, name := range names {
		out = append(out, name.Name)
	}

	return strings.Join(out, ", ")
}

// declaration is a type declared in this module, together with the context
// needed to resolve the type expressions inside it: the package that declares
// it, and the import aliases of the file it was declared in.
type declaration struct {
	expr    ast.Expr
	pkg     string
	aliases map[string]string
}

// pkgScope resolves a type expression appearing in one file to the
// package-qualified key collectDeclaredTypes uses.
//
// It exists because the same bare identifier means different things in
// different files: "Logger" inside runtime/ is the universal one-method
// interface, while "Logger" inside log/ is the rich self-returning one. It
// also maps import ALIASES back to real package names, since this module
// imports its own log package as both "log" and "obslog"/"logpkg".
type pkgScope struct {
	declared map[string]declaration
	pkg      string
	aliases  map[string]string
}

func newPkgScope(declared map[string]declaration, file *ast.File) *pkgScope {
	return &pkgScope{declared: declared, pkg: file.Name.Name, aliases: importAliases(file)}
}

// importAliases maps the name a file refers to each of THIS module's packages
// by, back to the real package name. This module imports its own log package
// as log, obslog and logpkg depending on the file, so a selector cannot be
// resolved without knowing which file it appears in.
func importAliases(file *ast.File) map[string]string {
	aliases := make(map[string]string, len(file.Imports))

	for _, imp := range file.Imports {
		path := strings.Trim(imp.Path.Value, `"`)
		if !strings.Contains(path, "lib-observability") {
			continue
		}

		real := path[strings.LastIndex(path, "/")+1:]

		name := real
		if imp.Name != nil {
			name = imp.Name.Name
		}

		aliases[name] = real
	}

	return aliases
}

// at returns the scope as seen from inside the file that declared decl.
//
// Both halves matter. The package name decides what a bare identifier means -
// "Logger" is the rich self-returning interface in log/ and the one-method
// universal one in runtime/. The alias map decides what a selector means, and
// it must come from the DECLARING file: the file accepting the parameter need
// not import this module under the same alias, or at all, so reusing its
// aliases would make a defined type look external and silently pass.
func (s *pkgScope) at(decl declaration) *pkgScope {
	return &pkgScope{declared: s.declared, pkg: decl.pkg, aliases: decl.aliases}
}

// qualify turns the identifiers of a type expression into package-qualified
// keys: a bare ident resolves against the current package, a selector against
// its import alias.
func (s *pkgScope) qualify(expr ast.Expr) []string {
	switch typed := expr.(type) {
	case *ast.Ident:
		return []string{s.pkg + "." + typed.Name}
	case *ast.SelectorExpr:
		pkg, ok := typed.X.(*ast.Ident)
		if !ok {
			return nil
		}

		real, aliased := s.aliases[pkg.Name]
		if !aliased {
			// Not one of ours - another module, whose identity we do not
			// propagate.
			return nil
		}

		return []string{real + "." + typed.Sel.Name}
	case *ast.StarExpr:
		return s.qualify(typed.X)
	case *ast.Ellipsis:
		return s.qualify(typed.Elt)
	case *ast.ArrayType:
		return s.qualify(typed.Elt)
	default:
		return nil
	}
}

// namesLocalInterface reports whether expr resolves to an interface declared by
// this module. Such a parameter is coupling-critical regardless of its name:
// satisfying it is the consumer's job, and it cannot do that without naming the
// type, which means importing this module and inheriting its major version.
func namesLocalInterface(expr ast.Expr, scope *pkgScope) bool {
	for _, name := range scope.qualify(expr) {
		decl, isLocal := scope.declared[name]
		if !isLocal {
			continue
		}

		iface, isInterface := decl.expr.(*ast.InterfaceType)
		if !isInterface {
			continue
		}

		// A SEALED interface - one with an unexported method - is an opaque
		// token, not a dependency the consumer supplies: the unexported method
		// makes it unimplementable outside this module by design. That is the
		// functional-option pattern (tracing.TelemetryOption), where the
		// consumer calls tracing.WithX to obtain a value and never implements
		// anything. Leave those to the name heuristic; a logger-shaped
		// parameter is still checked by it.
		if sealed(iface, scope.at(decl), 0) {
			continue
		}

		return true
	}

	return false
}

// sealed reports whether an interface carries an unexported method, which makes
// it unimplementable outside the declaring package.
//
// Embedding is followed: an interface that embeds a sealed one inherits the
// unexported method and is just as unimplementable, so classifying it as
// unsealed would make namesLocalInterface force a check that
// universalityViolation then fails on the private method's own types - a false
// positive on a legitimate option type. Each embedded interface is resolved in
// the scope of the file that declared it, since aliases differ per file.
func sealed(iface *ast.InterfaceType, scope *pkgScope, depth int) bool {
	if depth > maxTypeDepth {
		return false
	}

	for _, method := range iface.Methods.List {
		for _, name := range method.Names {
			if !name.IsExported() {
				return true
			}
		}

		if _, isFunc := method.Type.(*ast.FuncType); isFunc {
			continue
		}

		// An embedded interface. Follow it.
		for _, name := range scope.qualify(method.Type) {
			decl, isLocal := scope.declared[name]
			if !isLocal {
				continue
			}

			embedded, isInterface := decl.expr.(*ast.InterfaceType)
			if !isInterface {
				continue
			}

			if sealed(embedded, scope.at(decl), depth+1) {
				return true
			}
		}
	}

	return false
}
