// Command recompress walks an existing pictures directory and
// re-encodes every top-level .jpg in place at the same quality +
// max-width settings the live capture path uses (defined in
// internal/capture). Skips files that are already small enough.
//
// Usage:
//
//	recompress [-dir <path>] [-dry-run]
//
// Defaults to ./pictures (relative to the working directory). The
// -dry-run flag scans without writing to show projected savings.
//
// Crop sidecars (.crop.jpg, .crop.<n>.jpg) and .meta.json files are
// skipped — only the source frames get rewritten.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"image"
	"image/jpeg"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/image/draw"
)

const (
	savedJPEGQuality = 80
	savedMaxWidth    = 1920
)

func main() {
	dir := flag.String("dir", "pictures", "pictures directory to walk")
	dryRun := flag.Bool("dry-run", false, "report projected savings without writing")
	flag.Parse()

	entries, err := os.ReadDir(*dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read %s: %v\n", *dir, err)
		os.Exit(1)
	}

	var (
		scanned   int
		processed int
		skipped   int
		failed    int
		bytesIn   int64
		bytesOut  int64
	)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		lower := strings.ToLower(name)
		if !strings.HasSuffix(lower, ".jpg") {
			continue
		}
		// Only top-level frames; skip crop sidecars.
		if strings.Contains(name, ".crop.") {
			continue
		}
		scanned++
		path := filepath.Join(*dir, name)
		body, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "read %s: %v\n", name, err)
			failed++
			continue
		}
		out, ok := recompress(body)
		if !ok || len(out) >= len(body) {
			skipped++
			continue
		}
		bytesIn += int64(len(body))
		bytesOut += int64(len(out))
		processed++
		if *dryRun {
			fmt.Printf("would-write %s: %d → %d (%.0f%%)\n",
				name, len(body), len(out),
				100*float64(len(out))/float64(len(body)))
			continue
		}
		tmp := path + ".tmp"
		if err := os.WriteFile(tmp, out, 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "write tmp %s: %v\n", name, err)
			failed++
			continue
		}
		if err := os.Rename(tmp, path); err != nil {
			fmt.Fprintf(os.Stderr, "rename %s: %v\n", name, err)
			_ = os.Remove(tmp)
			failed++
			continue
		}
		// Drop any cached thumbnail so the next gallery view regenerates.
		_ = os.Remove(filepath.Join(*dir, ".thumbs", name))
		fmt.Printf("rewrote %s: %d → %d (%.0f%%)\n",
			name, len(body), len(out),
			100*float64(len(out))/float64(len(body)))
	}

	tag := "rewritten"
	if *dryRun {
		tag = "would rewrite"
	}
	fmt.Printf("\nscanned=%d  %s=%d  skipped=%d  failed=%d\n",
		scanned, tag, processed, skipped, failed)
	if processed > 0 {
		fmt.Printf("size: %.1f MB → %.1f MB (saved %.1f MB)\n",
			float64(bytesIn)/1024/1024,
			float64(bytesOut)/1024/1024,
			float64(bytesIn-bytesOut)/1024/1024)
	}
}

func recompress(in []byte) ([]byte, bool) {
	img, err := jpeg.Decode(bytes.NewReader(in))
	if err != nil {
		return nil, false
	}
	if sb := img.Bounds(); sb.Dx() > savedMaxWidth {
		h := sb.Dy() * savedMaxWidth / sb.Dx()
		if h < 1 {
			h = 1
		}
		dst := image.NewRGBA(image.Rect(0, 0, savedMaxWidth, h))
		draw.CatmullRom.Scale(dst, dst.Bounds(), img, sb, draw.Over, nil)
		img = dst
	}
	var out bytes.Buffer
	if err := jpeg.Encode(&out, img, &jpeg.Options{Quality: savedJPEGQuality}); err != nil {
		return nil, false
	}
	return out.Bytes(), true
}
