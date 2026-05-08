package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/linda/linda_cam/internal/aiquality"
	"github.com/linda/linda_cam/internal/auth"
	"github.com/linda/linda_cam/internal/birdinfo"
	"github.com/linda/linda_cam/internal/capture"
	"github.com/linda/linda_cam/internal/ebird"
	"github.com/linda/linda_cam/internal/classifier"
	"github.com/linda/linda_cam/internal/config"
	"github.com/linda/linda_cam/internal/detector"
	"github.com/linda/linda_cam/internal/detlog"
	"github.com/linda/linda_cam/internal/httpapi"
	"github.com/linda/linda_cam/internal/jpeg"
	"github.com/linda/linda_cam/internal/rtsp"
	"github.com/linda/linda_cam/internal/sightings"
	"github.com/linda/linda_cam/internal/stream"
	"github.com/linda/linda_cam/internal/web"
)

func main() {
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)

	exe, err := os.Executable()
	if err != nil {
		log.Fatalf("exe path: %v", err)
	}
	baseDir := filepath.Dir(exe)
	// When running `go run ./cmd/linda_cam` the binary lives in /tmp; fall
	// back to the current working directory in that case.
	if info, err := os.Stat(filepath.Join(baseDir, "web")); err != nil || !info.IsDir() {
		if cwd, err := os.Getwd(); err == nil {
			baseDir = cwd
		}
	}

	configPath := filepath.Join(baseDir, "config.json")
	picturesDir := filepath.Join(baseDir, "pictures")
	logDBPath := filepath.Join(baseDir, "log.db")
	sightingsDBPath := filepath.Join(baseDir, "sightings.db")
	hlsDir := filepath.Join(baseDir, "hls")
	ffmpegPath := resolveTool(baseDir, "ffmpeg")
	streamFFmpegPath := ffmpegPath
	if v := os.Getenv("LINDA_FFMPEG"); v != "" {
		streamFFmpegPath = v
	}
	audioMode := os.Getenv("LINDA_AUDIO_MODE")
	if audioMode == "" {
		audioMode = "copy"
	}
	ortLibPath := resolveLib(baseDir, "libonnxruntime.so")
	modelPath := filepath.Join(baseDir, "models", "yolov8n.onnx")
	birdModelPath := filepath.Join(baseDir, "models", "bird.onnx")
	birdClsModelPath := filepath.Join(baseDir, "models", "bird_classifier.onnx")
	birdClsClassesPath := filepath.Join(baseDir, "models", "bird_classifier_classes.json")

	cfgStore, err := config.New(configPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	sightingsStore, err := sightings.Open(sightingsDBPath)
	if err != nil {
		log.Fatalf("sightings db: %v", err)
	}
	defer sightingsStore.Close()

	captures, err := capture.New(picturesDir, sightingsStore)
	if err != nil {
		log.Fatalf("capture store: %v", err)
	}

	// One-time backfill: any picture file present on disk without a
	// matching SQL row gets one (legacy sidecars are read once for
	// metadata, then ignored going forward). Cheap on subsequent boots
	// — already-indexed names are skipped by the SELECT pre-pass.
	// One-time cleanup: an earlier Backfill bug indexed multi-crop
	// sidecar files (`<name>.crop.<n>.jpg`) as top-level rows in the
	// sightings table, polluting the gallery with crop-as-picture
	// entries. Purge those rows + the orphan thumbnails they
	// generated. Idempotent — affects 0 rows on subsequent boots.
	if n, err := sightingsStore.PurgeCropRows(); err != nil {
		log.Printf("sightings: purge crop rows: %v", err)
	} else if n > 0 {
		log.Printf("sightings: purged %d bogus crop-as-picture rows", n)
	}
	if n, err := sightings.PurgeOrphanCropThumbs(picturesDir); err != nil {
		log.Printf("sightings: purge orphan crop thumbs: %v", err)
	} else if n > 0 {
		log.Printf("sightings: removed %d orphan crop thumbnails", n)
	}
	if n, err := sightingsStore.Backfill(picturesDir); err != nil {
		log.Printf("sightings: backfill: %v", err)
	} else if n > 0 {
		log.Printf("sightings: backfilled %d rows from disk", n)
	}
	// One-time recovery: rescue rows whose SQL fields were emptied by
	// a destructive bulk-reclassify run (pre-fix). Sidecars on disk
	// still hold the original bird_species + ai_quality_score; re-
	// import any field that's missing in SQL but present in sidecar.
	// Idempotent — only fields that are still empty get refilled.
	if n, err := sightingsStore.RecoverFromSidecars(picturesDir); err != nil {
		log.Printf("sightings: recover: %v", err)
	} else if n > 0 {
		log.Printf("sightings: recovered %d rows from legacy sidecars", n)
	}
	// Reconcile rows whose .jpg was deleted out-of-band (e.g. by a
	// previous version's retention sweep, or manual rm).
	if n, err := sightingsStore.ReconcilePictureDeleted(picturesDir); err != nil {
		log.Printf("sightings: reconcile: %v", err)
	} else if n > 0 {
		log.Printf("sightings: marked %d rows as picture_deleted (file missing)", n)
	}

	detLogger, err := detlog.Open(logDBPath)
	if err != nil {
		log.Fatalf("detection log: %v", err)
	}
	defer detLogger.Close()

	authMgr := auth.New(cfgStore)

	rtspClient := rtsp.NewClient()
	extractor := jpeg.New(ffmpegPath, 2)
	streamer := stream.New(streamFFmpegPath, audioMode, hlsDir)

	det := detector.New(extractor, captures, cfgStore, detLogger)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := det.Start(ctx, ortLibPath, modelPath, birdModelPath); err != nil {
		log.Printf("detector start: %v (continuing without detection)", err)
	}

	// Optional: load fine-grained bird species classifier. Missing files are
	// silently ignored (detector runs without species enrichment).
	if _, err := os.Stat(birdClsModelPath); err == nil {
		if _, err := os.Stat(birdClsClassesPath); err != nil {
			log.Printf("classifier: %s present but classes JSON missing at %s (skipping)", birdClsModelPath, birdClsClassesPath)
		} else if !det.Ready() {
			log.Printf("classifier: skipping load — detector not ready (ORT env missing)")
		} else if c, err := classifier.New(birdClsModelPath, birdClsClassesPath); err != nil {
			log.Printf("classifier: load failed: %v (skipping)", err)
		} else {
			det.SetBirdClassifier(c)
			log.Printf("classifier: loaded %s (%d species)", birdClsModelPath, len(c.Classes()))
		}
	}

	// Picture-retention sweep: once on startup, then every 24h. Deletes any
	// .jpg in pictures/ older than 30 days, orphan thumbnails, and detection
	// log rows past the same cutoff.
	go retentionLoop(ctx, captures, detLogger, 30*24*time.Hour)

	// apply any existing URL from config
	initial := cfgStore.Get()
	if initial.RTSPURL != "" {
		rtspClient.SetURL(initial.RTSPURL)
		extractor.SetURL(initial.RTSPURL)
		streamer.SetURL(initial.RTSPURL)
	}
	if initial.AIQuality.Enabled && initial.AIQuality.URL != "" && initial.AIQuality.Model != "" {
		det.SetAIQuality(aiquality.New(initial.AIQuality))
		log.Printf("aiquality: enabled (model=%s, url=%s, threshold=%d)",
			initial.AIQuality.Model, initial.AIQuality.URL, initial.AIQuality.DiscardThreshold)
	}

	// eBird location-aware species filter. ebirdRefreshLoop builds a
	// fresh service from cfgStore on startup + every 24h, refreshes it,
	// and sets it on the detector. A nudge channel lets OnConfigChange
	// trigger a refresh out-of-band when the user updates Settings.
	ebirdNudge := make(chan struct{}, 1)
	go ebirdRefreshLoop(ctx, det, cfgStore, ebirdNudge)

	api := httpapi.New(httpapi.Deps{
		Auth:       authMgr,
		Config:     cfgStore,
		Captures:   captures,
		Sightings:  sightingsStore,
		Extractor:  extractor,
		RTSP:       rtspClient,
		Streamer:   streamer,
		Detector:   det,
		DetLog:     detLogger,
		BirdInfo:   birdinfo.New(),
		FFmpegPath: ffmpegPath,
		HLSDir:     hlsDir,
		OnConfigChange: func(cfg config.Config) {
			rtspClient.SetURL(cfg.RTSPURL)
			extractor.SetURL(cfg.RTSPURL)
			streamer.SetURL(cfg.RTSPURL)
			if cfg.AIQuality.Enabled && cfg.AIQuality.URL != "" && cfg.AIQuality.Model != "" {
				det.SetAIQuality(aiquality.New(cfg.AIQuality))
			} else {
				det.SetAIQuality(nil)
			}
			// Nudge the eBird refresh loop so it rebuilds the service
			// from the new config (or clears it when disabled). The loop
			// is the single source of truth; sending is non-blocking.
			select {
			case ebirdNudge <- struct{}{}:
			default:
			}
		},
	})

	r := chi.NewRouter()
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Route("/api", api.Mount)
	r.Mount("/", web.Handler())

	addr := initial.HTTPAddr
	if addr == "" {
		addr = ":8001"
	}
	srv := &http.Server{
		Addr:              addr,
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("linda_cam listening on %s (base=%s)", addr, baseDir)
	log.Printf("ffmpeg=%s  stream-ffmpeg=%s  audio=%s  ort=%s  model=%s  bird-model=%s  bird-classifier=%s",
		ffmpegPath, streamFFmpegPath, audioMode, ortLibPath, modelPath, birdModelPath, birdClsModelPath)

	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("http: %v", err)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	<-sigCh
	log.Printf("shutting down")
	shutdownCtx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer scancel()
	_ = srv.Shutdown(shutdownCtx)
	rtspClient.Stop()
	extractor.Stop()
	streamer.Stop()
	det.Stop()
}

func resolveTool(baseDir, name string) string {
	for _, dir := range candidateDirs(baseDir, "bin") {
		p := filepath.Join(dir, name)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return name // fall back to PATH lookup
}

func resolveLib(baseDir, name string) string {
	for _, dir := range candidateDirs(baseDir, "lib") {
		p := filepath.Join(dir, name)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return name
}

// candidateDirs returns sub-directories to search for bundled tools/libs:
// the binary's own baseDir/<sub> first, then $LINDA_APP_DIR/<sub> if set
// (the Docker layout, where the binary baseDir falls back to cwd /data
// because there's no sibling web/ to anchor to).
func candidateDirs(baseDir, sub string) []string {
	dirs := []string{filepath.Join(baseDir, sub)}
	if app := os.Getenv("LINDA_APP_DIR"); app != "" && app != baseDir {
		dirs = append(dirs, filepath.Join(app, sub))
	}
	return dirs
}

// retentionLoop prunes pictures, orphaned thumbnails, and detection log
// rows older than `age` once immediately, then every 24 hours, until ctx
// is canceled.
func retentionLoop(ctx context.Context, captures *capture.Store, detLogger *detlog.Logger, age time.Duration) {
	run := func() {
		if n, errs := captures.PurgeOlderThan(age); n > 0 || len(errs) > 0 {
			log.Printf("retention: deleted %d pictures older than %v (errors: %d)", n, age, len(errs))
			for _, err := range errs {
				log.Printf("retention: %v", err)
			}
		}
		if n, err := captures.PurgeOrphanThumbs(); err != nil {
			log.Printf("retention: orphan-thumb sweep: %v", err)
		} else if n > 0 {
			log.Printf("retention: deleted %d orphan thumbnails", n)
		}
		if n, err := detLogger.PurgeOlderThan(age); err != nil {
			log.Printf("retention: detection-log sweep: %v", err)
		} else if n > 0 {
			log.Printf("retention: deleted %d detection log rows older than %v", n, age)
		}
	}
	run()
	t := time.NewTicker(24 * time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			run()
		}
	}
}

// ebirdRefreshLoop is the single source of truth for the detector's
// eBird service: on startup, every 24h, and any time a `nudge` arrives
// (e.g. the user saved Settings), it reads the current EBird config,
// builds a fresh service, refreshes it, and installs it on the
// detector. When the filter is disabled it clears the detector's
// service so classifier output is no longer post-filtered.
func ebirdRefreshLoop(ctx context.Context, det *detector.Detector, cfgStore *config.Store, nudge <-chan struct{}) {
	run := func() {
		cfg := cfgStore.Get().EBird
		if !cfg.Enabled {
			det.SetEBird(nil)
			return
		}
		svc := ebird.New(cfg)
		rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if err := svc.Refresh(rctx); err != nil {
			log.Printf("ebird: refresh: %v", err)
			// Don't clear an existing populated service on transient
			// failure — keep filtering with whatever we last had.
			return
		}
		det.SetEBird(svc)
		stats := svc.Stats()
		log.Printf("ebird: refreshed (%d species cached)", stats.Count)
	}
	run()
	t := time.NewTicker(24 * time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			run()
		case <-nudge:
			run()
		}
	}
}
