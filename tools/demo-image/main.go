// Command demo-image writes the synthetic RKFW image the page's demo mode uses
// to a file, so the zip path can be exercised against a real image without a
// vendor blob (and without a multi-GiB download).
//
// It is the same bytes cmd/wasm serves as `rk3588.demoImage()`: internal/
// imageplan.DemoImage is the single source of truth for both.
//
// Usage: pixi run sample   (writes web/samples/demo-update.img + .zip)
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/SilenWang/rk3588-webflasher/internal/imageplan"
)

func main() {
	out := flag.String("out", "-", "path to write the demo .img to (\"-\" for stdout)")
	name := flag.String("name", imageplan.DemoImageName, "image name recorded in logs")
	flag.Parse()

	data := imageplan.DemoImage()
	if *out == "-" {
		if _, err := os.Stdout.Write(data); err != nil {
			fmt.Fprintf(os.Stderr, "demo-image: write stdout: %v\n", err)
			os.Exit(1)
		}
		return
	}
	if dir := filepath.Dir(*out); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "demo-image: create %s: %v\n", dir, err)
			os.Exit(1)
		}
	}
	if err := os.WriteFile(*out, data, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "demo-image: write %s: %v\n", *out, err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stdout, "%s: %s (%d bytes)\n", *name, *out, len(data))
}
