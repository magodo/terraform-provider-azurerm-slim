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

	var crudFuncTypeU types.Type
	for ident, obj := range pluginsdkPkg.TypesInfo.Defs {
		if ident.Name == "CreateFunc" {
			crudFuncTypeU = obj.(*types.TypeName).Type().(*types.Named).Underlying().(*types.Signature)
		}
	}

	var crudFuncTypeT types.Type
	for ident, obj := range sdkPkg.TypesInfo.Defs {
		if ident.Name == "ResourceRunFunc" {
			crudFuncTypeT = obj.(*types.TypeName).Type().(*types.Named).Underlying().(*types.Signature)
		}
	}

	for _, pkg := range servicePkgs {
		for _, file := range pkg.Syntax {
			if err := forUntyped(pkg, file, crudFuncTypeU); err != nil {
				return err
			}
			if err := forTyped(pkg, file, crudFuncTypeT); err != nil {
				return err
			}
		}
	}

	return nil
}

func forUntyped(pkg *packages.Package, file *ast.File, funcType types.Type) error {
	var modified bool
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
			if !types.Identical(pkg.TypesInfo.TypeOf(kvexpr.Value), funcType) {
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
	return nil
}

func forTyped(pkg *packages.Package, file *ast.File, funcType types.Type) error {
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
			if !types.Identical(pkg.TypesInfo.TypeOf(kvexpr.Value), funcType) {
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
