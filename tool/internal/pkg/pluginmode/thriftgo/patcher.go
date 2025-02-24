// Copyright 2021 CloudWeGo Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//   http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package thriftgo

import (
	"fmt"
	"io/ioutil"
	"path/filepath"
	"sort"
	"strings"
	"text/template"

	"github.com/cloudwego/thriftgo/generator/golang"
	"github.com/cloudwego/thriftgo/parser"
	"github.com/cloudwego/thriftgo/plugin"

	"github.com/cloudwego/kitex"
)

const kitexUnusedProtection = `
// KitexUnusedProtection is used to prevent 'imported and not used' error.
var KitexUnusedProtection = struct{}{}
`

//lint:ignore U1000 until protectionInsertionPoint is used
var protectionInsertionPoint = "KitexUnusedProtection"

type patcher struct {
	noFastAPI bool
	utils     *golang.CodeUtils
	module    string
	copyIDL   bool

	fileTpl *template.Template
}

func (p *patcher) buildTemplates() error {
	m := p.utils.BuildFuncMap()
	m["ReorderStructFields"] = p.reorderStructFields
	m["TypeIDToGoType"] = func(t string) string { return typeIDToGoType[t] }
	m["FilterBase"] = p.filterBase
	m["IsBinaryOrStringType"] = p.isBinaryOrStringType
	m["Version"] = func() string { return kitex.Version }
	m["GenerateFastAPIs"] = func() bool { return !p.noFastAPI }
	m["ToPackageNames"] = func(imports map[string]string) (res []string) {
		for pth, alias := range imports {
			if alias != "" {
				res = append(res, alias)
			} else {
				res = append(res, strings.ToLower(filepath.Base(pth)))
			}
		}
		sort.Strings(res)
		return
	}

	tpl := template.New("kitex").Funcs(m)
	for _, txt := range allTemplates {
		tpl = template.Must(tpl.Parse(txt))
	}
	p.fileTpl = tpl
	return nil
}

func (p *patcher) patch(req *plugin.Request) (patches []*plugin.Generated, err error) {
	p.buildTemplates()
	var buf strings.Builder

	protection := make(map[string]*plugin.Generated)

	for ast := range req.AST.DepthFirstSearch() {
		scope, err := p.utils.BuildScope(ast)
		if err != nil {
			return nil, fmt.Errorf("build scope for ast %q: %w", ast.Filename, err)
		}
		p.utils.SetRootScope(scope)

		namespace := ast.GetNamespaceOrReferenceName("go")
		pkgName := p.utils.NamespaceToPackage(namespace)

		path, err := p.utils.GetFilePath(ast)
		if err != nil {
			return nil, err
		}
		full := filepath.Join(req.OutputPath, path)
		dir, base := filepath.Split(full)
		target := filepath.Join(dir, "k-"+base)

		// Define KitexUnusedProtection in k-consts.go .
		// Add k-consts.go before target to force the k-consts.go generated by consts.thrift to be renamed.
		consts := filepath.Join(filepath.Dir(full), "k-consts.go")
		if protection[consts] == nil {
			patch := &plugin.Generated{
				Content: "package " + pkgName + "\n" + kitexUnusedProtection,
				Name:    &consts,
			}
			patches = append(patches, patch)
			protection[consts] = patch
		}

		buf.Reset()
		data := &golang.Data{AST: ast, PkgName: pkgName}
		data.Imports, err = p.utils.ResolveImports()
		if err != nil {
			return nil, fmt.Errorf("resolve imports failed for %q: %w", ast.Filename, err)
		}
		p.filterStdLib(data.Imports)
		if err = p.fileTpl.ExecuteTemplate(&buf, "file", data); err != nil {
			return nil, fmt.Errorf("%q: %w", ast.Filename, err)
		}
		patches = append(patches, &plugin.Generated{
			Content: buf.String(),
			Name:    &target,
		})

		if p.copyIDL {
			content, err := ioutil.ReadFile(ast.Filename)
			if err != nil {
				return nil, fmt.Errorf("read %q: %w", ast.Filename, err)
			}
			path := filepath.Join(filepath.Dir(full), filepath.Base(ast.Filename))
			patches = append(patches, &plugin.Generated{
				Content: string(content),
				Name:    &path,
			})
		}
	}
	return
}

func (p *patcher) filterBase(ast *parser.Thrift) interface{} {
	var req, res []*parser.StructLike
	for _, s := range ast.GetStructLike() {
		for _, f := range s.Fields {
			fn, _ := p.utils.Unexport(f.Name)
			tn := f.Type.Name
			if fn == "base" && tn == "base.Base" {
				req = append(req, s)
			}
			if fn == "baseResp" && tn == "base.BaseResp" {
				res = append(res, s)
			}
		}
	}
	return &struct {
		Requests  []*parser.StructLike
		Responses []*parser.StructLike
	}{Requests: req, Responses: res}
}

func (p *patcher) reorderStructFields(fields []*parser.Field) ([]*parser.Field, error) {
	fixedLengthFields := make(map[*parser.Field]bool, len(fields))
	for _, field := range fields {
		ok, err := p.utils.IsFixedLengthType(field.Type)
		if err != nil {
			return nil, err
		}
		fixedLengthFields[field] = ok
	}

	sortedFields := make([]*parser.Field, 0, len(fields))
	for _, v := range fields {
		if fixedLengthFields[v] {
			sortedFields = append(sortedFields, v)
		}
	}
	for _, v := range fields {
		if !fixedLengthFields[v] {
			sortedFields = append(sortedFields, v)
		}
	}

	return sortedFields, nil
}

func (p *patcher) filterStdLib(imports map[string]string) {
	// remove std libs and thrift to prevent duplicate import.
	prefix := p.module + "/"
	for pth := range imports {
		if strings.HasPrefix(pth, prefix) { // local module
			continue
		}
		if pth == "github.com/apache/thrift/lib/go/thrift" {
			delete(imports, pth)
		}
		if strings.HasPrefix(pth, "github.com/cloudwego/thriftgo") {
			delete(imports, pth)
		}
		if !strings.Contains(pth, ".") { // std lib
			delete(imports, pth)
		}
	}
}

func (p *patcher) isBinaryOrStringType(t *parser.Type) (ok bool, err error) {
	ok, err = p.utils.IsBinaryType(t)
	if err != nil || ok {
		return
	}
	ok, err = p.utils.IsStringType(t)
	return
}

var typeIDToGoType = map[string]string{
	"Bool":   "bool",
	"Byte":   "int8",
	"I16":    "int16",
	"I32":    "int32",
	"I64":    "int64",
	"Double": "float64",
	"String": "string",
	"Binary": "[]byte",
}
