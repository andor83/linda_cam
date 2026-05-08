package capture

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/image/draw"
)

const (
	thumbDirName = ".thumbs"
	thumbWidth   = 320
	thumbQuality = 80
	metaSuffix   = ".meta.json"
	cropSuffix   = ".crop.jpg"
	// savedJPEGQuality + savedMaxWidth control the on-disk size of
	// every saved gallery picture. Re-encode at q80 alone caps out
	// around 1.3 MB on a 4K source — pixel count dominates. Combining
	// with a half-resolution 1920px max width brings 4K frames to
	// ~0.7 MB while keeping more than enough detail for the gallery
	// preview and species classifier.
	//
	// Crops live in their own files (.crop.<n>.jpg) and are saved at
	// q90 — independent of these settings — because they're already
	// small (the bird's bbox) and sharper crops help the classifier.
	savedJPEGQuality = 80
	savedMaxWidth    = 1920
)

// BBox is a normalized bounding box (each coord in [0,1] of the source frame).
// Mirrors detector.BBox so the capture package has no upstream dependency.
type BBox struct {
	X1 float64 `json:"x1"`
	Y1 float64 `json:"y1"`
	X2 float64 `json:"x2"`
	Y2 float64 `json:"y2"`
}

// DetectionRecord is one persisted YOLO detection: class name, confidence,
// and the bounding box it was at, normalized to the source frame dimensions.
type DetectionRecord struct {
	Name       string  `json:"name"`
	Confidence float64 `json:"confidence"`
	Box        BBox    `json:"box"`
}

// SpeciesGuess is one classifier prediction stored on a picture's sidecar.
// Mirrors classifier.Guess but kept here so the capture package has no
// dependency on the ONNX classifier.
type SpeciesGuess struct {
	Name       string  `json:"name"`
	Confidence float64 `json:"confidence"`
}

// BirdCropInfo describes one bird crop sub-image saved alongside a
// picture. A sighting can carry multiple crops when several birds
// were visible during the session window. Each crop has its own
// classifier guesses, AI-quality score, YOLO confidence, and the
// normalized bbox in the parent frame it was extracted from.
type BirdCropInfo struct {
	Filename    string         `json:"filename"`
	Species     []SpeciesGuess `json:"species,omitempty"`
	AIScore     int            `json:"ai_score,omitempty"`
	YOLOConf    float64        `json:"yolo_conf,omitempty"`
	Box         BBox           `json:"box,omitempty"`
	UserSpecies string         `json:"user_species,omitempty"` // human override for this specific bird
}

// Metadata is the on-disk sidecar (`<picture>.meta.json`) holding any
// after-the-fact analysis we attach to a saved picture.
type Metadata struct {
	Detections     []DetectionRecord `json:"detections,omitempty"`
	BirdSpecies    []SpeciesGuess    `json:"bird_species,omitempty"` // legacy single-crop species (fallback)
	BirdCrop       string            `json:"bird_crop,omitempty"`    // legacy single-crop filename (fallback)
	BirdCrops      []BirdCropInfo    `json:"bird_crops,omitempty"`   // multi-crop bird sightings
	UserSpecies    string            `json:"user_species,omitempty"` // human-curated override
	UserNotes      string            `json:"user_notes,omitempty"`
	AnalyzedAt     *time.Time        `json:"analyzed_at,omitempty"`
	ReclassifiedAt *time.Time        `json:"reclassified_at,omitempty"`

	// AIQualityScore is the most-recent 0–100 score the AI image-quality
	// service returned for this picture (nil → never scored). Persisted so
	// the gallery can display it without round-tripping the AI on every view.
	AIQualityScore *int       `json:"ai_quality_score,omitempty"`
	AIQualityAt    *time.Time `json:"ai_quality_at,omitempty"`
	// AIQualityError records the last scoring failure (network, parse,
	// "no choices in response", etc.) so the gallery can flag pictures
	// whose AI score is missing because the call errored out — distinct
	// from "not scored yet". Empty string means no failure to report.
	AIQualityError string `json:"ai_quality_error,omitempty"`
	// AIQualityRaw is the model's raw response content from the last
	// quality scoring call — surfaced in the gallery modal's Debug tab
	// so the user can see exactly what the LLM returned.
	AIQualityRaw string `json:"ai_quality_raw,omitempty"`
}

