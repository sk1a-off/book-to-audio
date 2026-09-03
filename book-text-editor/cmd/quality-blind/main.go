// Command quality-blind builds a reveal-safe segmentation A/B listening page.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"book-text-editor/internal/qualityblind"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		_, _ = fmt.Fprintf(os.Stderr, "quality blind package failed: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("quality-blind", flag.ContinueOnError)
	flags.SetOutput(stderr)
	sessionID := flags.String("session", "", "unique blind session ID (required)")
	experimentA := flags.String("experiment-a", "", "first quality-runner experiment directory (required)")
	experimentB := flags.String("experiment-b", "", "second quality-runner experiment directory (required)")
	outputRoot := flags.String("output", "", "blind package output root (required)")
	shuffleSeed := flags.Uint64("shuffle-seed", 42, "deterministic hidden-label shuffle seed")
	primaryVariable := flags.String(
		"primary-variable",
		qualityblind.SegmenterVariable,
		"controlled variable: segmenter_version, normalizer_version, inference_config.fade_duration, or engine",
	)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments: %v", flags.Args())
	}
	if strings.TrimSpace(*sessionID) == "" {
		return errors.New("-session is required")
	}
	if strings.TrimSpace(*experimentA) == "" || strings.TrimSpace(*experimentB) == "" {
		return errors.New("-experiment-a and -experiment-b are required")
	}
	if strings.TrimSpace(*outputRoot) == "" {
		return errors.New("-output is required")
	}
	report, err := qualityblind.Build(qualityblind.Config{
		SessionID:       *sessionID,
		ExperimentA:     *experimentA,
		ExperimentB:     *experimentB,
		OutputRoot:      *outputRoot,
		ShuffleSeed:     *shuffleSeed,
		PrimaryVariable: strings.TrimSpace(*primaryVariable),
	})
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(
		stdout,
		"blind package complete: session=%s cases=%d output=%s\n",
		report.SessionID,
		report.CaseCount,
		report.OutputDir,
	)
	return err
}
