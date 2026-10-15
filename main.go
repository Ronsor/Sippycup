package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/ronsor/sippycup/libsippycup"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		slog.Error("sippycup", "error", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" {
		fmt.Fprintln(os.Stdout, usageText)

		return nil
	}

	if args[0] == "example" {
		name := "echo"
		if len(args) > 1 {
			name = args[1]
		}

		config, err := exampleConfig(name)
		if err != nil {
			return err
		}

		_, err = os.Stdout.Write(config)

		return err
	}

	mode := args[0]
	if mode != "serve" && mode != "check" && mode != "listen" && mode != "dial" {
		return fmt.Errorf("unknown command %q", mode)
	}

	options, err := parseCLIFlags(mode, args[1:])
	if err != nil || options == nil {
		return err
	}

	if options.debug {
		slog.SetLogLoggerLevel(slog.LevelDebug)
	}

	var c *libsippycup.Config
	switch mode {
	case "serve", "check":
		if options.config == "" {
			return errors.New("--config is required")
		}
		c, err = LoadConfig(options.config)
	case "listen":
		if len(options.command) == 0 {
			return errors.New("listen requires a command after --")
		}
		command := []string{}
		for _, arg := range options.command {
			command = append(command, strconv.Quote(arg))
		}
		text := fmt.Sprintf(
			"source \"cli\" {\n type = %q\n bind = %q\n extended = %t\n",
			options.protocol,
			options.bind,
			options.extended,
		)
		if options.protocol == "audiosocket_ws" || options.protocol == "webrtc_http" {
			text += fmt.Sprintf(" path = %q\n", options.path)
		}
		text += fmt.Sprintf(`}
endpoint "handler" {
 type = "exec"
 command = [%s]
 io = "pcm"
 transcode {
 codec = "pcm_s16le"
 sample_rate = %d
 }
}
route "cli" {
 connect { endpoint = endpoint.handler }
}
`, strings.Join(command, ","), options.sampleRate)
		c, err = ParseConfig([]byte(text), "<cli>")
	case "dial":
		if options.config != "" {
			c, err = LoadConfig(options.config)
		} else {
			if options.to == "" {
				return errors.New("dial requires --to or --config with --endpoint")
			}

			attr := "address"
			if options.protocol == "sip" {
				attr = "uri"
			} else if options.protocol == "audiosocket_ws" || options.protocol == "webrtc_http" {
				attr = "url"
			}

			c, err = ParseConfig(
				[]byte(fmt.Sprintf(
					"endpoint \"cli\" {\n type = %q\n %s = %q\n extended = %t\n}\n",
					options.protocol,
					attr,
					options.to,
					options.extended,
				)),
				"<cli>",
			)
			options.endpoint = "cli"
			options.to = ""
		}
	}

	if err != nil {
		return err
	}

	if mode == "check" {
		fmt.Fprintln(os.Stdout, "configuration valid")

		return nil
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if mode == "dial" {
		return libsippycup.DialCommand(ctx, c, options.endpoint, options.to, options.sampleRate, options.command)
	}

	if mode == "serve" {
		reload, stop := reloadSignals()
		defer stop()

		return serveConfig(ctx, options.config, c, reload)
	}

	return libsippycup.NewEngine(c).Serve(ctx)
}