// MetaStore is the persistence layer behind picture metadata. The
// concrete implementation (internal/sightings) is SQLite-backed, but
// capture only sees this interface so it has no DB dependency.
type MetaStore interface {
	Insert(name string, savedAt time.Time) error
	Upsert(name string, savedAt time.Time, md Metadata) error
	Get(name string) (Metadata, bool, error)
	Delete(name string) error
	List(picturesDir string) ([]Picture, error)
	SetHeart(name string, hearted bool) error
	Hearted(name string) (bool, error)
	MarkPictureDeleted(name string) error
	CullCandidates(cutoff time.Time) ([]string, error)
}

type Store struct {
	dir  string
	meta MetaStore
}

// New constructs a Store backed by `dir` for files and `meta` for
// metadata + the picture index. `meta` is required.
func New(dir string, meta MetaStore) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir pictures: %w", err)
	}
	if meta == nil {
		return nil, errors.New("capture: meta store is required")
	}
	return &Store{dir: dir, meta: meta}, nil
}

func (s *Store) Dir() string { return s.dir }

type Reason struct {
	Manual     bool
	Species    string
	Confidence float64
}

// Replace atomically overwrites an existing capture's JPEG with new bytes
// and invalidates the cached thumbnail so subsequent ?thumb=1 requests
// regenerate against the new content. The caller is responsible for re-
// running analysis afterwards to refresh the .crop.jpg and .meta.json
// sidecars (typically via Detector.persistAnalysis).
func (s *Store) Replace(name string, jpegBytes []byte) error {
	if len(jpegBytes) == 0 {
		return errors.New("empty jpeg")
	}
	jpegBytes = recompressOrPassthrough(jpegBytes)
	p, err := s.Path(name)
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, jpegBytes, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	// Drop the cached thumbnail; ThumbnailPath regenerates from source on
	// next read.
	_ = os.Remove(filepath.Join(s.dir, thumbDirName, name))
	return nil
}

// recompressOrPassthrough decodes the JPEG, downscales to
// savedMaxWidth if it's wider, and re-encodes at savedJPEGQuality.
// Decode failures (partial frames from the extractor, exotic JPEG
// variants) fall back to the original bytes — better to keep an
// oversized picture than to drop the capture entirely. If the
// re-encoded result is somehow larger than the input (rare; happens
// when the source was already aggressively compressed), we also keep
// the original.
func recompressOrPassthrough(in []byte) []byte {
	img, err := jpeg.Decode(bytes.NewReader(in))
	if err != nil {
		return in
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
		return in
	}
	if out.Len() >= len(in) {
		return in
	}
	return out.Bytes()
}

func (s *Store) Save(jpegBytes []byte, reason Reason) (string, error) {
	if len(jpegBytes) == 0 {
		return "", errors.New("no frame available yet")
	}
	jpegBytes = recompressOrPassthrough(jpegBytes)
	now := time.Now()
	ts := now.Format("2006-01-02_15-04-05")
	var suffix string
	switch {
	case reason.Manual:
		suffix = "manual"
	case reason.Species != "":
		suffix = fmt.Sprintf("%s_%02d", sanitizeSpecies(reason.Species), int(reason.Confidence*100))
	default:
		suffix = "auto"
	}
	name := fmt.Sprintf("%s_%s.jpg", ts, suffix)
	path := filepath.Join(s.dir, name)
	if err := os.WriteFile(path, jpegBytes, 0o644); err != nil {
		return "", err
	}
	if err := s.meta.Insert(name, now); err != nil {
		// Best-effort: file is on disk; if the row insert failed, the
		// startup backfill will pick it up next launch. Don't fail the
		// save.
		fmt.Fprintf(os.Stderr, "capture: meta insert %s: %v\n", name, err)
	}
	return name, nil
}

