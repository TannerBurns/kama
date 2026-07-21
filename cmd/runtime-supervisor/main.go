/*
Copyright 2026 Kama Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Command runtime-supervisor owns one llama-server process and exposes
// Pod-internal lifecycle diagnostics.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	kamaruntime "github.com/TannerBurns/kama/internal/runtime"
	"github.com/TannerBurns/kama/internal/version"
)

const defaultConfigFile = "/etc/kama/runtime/config.json"

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}

func run(parent context.Context, arguments []string, stdout, stderr io.Writer) int {
	if len(arguments) > 0 && arguments[0] == "drain" {
		return runDrain(parent, arguments[1:], stderr)
	}

	flags := flag.NewFlagSet("kama-runtime-supervisor", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var configFile string
	var showVersion bool
	flags.StringVar(&configFile, "config", defaultConfigFile, "Path to the controller-owned runtime JSON config")
	flags.BoolVar(&showVersion, "version", false, "Print the Kama runtime supervisor version and exit")
	if err := flags.Parse(arguments); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		_, _ = fmt.Fprintln(stderr, "kama-runtime-supervisor does not accept positional arguments")
		return 2
	}
	if showVersion {
		_, _ = fmt.Fprintln(stdout, version.Version)
		return 0
	}
	if configFile == "" {
		_, _ = fmt.Fprintln(stderr, "--config must not be empty")
		return 2
	}

	ctx, stop := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	defer stop()
	logger := slog.New(slog.NewJSONHandler(stdout, nil))
	options := kamaruntime.Options{Logger: logger}
	config, err := readConfig(configFile)
	var supervisor *kamaruntime.Supervisor
	if err != nil {
		logger.Error("Runtime configuration is invalid", "error", err.Error())
		config.SchemaVersion = kamaruntime.SchemaVersion
		config.Default()
		supervisor = kamaruntime.NewFailedSupervisor(config, err, options)
	} else {
		supervisor = kamaruntime.NewSupervisor(config, options)
	}
	if err := supervisor.Run(ctx); err != nil {
		logger.Error("Runtime supervisor stopped", "error", err.Error())
		return 1
	}
	return 0
}

func runDrain(parent context.Context, arguments []string, stderr io.Writer) int {
	flags := flag.NewFlagSet("kama-runtime-supervisor drain", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var address string
	flags.StringVar(&address, "address", "127.0.0.1:8081", "Loopback supervisor address")
	if err := flags.Parse(arguments); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		_, _ = fmt.Fprintln(stderr, "kama-runtime-supervisor drain does not accept positional arguments")
		return 2
	}
	ctx, stop := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := kamaruntime.RequestDrain(ctx, address); err != nil {
		_, _ = fmt.Fprintf(stderr, "drain runtime: %v\n", err)
		return 1
	}
	return 0
}

func readConfig(filename string) (kamaruntime.Config, error) {
	file, err := os.Open(filename)
	if err != nil {
		return kamaruntime.Config{}, fmt.Errorf("open runtime config: %w", err)
	}
	defer func() { _ = file.Close() }()
	return kamaruntime.DecodeConfig(file)
}
