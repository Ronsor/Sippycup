package main

import (
	"fmt"
	"os"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/gohcl"
	"github.com/hashicorp/hcl/v2/hclparse"
	"github.com/ronsor/sippycup/libsippycup"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/function"
)

// LoadConfig reads, parses, and validates HCL without opening sockets.
func LoadConfig(path string) (*libsippycup.Config, error) {
	b, e := os.ReadFile(path)
	if e != nil {
		return nil, e
	}

	return ParseConfig(b, path)
}

// ParseConfig decodes and validates HCL, using path in diagnostics.
// It resolves env() values immediately and returns no partially valid configuration.
func ParseConfig(b []byte, path string) (*libsippycup.Config, error) {
	p := hclparse.NewParser()
	f, d := p.ParseHCL(b, path)
	if d.HasErrors() {
		return nil, fmt.Errorf("%s", d.Error())
	}

	schema := &hcl.BodySchema{
		Blocks: []hcl.BlockHeaderSchema{
			{Type: "credential", LabelNames: []string{"name"}},
			{Type: "credential_set", LabelNames: []string{"name"}},
			{Type: "source", LabelNames: []string{"name"}},
			{Type: "endpoint", LabelNames: []string{"name"}},
			{Type: "route", LabelNames: []string{"name"}},
			{Type: "runtime"},
		},
	}
	content, d := f.Body.Content(schema)
	if d.HasErrors() {
		return nil, fmt.Errorf("%s", d.Error())
	}

	vars := map[string]cty.Value{}
	refs := map[string]map[string]cty.Value{}
	for _, block := range content.Blocks {
		if len(block.Labels) == 0 {
			continue
		}

		kind, name := block.Type, block.Labels[0]
		if refs[kind] == nil {
			refs[kind] = map[string]cty.Value{}
		}

		if _, ok := refs[kind][name]; ok {
			return nil, fmt.Errorf("duplicate %s %q", kind, name)
		}

		refs[kind][name] = cty.StringVal(name)
	}

	for _, kind := range []string{"credential", "credential_set", "source", "endpoint"} {
		if refs[kind] == nil {
			refs[kind] = map[string]cty.Value{}
		}

		vars[kind] = cty.ObjectVal(refs[kind])
	}

	env := function.New(&function.Spec{
		Params: []function.Parameter{{Name: "name", Type: cty.String}},
		Type:   function.StaticReturnType(cty.String),
		Impl: func(args []cty.Value, _ cty.Type) (cty.Value, error) {
			v, ok := os.LookupEnv(args[0].AsString())
			if !ok {
				return cty.NilVal, fmt.Errorf("environment variable %q is not set", args[0].AsString())
			}

			return cty.StringVal(v), nil
		},
	})
	ctx := &hcl.EvalContext{
		Variables: vars,
		Functions: map[string]function.Function{"env": env},
	}
	c := &libsippycup.Config{EvalContext: ctx}
	d = gohcl.DecodeBody(f.Body, ctx, c)
	if d.HasErrors() {
		return nil, fmt.Errorf("%s", d.Error())
	}

	if e := c.Validate(); e != nil {
		return nil, e
	}

	return c, nil
}