func sanitizeSpecies(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	b := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			b = append(b, c)
		case c == ' ', c == '-', c == '_':
			b = append(b, '-')
		}
	}
	if len(b) == 0 {
		return "animal"
	}
	return string(b)
}

type Picture struct {
	Name           string            `json:"name"`
	Size           int64             `json:"size"`
	ModTime        time.Time         `json:"mod_time"`
	Species        string            `json:"species,omitempty"`
	Manual         bool              `json:"manual"`
	Detections     []DetectionRecord `json:"detections,omitempty"`
	BirdSpecies    []SpeciesGuess    `json:"bird_species,omitempty"`
	HasCrop        bool              `json:"has_crop,omitempty"`
	BirdCrop       string            `json:"-"` // populated from db; not surfaced to client
	UserSpecies    string            `json:"user_species,omitempty"`
	UserNotes      string            `json:"user_notes,omitempty"`
	AnalyzedAt     *time.Time        `json:"analyzed_at,omitempty"`
	ReclassifiedAt *time.Time        `json:"reclassified_at,omitempty"`
	AIQualityScore *int              `json:"ai_quality_score,omitempty"`
	AIQualityAt    *time.Time        `json:"ai_quality_at,omitempty"`
	AIQualityError string            `json:"ai_quality_error,omitempty"`
	BirdCrops      []BirdCropInfo    `json:"bird_crops,omitempty"`
	Hearted        bool              `json:"hearted,omitempty"`
}

// List returns the live (non-culled) pictures, newest first. Backed
// by the SQL index, not a directory walk.
func (s *Store) List() ([]Picture, error) {
	return s.meta.List(s.dir)
}

// ReadMetadata returns the metadata stored in the SQL row. The bool
// is false when the row doesn't exist (caller treats that as
// "no analysis yet").
func (s *Store) ReadMetadata(name string) (Metadata, bool, error) {
	if _, err := s.Path(name); err != nil {
		return Metadata{}, false, err
	}
	return s.meta.Get(name)
}

// WriteMetadata upserts md into the SQL row for `name`. The row is
// expected to already exist (Save inserts it); when it doesn't, the
// upsert creates one with savedAt = now (handles backfill races).
func (s *Store) WriteMetadata(name string, md Metadata) error {
	if _, err := s.Path(name); err != nil {
		return err
	}
	return s.meta.Upsert(name, time.Now(), md)
}

// SetHeart marks/unmarks a picture as hearted. Hearted pictures are
// excluded from the retention cull.
func (s *Store) SetHeart(name string, hearted bool) error {
	if _, err := s.Path(name); err != nil {
		return err
	}
	return s.meta.SetHeart(name, hearted)
}

// Hearted reports whether the picture is currently hearted.
func (s *Store) Hearted(name string) (bool, error) {
	if _, err := s.Path(name); err != nil {
		return false, err
	}
	return s.meta.Hearted(name)
}

func (s *Store) Path(name string) (string, error) {
	if strings.Contains(name, "/") || strings.Contains(name, "\\") || strings.Contains(name, "..") {
		return "", errors.New("invalid name")
	}
	return filepath.Join(s.dir, name), nil
}

// speciesFromName pulls the species token from a filename produced by Save.
// Files are named <ts>_<suffix>.jpg where suffix is "manual", "auto", or
// "<species>_<conf>". Returns "" for manual/auto/unknown shapes.
func speciesFromName(name string) string {
	n := strings.TrimSuffix(strings.ToLower(name), ".jpg")
	parts := strings.Split(n, "_")
	// Expected shapes:
	//   2026-04-21_15-04-05_manual            → len 3
	//   2026-04-21_15-04-05_auto              → len 3
	//   2026-04-21_15-04-05_<species>_<conf>  → len 4
	if len(parts) != 4 {
		return ""
	}
	sp := parts[2]
	if sp == "manual" || sp == "auto" {
		return ""
	}
	return sp
}

