package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type reloadLog struct {
	messages chan string
}

func (l reloadLog) Write(p []byte) (int, error) {
	select {
	case l.messages <- string(p):
	default:
	}

	return len(p), nil
}

func TestConfigReload(t *testing.T) {
	logs := reloadLog{messages: make(chan string, 64)}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	waitLog := func(message string) {
		t.Helper()
		timer := time.NewTimer(5 * time.Second)
		defer timer.Stop()
		for {
			select {
			case line := <-logs.messages:
				if strings.Contains(line, message) {
					return
				}
			case <-timer.C:
				t.Fatalf("timed out waiting for %q", message)
			}
		}
	}

	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	address := probe.Addr().String()
	probe.Close()
	path := filepath.Join(t.TempDir(), "calls.hcl")
	write := func(text string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	configuration := func(bind, route string) string {
		return fmt.Sprintf("source \"web\" {\n type=\"webrtc_http\"\n bind=%q\n path=%q\n}\n", bind, route)
	}
	write(configuration(address, "/before"))
	config, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	requests := make(chan os.Signal, 1)
	finished := make(chan error, 1)
	go func() { finished <- serveConfig(ctx, path, config, requests) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-finished:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("gateway did not stop")
		}
	})
	waitLog("HTTP server listening")

	client := &http.Client{Timeout: 2 * time.Second}
	defer client.CloseIdleConnections()
	checkPage := func(route string, want int) {
		t.Helper()
		r, err := http.NewRequest("GET", "http://"+address+route, nil)
		if err != nil {
			t.Fatal(err)
		}

		r.Header.Set("Accept", "text/html")
		response, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}

		response.Body.Close()
		if response.StatusCode != want {
			t.Fatalf("%s: status %d, want %d", route, response.StatusCode, want)
		}
	}
	checkPage("/before", 200)

	// Validation fails before the current engine is stopped.
	write(`source "broken" {`)
	requests <- os.Interrupt
	waitLog("configuration reload rejected")
	checkPage("/before", 200)

	// The same address can be rebound with a different HTTP path.
	write(configuration(address, "/after"))
	requests <- os.Interrupt
	waitLog("configuration reloaded")
	checkPage("/after", 200)
	checkPage("/before", 404)

	// Listener changes are rejected before touching any running transport.
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	defer occupied.Close()
	write(configuration(occupied.Addr().String(), "/unavailable"))
	requests <- os.Interrupt
	waitLog("configuration reload rejected")
	checkPage("/after", 200)

	// A rejected listener change must leave subsequent reloads operational.
	write(configuration(address, "/final"))
	requests <- os.Interrupt
	waitLog("configuration reloaded")
	checkPage("/final", 200)
	close(requests)
}
