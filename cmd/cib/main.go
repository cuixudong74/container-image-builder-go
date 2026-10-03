// cib is the local container image builder CLI. It builds OCI image
// layouts from a build description file and a context directory, with no
// daemon and no network access.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/cuixudong74/container-image-builder-go/pkg/engine"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "cib: error: "+err.Error())
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 || args[0] != "build" {
		return fmt.Errorf("usage: cib build -f <buildfile> -C <context> -o <output> [--platform os/arch,...] [--cache-dir dir] [--epoch unix] [--max-layer-size bytes] [--timeout dur]")
	}
	fs := flag.NewFlagSet("build", flag.ContinueOnError)
	buildfile := fs.String("f", "Containerfile", "build description file")
	contextDir := fs.String("C", ".", "build context directory")
	output := fs.String("o", "", "output OCI layout directory (required)")
	platforms := fs.String("platform", "linux/amd64", "comma-separated os/arch list")
	cacheDir := fs.String("cache-dir", ".cib-cache", "layer cache directory")
	epoch := fs.Int64("epoch", 0, "fixed unix timestamp stamped into images (reproducibility)")
	maxLayer := fs.Int64("max-layer-size", 0, "maximum bytes per layer (0 = default)")
	timeout := fs.Duration("timeout", 0, "build timeout (0 = none)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *output == "" {
		return fmt.Errorf("-o output directory is required")
	}

	var plats []string
	for _, p := range strings.Split(*platforms, ",") {
		if p = strings.TrimSpace(p); p != "" {
			plats = append(plats, p)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *timeout)
		defer cancel()
	}

	eng, err := engine.New(engine.Options{
		BuildfilePath: *buildfile,
		ContextDir:    *contextDir,
		CacheDir:      *cacheDir,
		OutputDir:     *output,
		Platforms:     plats,
		Epoch:         time.Unix(*epoch, 0).UTC(),
		MaxLayerSize:  *maxLayer,
	})
	if err != nil {
		return err
	}
	res, err := eng.Build(ctx)
	if err != nil {
		return err
	}
	fmt.Println("index: " + res.IndexDigest)
	for _, p := range plats {
		fmt.Printf("manifest[%s]: %s\n", p, res.Manifests[p])
	}
	fmt.Println("output: " + res.OutputDir)
	return nil
}