// Delete removes the picture entirely: file, thumb, crop, the legacy
// .meta.json sidecar (if present from a pre-migration save), and the
// SQL row. Used for manual user deletes from the gallery.
func (s *Store) Delete(name string) error {
	p, err := s.Path(name)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	_ = os.Remove(filepath.Join(s.dir, thumbDirName, name))
	_ = os.Remove(filepath.Join(s.dir, name+cropSuffix))
	s.removeIndexedCrops(name)
	_ = os.Remove(filepath.Join(s.dir, name+metaSuffix))
	if err := s.meta.Delete(name); err != nil {
		return err
	}
	return nil
}

// deleteFilesOnly removes file artifacts (jpg + thumb + crop + legacy
// sidecar) but leaves the SQL row in place. Used by the retention
// sweep so the row's metadata stays around for statistics after the
// jpg has been culled.
func (s *Store) deleteFilesOnly(name string) error {
	p, err := s.Path(name)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	_ = os.Remove(filepath.Join(s.dir, thumbDirName, name))
	_ = os.Remove(filepath.Join(s.dir, name+cropSuffix))
	s.removeIndexedCrops(name)
	_ = os.Remove(filepath.Join(s.dir, name+metaSuffix))
	return nil
}

// WriteCrop persists JPEG-encoded crop bytes alongside the source picture as
// `<name>.crop.jpg` and returns the new file's name (relative to the store
// dir). Empty input is a no-op returning "".
func (s *Store) WriteCrop(name string, jpegBytes []byte) (string, error) {
	if len(jpegBytes) == 0 {
		return "", nil
	}
	if _, err := s.Path(name); err != nil {
		return "", err
	}
	cropName := name + cropSuffix
	cropPath := filepath.Join(s.dir, cropName)
	tmp := cropPath + ".tmp"
	if err := os.WriteFile(tmp, jpegBytes, 0o644); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, cropPath); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	return cropName, nil
}

// WriteCropImage encodes img as JPEG (quality 90) and persists it as the
// legacy single-crop sidecar (`<name>.crop.jpg`) for the named picture.
// Used by paths that haven't migrated to indexed multi-crop yet.
func (s *Store) WriteCropImage(name string, img image.Image) (string, error) {
	return s.writeCropImageAt(name, img, name+cropSuffix)
}

// WriteCropImageIndexed encodes img and persists it as the indexed
// multi-crop sidecar (`<name>.crop.<index>.jpg`). Returns the crop's
// filename for storage in the sightings row's crops_json.
func (s *Store) WriteCropImageIndexed(name string, index int, img image.Image) (string, error) {
	cropName := indexedCropName(name, index)
	return s.writeCropImageAt(name, img, cropName)
}

func (s *Store) writeCropImageAt(name string, img image.Image, cropName string) (string, error) {
	if img == nil {
		return "", nil
	}
	if _, err := s.Path(name); err != nil {
		return "", err
	}
	cropPath := filepath.Join(s.dir, cropName)
	tmp := cropPath + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return "", err
	}
	if err := jpeg.Encode(out, img, &jpeg.Options{Quality: 90}); err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		return "", err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	if err := os.Rename(tmp, cropPath); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	return cropName, nil
}

// indexedCropName produces "<picture>.crop.<index>.jpg" for the
// multi-crop bird sighting sidecars.
func indexedCropName(name string, index int) string {
	return fmt.Sprintf("%s.crop.%d.jpg", name, index)
}

// removeIndexedCrops walks the pictures directory removing every
// `<name>.crop.<n>.jpg` for the given picture. Indexed crops can
// be 0..N for any N; we glob rather than hardcode a count.
func (s *Store) removeIndexedCrops(name string) {
	matches, _ := filepath.Glob(filepath.Join(s.dir, name+".crop.*.jpg"))
	for _, m := range matches {
		_ = os.Remove(m)
	}
}

// RemoveAllCrops deletes every crop sidecar for a picture: both the
// legacy single-crop file and every indexed multi-crop file. Used
// by manual reclassify before writing fresh crops.
func (s *Store) RemoveAllCrops(name string) {
	_ = os.Remove(filepath.Join(s.dir, name+cropSuffix))
	s.removeIndexedCrops(name)
}

