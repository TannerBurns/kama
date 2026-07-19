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

// Command importer validates or publishes one immutable Kama artifact.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/TannerBurns/kama/internal/artifact"
	"github.com/TannerBurns/kama/internal/version"
)

const (
	defaultSpecFile   = "/etc/kama/import/spec.json"
	defaultResultFile = "/dev/termination-log"
)

func main() {
	exitCode := run(context.Background(), os.Args[1:], os.Stdout, os.Stderr)
	os.Exit(exitCode)
}

func run(parent context.Context, arguments []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("kama-importer", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var specFile, resultFile string
	var showVersion bool
	flags.StringVar(&specFile, "spec-file", defaultSpecFile, "Path to the mounted importer JSON spec")
	flags.StringVar(&resultFile, "result-file", defaultResultFile, "Path for the compact termination summary")
	flags.BoolVar(&showVersion, "version", false, "Print the Kama importer version and exit")
	if err := flags.Parse(arguments); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		_, _ = fmt.Fprintln(stderr, "kama-importer does not accept positional arguments")
		return 2
	}
	if showVersion {
		_, _ = fmt.Fprintln(stdout, version.Version)
		return 0
	}
	if specFile == "" || resultFile == "" {
		_, _ = fmt.Fprintln(stderr, "--spec-file and --result-file must not be empty")
		return 2
	}

	ctx, stop := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	defer stop()
	spec, err := readSpec(specFile)
	var result artifact.Result
	if err != nil {
		result = artifact.NewFailureResult("", err)
	} else {
		result = artifact.Execute(ctx, spec)
	}
	if err := os.WriteFile(resultFile, artifact.MarshalSummary(result), 0o600); err != nil {
		result = artifact.NewFailureResult(result.Mode, fmt.Errorf("write termination summary: %w", err))
	}
	line, err := artifact.MarshalResultLine(result)
	if err != nil {
		fallback := artifact.NewFailureResult(result.Mode, err)
		line, _ = artifact.MarshalResultLine(fallback)
		result = fallback
	}
	if _, err := stdout.Write(line); err != nil {
		_, _ = fmt.Fprintln(stderr, "write importer result: output unavailable")
		return 1
	}
	if !result.Success {
		return 1
	}
	return 0
}

func readSpec(filename string) (artifact.Spec, error) {
	file, err := os.Open(filename)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return artifact.Spec{}, &artifact.Error{
				Reason: artifact.ReasonInvalidSpec,
				Op:     "open importer spec",
				Err:    errors.New("spec file does not exist"),
			}
		}
		return artifact.Spec{}, &artifact.Error{Reason: artifact.ReasonInvalidSpec, Op: "open importer spec", Err: err}
	}
	defer func() { _ = file.Close() }()
	return artifact.DecodeSpec(file)
}
