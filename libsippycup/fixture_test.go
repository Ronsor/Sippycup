package libsippycup

import (
	"fmt"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/gohcl"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
)

// fixtureConfig decodes the HCL shorthand used in engine tests. It deliberately
// has no frontend features (file loading, env, or duplicate-name diagnostics).
// The production parser and its tests live in the root CLI package.
func fixtureConfig(text []byte, name string) (*Config, error) {
	file, diagnostics := hclsyntax.ParseConfig(text, name, hcl.InitialPos)
	if diagnostics.HasErrors() {
		return nil, fmt.Errorf("%s", diagnostics.Error())
	}

	refs := map[string]map[string]cty.Value{}
	for _, block := range file.Body.(*hclsyntax.Body).Blocks {
		if len(block.Labels) == 1 {
			if refs[block.Type] == nil {
				refs[block.Type] = map[string]cty.Value{}
			}

			refs[block.Type][block.Labels[0]] = cty.StringVal(block.Labels[0])
		}
	}

	ctx := &hcl.EvalContext{Variables: map[string]cty.Value{}}
	for kind, values := range refs {
		ctx.Variables[kind] = cty.ObjectVal(values)
	}

	config := &Config{EvalContext: ctx}
	if diagnostics := gohcl.DecodeBody(file.Body, ctx, config); diagnostics.HasErrors() {
		return nil, fmt.Errorf("%s", diagnostics.Error())
	}

	if err := config.Validate(); err != nil {
		return nil, err
	}

	return config, nil
}