// CropPath returns the absolute path to a previously-saved legacy
// single-crop. New multi-crop sightings expose their crop files via
// IndexedCropPath instead.
func (s *Store) CropPath(name string) (string, error) {
	if _, err := s.Path(name); err != nil {
		return "", err
	}
	cp := filepath.Join(s.dir, name+cropSuffix)
	if _, err := os.Stat(cp); err != nil {
		return "", err
	}
	return cp, nil
}

// IndexedCropPath returns the absolute path to a multi-crop sidecar
// at the given index, or an error wrapping os.ErrNotExist if it
// doesn't exist.
func (s *Store) IndexedCropPath(name string, index int) (string, error) {
	if _, err := s.Path(name); err != nil {
		return "", err
	}
	cp := filepath.Join(s.dir, indexedCropName(name, index))
	if _, err := os.Stat(cp); err != nil {
		return "", err
	}
	return cp, nil
}

// ThumbnailPath ensures a cached 320px-wide JPEG thumbnail exists for the
// named picture and returns its path. A cached thumb is reused if its mtime
// is newer than the source image, otherwise it's regenerated.
func (s *Store) ThumbnailPath(name string) (string, error) {
	srcPath, err := s.Path(name)
	if err != nil {
		return "", err
	}
	srcInfo, err := os.Stat(srcPath)
	if err != nil {
		return "", err
	}
	thumbPath := filepath.Join(s.dir, thumbDirName, name)
	if ti, err := os.Stat(thumbPath); err == nil && ti.ModTime().After(srcInfo.ModTime()) {
		return thumbPath, nil
	}
	if err := generateThumb(srcPath, thumbPath); err != nil {
		return "", err
	}
	return thumbPath, nil
}

// PurgeOlderThan removes the file artifacts (jpg, thumb, crop, legacy
// sidecar) for every non-hearted picture saved more than `age` ago,
// then flips the SQL row's picture_deleted flag — the row stays so
// historical statistics remain accurate. Hearted pictures are kept
// indefinitely (files included).
func (s *Store) PurgeOlderThan(age time.Duration) (int, []error) {
	cutoff := time.Now().Add(-age)
	names, err := s.meta.CullCandidates(cutoff)
	if err != nil {
		return 0, []error{err}
	}
	var errs []error
	removed := 0
	for _, name := range names {
		if err := s.deleteFilesOnly(name); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
			continue
		}
		if err := s.meta.MarkPictureDeleted(name); err != nil {
			errs = append(errs, fmt.Errorf("%s: mark deleted: %w", name, err))
			continue
		}
		removed++
	}
	return removed, errs
}

// PurgeOrphanThumbs removes any cached thumbnails whose source picture has
// been deleted out from under us (e.g., by manual rm).
func (s *Store) PurgeOrphanThumbs() (int, error) {
	thumbDir := filepath.Join(s.dir, thumbDirName)
	entries, err := os.ReadDir(thumbDir)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		src := filepath.Join(s.dir, e.Name())
		if _, err := os.Stat(src); errors.Is(err, os.ErrNotExist) {
			if err := os.Remove(filepath.Join(thumbDir, e.Name())); err == nil {
				removed++
			}
		}
	}
	return removed, nil
}

func generateThumb(srcPath, thumbPath string) error {
	srcFile, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer srcFile.Close()
	src, err := jpeg.Decode(srcFile)
	if err != nil {
		return fmt.Errorf("decode %s: %w", srcPath, err)
	}
	sb := src.Bounds()
	if sb.Dx() == 0 || sb.Dy() == 0 {
		return errors.New("source image is empty")
	}
	tw := thumbWidth
	th := sb.Dy() * thumbWidth / sb.Dx()
	if th < 1 {
		th = 1
	}
	dst := image.NewRGBA(image.Rect(0, 0, tw, th))
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, sb, draw.Over, nil)
	if err := os.MkdirAll(filepath.Dir(thumbPath), 0o755); err != nil {
		return err
	}
	tmp := thumbPath + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if err := jpeg.Encode(out, dst, &jpeg.Options{Quality: thumbQuality}); err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, thumbPath)
}
