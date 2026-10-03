// Command image-builder builds container images locally without a daemon.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/cuixudong74/container-image-builder-go/internal/builder"
)

const version = "0.1.0"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "build":
		if err := runBuild(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "error: %s\n", err.Error())
			os.Exit(1)
		}
	case "version", "--version", "-v":
		fmt.Println("image-builder", version)
	case "help", "--help", "-h":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `Usage: image-builder <command> [options]

Commands:
  build    Build a container image from a description file and context
  version  Print version information

Run "image-builder build -h" for build options.
`)
}

func runBuild(args []string) error {
	fs := flag.NewFlagSet("build", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	file := fs.String("f", "Dockerfile", "build description file")
	contextDir := fs.String("c", ".", "build context directory")
	output := fs.String("o", "image.tar", "output OCI archive path")
	cacheDir := fs.String("cache", ".build-cache", "layer cache directory")
	platforms := fs.String("platform", "", "comma-separated target platforms (e.g. linux/amd64,linux/arm64)")
	tags := fs.String("t", "", "image tag (repeatable, comma-separated)")
	bases := fs.String("base", "", "base image mapping name=path (repeatable, comma-separated)")
	epoch := fs.Int64("epoch", 0, "fixed build epoch (unix seconds) for reproducibility")
	maxLayer := fs.Int64("max-layer-size", 0, "maximum uncompressed layer size in bytes (0 = unlimited)")
	maxFile := fs.Int64("max-context-file", 0, "maximum context file size in bytes (0 = unlimited)")
	workers := fs.Int("workers", 4, "parallel stage workers")
	if err := fs.Parse(args); err != nil {
		return err
	}

	absFile, err := filepath.Abs(*file)
	if err != nil {
		return err
	}
	absContext, err := filepath.Abs(*contextDir)
	if err != nil {
		return err
	}
	absOut, err := filepath.Abs(*output)
	if err != nil {
		return err
	}

	plats, err := builder.ParsePlatforms(*platforms)
	if err != nil {
		return err
	}

	req := &builder.BuildRequest{
		DescriptionFile: absFile,
		ContextDir:      absContext,
		Output:          absOut,
		Platforms:       plats,
		CacheDir:        *cacheDir,
		Epoch:           *epoch,
		MaxLayerSize:    *maxLayer,
		MaxFileSize:     *maxFile,
		Workers:         *workers,
		Bases:           map[string]string{},
	}
	if *tags != "" {
		req.Tags = strings.Split(*tags, ",")
	}
	if *bases != "" {
		for _, b := range strings.Split(*bases, ",") {
			eq := strings.IndexByte(b, '=')
			if eq < 0 {
				return fmt.Errorf("--base expects name=path, got %q", b)
			}
			name, path := b[:eq], b[eq+1:]
			absPath, err := filepath.Abs(path)
			if err != nil {
				return err
			}
			req.Bases[name] = absPath
		}
	}

	res, err := builder.Build(context.Background(), req)
	if err != nil {
		return err
	}
	fmt.Printf("Successfully built %s\n", res.Output)
	for plat, d := range res.ManifestDigests {
		fmt.Printf("  %s: %s\n", plat, d)
	}
	fmt.Printf("  index: %s\n", res.IndexDigest)
	return nil
}
