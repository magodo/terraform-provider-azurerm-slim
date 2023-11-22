package main

import (
	"errors"
	"fmt"
	"go/ast"
	"go/format"
	"go/token"
	"go/types"
	"log"
	"os"
	"strings"

	"golang.org/x/tools/go/packages"
)

func run() error {
	dir := "."
	cfg := packages.Config{Dir: dir, Mode: packages.LoadSyntax}
	pkgs, err := packages.Load(&cfg, "./internal/...")
	if err != nil {
		return err
	}
	if packages.PrintErrors(pkgs) > 0 {
		return errors.New("packages contain errors")
	}

	var pluginsdkPkg, sdkPkg *packages.Package
	var servicePkgs []*packages.Package

	for _, pkg := range pkgs {
		p := pkg.PkgPath
		if p == "github.com/hashicorp/terraform-provider-azurerm/internal/tf/pluginsdk" {
			pluginsdkPkg = pkg
			continue
		}
		if p == "github.com/hashicorp/terraform-provider-azurerm/internal/sdk" {
			sdkPkg = pkg
			continue
		}
		if strings.HasPrefix(p, "github.com/hashicorp/terraform-provider-azurerm/internal/services/") {
			servicePkgs = append(servicePkgs, pkg)
			continue
		}
	}

	if err := forUntyped(pluginsdkPkg, servicePkgs); err != nil {
		return err
	}

	if err := forTyped(sdkPkg, servicePkgs); err != nil {
		return err
	}

	return nil
}

func forUntyped(pluginsdkPkg *packages.Package, servicePkgs []*packages.Package) error {
	var crudFuncType types.Type

	for ident, obj := range pluginsdkPkg.TypesInfo.Defs {
		if ident.Name == "CreateFunc" {
			crudFuncType = obj.(*types.TypeName).Type().(*types.Named).Underlying().(*types.Signature)
		}
	}

	// Find all uses (i.e. ident -> types.Object) of the CUD functions in the schema declaration
	cudObjs := map[types.Object]bool{}
	for _, pkg := range servicePkgs {
		for _, file := range pkg.Syntax {
			for _, decl := range file.Decls {
				fdecl, ok := decl.(*ast.FuncDecl)
				if !ok {
					continue
				}
				if fdecl.Type == nil || fdecl.Type.Results == nil {
					continue
				}
				if len(fdecl.Type.Results.List) != 1 {
					continue
				}
				ret := fdecl.Type.Results.List[0]
				sexpr, ok := ret.Type.(*ast.StarExpr)
				if !ok {
					continue
				}
				sel, ok := sexpr.X.(*ast.SelectorExpr)
				if !ok {
					continue
				}
				x, ok := sel.X.(*ast.Ident)
				if !ok {
					continue
				}
				if x.Name != "pluginsdk" {
					continue
				}
				if sel.Sel.Name != "Resource" {
					continue
				}
				ast.Inspect(fdecl.Body, func(n ast.Node) bool {
					kvexpr, ok := n.(*ast.KeyValueExpr)
					if !ok {
						return true
					}
					keyIdent, ok := kvexpr.Key.(*ast.Ident)
					if !ok {
						return true
					}
					if !(keyIdent.Name == "Create" || keyIdent.Name == "Update" || keyIdent.Name == "Delete") {
						return true
					}
					if !types.Identical(pkg.TypesInfo.TypeOf(kvexpr.Value), crudFuncType) {
						return true
					}
					cudObjs[pkg.TypesInfo.Uses[kvexpr.Value.(*ast.Ident)]] = true
					return false
				})
			}
		}
	}

	// Rewrite these CUD function's definitions
	for _, pkg := range servicePkgs {
		for _, file := range pkg.Syntax {
			var modified bool
			for _, decl := range file.Decls {
				fdecl, ok := decl.(*ast.FuncDecl)
				if !ok {
					continue
				}
				if _, ok := cudObjs[pkg.TypesInfo.Defs[fdecl.Name]]; !ok {
					continue
				}
				modified = true
				fdecl.Body.List = []ast.Stmt{
					&ast.ReturnStmt{Results: []ast.Expr{&ast.Ident{Name: "nil"}}},
				}
			}
			if modified {
				if err := write(file, pkg.Fset); err != nil {
					return err
				}
			}
		}
	}

	return nil
}

func forTyped(sdkPkg *packages.Package, servicePkgs []*packages.Package) error {
	var crudFuncType types.Type
	for ident, obj := range sdkPkg.TypesInfo.Defs {
		if ident.Name == "ResourceRunFunc" {
			crudFuncType = obj.(*types.TypeName).Type().(*types.Named).Underlying().(*types.Signature)
		}
	}

	for _, pkg := range servicePkgs {
		for _, file := range pkg.Syntax {
			var modified bool
			for _, decl := range file.Decls {
				fdecl, ok := decl.(*ast.FuncDecl)
				if !ok {
					continue
				}
				if fdecl.Name == nil {
					continue
				}
				if name := fdecl.Name.Name; name != "Create" && name != "Update" && name != "Delete" {
					continue
				}
				if fdecl.Type == nil || fdecl.Type.Results == nil {
					continue
				}
				if len(fdecl.Type.Results.List) != 1 {
					continue
				}
				ret := fdecl.Type.Results.List[0]
				sel, ok := ret.Type.(*ast.SelectorExpr)
				if !ok {
					continue
				}
				x, ok := sel.X.(*ast.Ident)
				if !ok {
					continue
				}
				if x.Name != "sdk" {
					continue
				}
				if sel.Sel.Name != "ResourceFunc" {
					continue
				}
				ast.Inspect(fdecl.Body, func(n ast.Node) bool {
					kvexpr, ok := n.(*ast.KeyValueExpr)
					if !ok {
						return true
					}
					keyIdent, ok := kvexpr.Key.(*ast.Ident)
					if !ok {
						return true
					}
					if keyIdent.Name != "Func" {
						return true
					}
					if !types.Identical(pkg.TypesInfo.TypeOf(kvexpr.Value), crudFuncType) {
						return true
					}
					modified = true
					kvexpr.Value = &ast.Ident{Name: "nil"}
					return false
				})
			}
			if modified {
				if err := write(file, pkg.Fset); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func write(file *ast.File, fset *token.FileSet) error {
	pos := fset.Position(file.Pos())
	f, err := os.OpenFile(pos.Filename, os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return fmt.Errorf("openning %s for rewriting", pos.Filename)
	}
	defer f.Close()
	if err := format.Node(f, fset, file); err != nil {
		return fmt.Errorf("rewriting %s", pos.Filename)
	}
	return nil
}

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}
