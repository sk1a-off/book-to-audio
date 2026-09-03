// Command quality-corpus validates a version-controlled TTS quality corpus.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"book-text-editor/internal/quality"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		_, _ = fmt.Fprintf(os.Stderr, "quality corpus validation failed: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("quality-corpus", flag.ContinueOnError)
	flags.SetOutput(stderr)
	corpusPath := flags.String(
		"corpus",
		"../testdata/tts_quality/corpus.json",
		"path to the versioned TTS quality corpus",
	)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments: %v", flags.Args())
	}

	corpus, err := quality.LoadCorpus(*corpusPath)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(
		stdout,
		"quality corpus valid: version=%s language=%s cases=%d\n",
		corpus.CorpusVersion,
		corpus.Language,
		len(corpus.Cases),
	)
	return err
}
