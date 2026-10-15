package main

import (
	"errors"
	"flag"
)

// cliFlags holds parsed options and the subprocess command following the flags.
type cliFlags struct {
	config     string
	protocol   string
	bind       string
	path       string
	extended   bool
	endpoint   string
	to         string
	sampleRate int
	debug      bool
	command    []string
}

// parseCLIFlags defines and parses flags shared by serve, check, listen, and dial.
// A nil result with no error means flag help was printed and execution should stop.
func parseCLIFlags(mode string, args []string) (*cliFlags, error) {
	var options cliFlags
	fs := flag.NewFlagSet(mode, flag.ContinueOnError)
	fs.StringVar(&options.config, "config", "", "HCL configuration file")
	fs.StringVar(&options.protocol, "protocol", "audiosocket", "protocol for CLI mode")
	fs.StringVar(&options.bind, "bind", "127.0.0.1:9019", "source bind address")
	fs.StringVar(&options.path, "path", "/audio", "HTTP source path")
	fs.BoolVar(&options.extended, "extended", false, "enable extended AudioSocket identities")
	fs.StringVar(&options.endpoint, "endpoint", "", "configured outbound endpoint")
	fs.StringVar(&options.to, "to", "", "destination user or endpoint address")
	fs.IntVar(&options.sampleRate, "sample-rate", 8000, "subprocess PCM sample rate")
	fs.BoolVar(&options.debug, "debug", false, "enable debug logging")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil, nil
		}

		return nil, err
	}

	options.command = fs.Args()

	return &options, nil
}
