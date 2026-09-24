package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/wongpinter/gdm/internal/mediaorg"
)

func runOrganize(args []string, out io.Writer) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		printOrganizeUsage(out)
		return nil
	}
	kind := strings.ToLower(args[0])
	if kind != mediaorg.TV && kind != mediaorg.Movie {
		return fmt.Errorf("media type must be tv or movie")
	}
	if len(args) > 1 && (args[1] == "-h" || args[1] == "--help") {
		printOrganizeUsage(out)
		return nil
	}
	fs := flag.NewFlagSet("organize "+kind, flag.ContinueOnError)
	fs.SetOutput(out)
	input := fs.String("input", "", "file or directory to scan recursively")
	output := fs.String("output", "", "organized media root")
	query := fs.String("query", "", "override show or movie title")
	apply := fs.Bool("apply", false, "copy matched files (default: preview only)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	if *input == "" || *output == "" {
		return errors.New("--input and --output are required")
	}
	return mediaorg.Run(context.Background(), mediaorg.Options{
		Kind: kind, Input: *input, Output: *output, Query: *query, Apply: *apply,
	}, out)
}

func printOrganizeUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage: gdm organize <tv|movie> --input PATH --output PATH [--query TITLE] [--apply]")
	fmt.Fprintln(w, "Preview is default. --apply copies matched media and refuses to overwrite existing files.")
	fmt.Fprintln(w, "Movies need TMDB_API_KEY or TMDB_READ_ACCESS_TOKEN.")
	fmt.Fprintln(w, "TV example: gdm organize tv --input ./downloads --output ./library --query 'The Simpsons'")
	fmt.Fprintln(w, "Movie example: TMDB_API_KEY=... gdm organize movie --input ./downloads --output ./Movies")
}
