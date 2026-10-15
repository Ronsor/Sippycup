package main

import (
	"embed"
	"fmt"
	"strings"
)

//go:embed examples/config/*.hcl
var exampleConfigs embed.FS

// exampleConfig returns an embedded configuration by its filename without .hcl.
func exampleConfig(name string) ([]byte, error) {
	if name == "" || strings.ContainsAny(name, "/\\") {
		return nil, fmt.Errorf("unknown example %q", name)
	}

	data, err := exampleConfigs.ReadFile("examples/config/" + name + ".hcl")
	if err != nil {
		return nil, fmt.Errorf("unknown example %q", name)
	}

	return data, nil
}

const usageText = `sippycup - HCL-configured audio gateway

  sippycup serve --config calls.hcl
  sippycup check --config calls.hcl
  sippycup example
  sippycup example gateway
  sippycup example sip-to-audiosocket
  sippycup example audiosocket-to-sip
  sippycup example audiosocket-metadata
  sippycup example sip-early-media
  sippycup example webrtc-early-media
  sippycup listen --protocol audiosocket --bind :9019 -- ./handler
  sippycup dial --config calls.hcl --endpoint provider --to 100 -- ./handler

The handler receives mono signed 16-bit little-endian PCM on stdin and writes
PCM to stdout. stderr is reserved for logs. Use --sample-rate to select its rate.
Run any command with --help for flags. No shell is implicitly executed.
Open a webrtc_http or audiosocket_ws source URL in a browser for a call keypad.
Microphone access requires HTTPS or localhost.
On Unix, SIGHUP reloads serve --config; existing calls continue during reload.`
