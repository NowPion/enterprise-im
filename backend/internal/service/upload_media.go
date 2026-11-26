package service

import (
	"bytes"
	"encoding/binary"
	"errors"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"math"
	"os"
	"path"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/gabriel-vasile/mimetype"
	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"

	"enterprise-im/backend/internal/config"
	"enterprise-im/backend/internal/model"
	"enterprise-im/backend/internal/pkg/errcode"
)

type mediaValidationResult struct {
	Mime               string
	Ext                string
	Width              *uint32
	Height             *uint32
	DurationMs         *uint32
	ThumbnailTempPath  string
	ThumbnailMime      *string
	ThumbnailExt       string
	ThumbnailSizeBytes *uint64
}

var imageAllowedMIMEs = map[string]string{
	"image/jpeg": ".jpg",
	"image/png":  ".png",
	"image/gif":  ".gif",
	"image/webp": ".webp",
}

var voiceAllowedMIMEs = map[string]string{
	"audio/wav":  ".wav",
	"audio/mpeg": ".mp3",
	"audio/ogg":  ".ogg",
	"audio/mp4":  ".m4a",
	"audio/aac":  ".aac",
	"audio/webm": ".webm",
}

var fileAllowedMIMEs = map[string]string{
	"image/jpeg": ".jpg",
	"image/png":  ".png",
	"image/gif":  ".gif",
	"image/webp": ".webp",
	"audio/wav":  ".wav",
	"audio/mpeg": ".mp3",
	"audio/ogg":  ".ogg",
	"audio/mp4":  ".m4a",
	"audio/aac":  ".aac",
	"audio/webm": ".webm",
	"video/mp4":  ".mp4",
	"video/webm": ".webm",
	"application/pdf":  ".pdf",
	"text/plain":       ".txt",
	"text/csv":         ".csv",
	"application/json": ".json",
	"application/zip":  ".zip",
	"application/gzip": ".gz",
	"application/x-tar": ".tar",
	"application/x-7z-compressed":  ".7z",
	"application/vnd.rar":          ".rar",
	"application/x-rar-compressed": ".rar",
	"application/msword":           ".doc",
	"application/vnd.openxmlformats-officedocument.wordprocessingml.document": ".docx",
	"application/vnd.ms-excel": ".xls",
	"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet": ".xlsx",
	"application/vnd.ms-powerpoint": ".ppt",
	"application/vnd.openxmlformats-officedocument.presentationml.presentation": ".pptx",
	"application/rtf": ".rtf",
}

var extensionExpectedMIMEs = map[string][]string{
	".jpg":      {"image/jpeg"},
	".jpeg":     {"image/jpeg"},
	".png":      {"image/png"},
	".gif":      {"image/gif"},
	".webp":     {"image/webp"},
	".wav":      {"audio/wav"},
	".mp3":      {"audio/mpeg"},
	".ogg":      {"audio/ogg"},
	".oga":      {"audio/ogg"},
	".opus":     {"audio/ogg"},
	".m4a":      {"audio/mp4"},
	".aac":      {"audio/aac"},
	".webm":     {"audio/webm", "video/webm"},
	".mp4":      {"video/mp4", "audio/mp4"},
	".pdf":      {"application/pdf"},
	".txt":      {"text/plain"},
	".log":      {"text/plain"},
	".md":       {"text/plain"},
	".markdown": {"text/plain"},
	".csv":      {"text/csv", "text/plain"},
	".json":     {"application/json", "text/plain"},
	".yaml":     {"text/plain"},
	".yml":      {"text/plain"},
	".zip":      {"application/zip"},
	".gz":       {"application/gzip"},
	".tgz":      {"application/gzip"},
	".tar":      {"application/x-tar"},
	".7z":       {"application/x-7z-compressed"},
	".rar":      {"application/vnd.rar", "application/x-rar-compressed"},
	".doc":      {"application/msword"},
	".docx":     {"application/vnd.openxmlformats-officedocument.wordprocessingml.document", "application/zip"},
	".xls":      {"application/vnd.ms-excel"},
	".xlsx":     {"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", "application/zip"},
	".ppt":      {"application/vnd.ms-powerpoint"},
	".pptx":     {"application/vnd.openxmlformats-officedocument.presentationml.presentation", "application/zip"},
	".rtf":      {"application/rtf"},
	".html":     {"text/html"},
	".htm":      {"text/html"},
	".svg":      {"image/svg+xml"},
	".js":       {"application/javascript", "text/javascript"},
	".exe":      {"application/x-msdownload", "application/vnd.microsoft.portable-executable"},
	".sh":       {"application/x-sh", "text/x-shellscript"},
}

// SanitizeFilename strips directory paths, control bytes, and header-unsafe characters
// while preserving safe UTF-8 characters and extension.
func SanitizeFilename(raw, fallbackExt string) string {
	replaced := strings.ReplaceAll(raw, "\\", "/")
	base := path.Base(replaced)
	if base == "." || base == ".." || base == "/" {
		base = ""
	}

	var b strings.Builder
	b.Grow(len(base))
	for _, r := range base {
		if r < 0x20 || r == 0x7f || r == '/' || r == '\\' || r == '"' || r == '\'' || r == ';' {
			continue
		}
		b.WriteRune(r)
	}
	cleaned := strings.TrimSpace(b.String())
	cleaned = strings.TrimLeft(cleaned, ".")
	cleaned = strings.TrimSpace(cleaned)
	if cleaned == "" {
		if fallbackExt == "" {
			return "upload"
		}
		return "upload" + fallbackExt
	}

	ext := filepath.Ext(cleaned)
	if len(cleaned) <= 200 {
		return cleaned
	}
	maxStemBytes := 200 - len(ext)
	if maxStemBytes < 16 {
		maxStemBytes = 200
		ext = ""
	}
	stem := strings.TrimSuffix(cleaned, ext)
	for len(stem) > maxStemBytes {
		_, size := utf8.DecodeLastRuneInString(stem)
		if size <= 0 {
			break
		}
		stem = stem[:len(stem)-size]
	}
	if stem == "" {
		stem = "upload"
	}
	return stem + ext
}

func normalizeMIMEType(raw string) string {
	base := strings.ToLower(strings.TrimSpace(strings.SplitN(raw, ";", 2)[0]))
	switch base {
	case "image/jpg", "image/pjpeg":
		return "image/jpeg"
	case "audio/x-wav", "audio/wave", "audio/vnd.wave":
		return "audio/wav"
	case "audio/mp3", "audio/x-mpeg":
		return "audio/mpeg"
	case "application/ogg", "audio/opus":
		return "audio/ogg"
	case "audio/x-m4a", "audio/m4a":
		return "audio/mp4"
	case "audio/x-aac":
		return "audio/aac"
	case "application/x-zip-compressed":
		return "application/zip"
	case "application/x-gzip":
		return "application/gzip"
	default:
		return base
	}
}

func detectRealMIME(tempPath, purpose, rawFilename, declaredMime string) (string, error) {
	detected, err := mimetype.DetectFile(tempPath)
	if err != nil {
		return "", err
	}
	mime := normalizeMIMEType(detected.String())

	// Raw MP3 frames without ID3 headers can sniff as application/octet-stream; verify >=2 valid MPEG frames.
	if mime == "application/octet-stream" &&
		(strings.EqualFold(filepath.Ext(rawFilename), ".mp3") || normalizeMIMEType(declaredMime) == "audio/mpeg") {
		if f, openErr := os.Open(tempPath); openErr == nil {
			defer func() { _ = f.Close() }()
			if ms, mp3Err := parseMP3DurationMs(f, 2); mp3Err == nil && ms > 0 {
				return "audio/mpeg", nil
			}
		}
	}

	if purpose == model.AssetPurposeVoice && mime == "video/webm" {
		return "audio/webm", nil
	}
	return mime, nil
}

func checkDeclaredConsistency(actualMime, declaredHeader, rawFilename string) *errcode.Error {
	if decl := normalizeMIMEType(declaredHeader); decl != "" && decl != "application/octet-stream" && decl != "binary/octet-stream" {
		if !mimesCompatible(actualMime, decl) {
			return errcode.ErrUploadTypeMismatch
		}
	}

	ext := strings.ToLower(filepath.Ext(strings.TrimSpace(rawFilename)))
	if ext != "" {
		if expected, known := extensionExpectedMIMEs[ext]; known {
			matched := false
			for _, candidate := range expected {
				if mimesCompatible(actualMime, candidate) {
					matched = true
					break
				}
			}
			if !matched {
				return errcode.ErrUploadTypeMismatch
			}
		}
	}
	return nil
}

func mimesCompatible(actual, declared string) bool {
	a := normalizeMIMEType(actual)
	d := normalizeMIMEType(declared)
	if a == d {
		return true
	}
	if (a == "text/plain" && (d == "text/csv" || d == "application/json")) ||
		(d == "text/plain" && (a == "text/csv" || a == "application/json")) {
		return true
	}
	if (a == "audio/webm" && d == "video/webm") || (a == "video/webm" && d == "audio/webm") {
		return true
	}
	if (a == "audio/mp4" && d == "video/mp4") || (a == "video/mp4" && d == "audio/mp4") {
		return true
	}
	if a == "application/zip" && (d == "application/vnd.openxmlformats-officedocument.wordprocessingml.document" ||
		d == "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet" ||
		d == "application/vnd.openxmlformats-officedocument.presentationml.presentation") {
		return true
	}
	if d == "application/zip" && (a == "application/vnd.openxmlformats-officedocument.wordprocessingml.document" ||
		a == "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet" ||
		a == "application/vnd.openxmlformats-officedocument.presentationml.presentation") {
		return true
	}
	if (a == "application/vnd.rar" && d == "application/x-rar-compressed") ||
		(a == "application/x-rar-compressed" && d == "application/vnd.rar") {
		return true
	}
	return false
}

func inspectAndValidateMedia(
	storage *DiskStorage,
	tempPath string,
	purpose string,
	declaredMime string,
	rawFilename string,
	cfg config.UploadConfig,
) (mediaValidationResult, *errcode.Error) {
	actualMime, err := detectRealMIME(tempPath, purpose, rawFilename, declaredMime)
	if err != nil {
		return mediaValidationResult{}, errcode.ErrInternalConsistency
	}

	if mismatchErr := checkDeclaredConsistency(actualMime, declaredMime, rawFilename); mismatchErr != nil {
		return mediaValidationResult{}, mismatchErr
	}

	var ext string
	var allowed bool
	switch purpose {
	case model.AssetPurposeImage, model.AssetPurposeAvatar, model.AssetPurposeGroupAvatar:
		ext, allowed = imageAllowedMIMEs[actualMime]
	case model.AssetPurposeVoice:
		ext, allowed = voiceAllowedMIMEs[actualMime]
	case model.AssetPurposeFile:
		ext, allowed = fileAllowedMIMEs[actualMime]
	default:
		return mediaValidationResult{}, errcode.ErrInvalidValue.WithMessage("invalid upload purpose")
	}
	if !allowed {
		return mediaValidationResult{}, errcode.ErrUploadInvalidFile
	}

	res := mediaValidationResult{
		Mime: actualMime,
		Ext:  ext,
	}

	switch purpose {
	case model.AssetPurposeImage, model.AssetPurposeAvatar, model.AssetPurposeGroupAvatar:
		return processImageMedia(storage, tempPath, res, cfg)
	case model.AssetPurposeVoice:
		return processVoiceMedia(tempPath, res, cfg)
	default:
		return res, nil
	}
}

func processImageMedia(
	storage *DiskStorage,
	tempPath string,
	res mediaValidationResult,
	cfg config.UploadConfig,
) (mediaValidationResult, *errcode.Error) {
	f, err := os.Open(tempPath)
	if err != nil {
		return mediaValidationResult{}, errcode.ErrInternalConsistency
	}
	defer func() { _ = f.Close() }()

	imgCfg, _, err := image.DecodeConfig(f)
	if err != nil || imgCfg.Width <= 0 || imgCfg.Height <= 0 {
		return mediaValidationResult{}, errcode.ErrUploadTypeMismatch
	}
	if imgCfg.Width > cfg.MaxImageWidth ||
		imgCfg.Height > cfg.MaxImageHeight ||
		int64(imgCfg.Width)*int64(imgCfg.Height) > cfg.MaxImagePixels {
		return mediaValidationResult{}, errcode.ErrUploadTooLarge
	}

	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return mediaValidationResult{}, errcode.ErrInternalConsistency
	}
	img, _, err := image.Decode(f)
	if err != nil {
		return mediaValidationResult{}, errcode.ErrUploadTypeMismatch
	}
	if res.Mime == "image/gif" {
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return mediaValidationResult{}, errcode.ErrInternalConsistency
		}
		if _, err := gif.DecodeAll(f); err != nil {
			return mediaValidationResult{}, errcode.ErrUploadTypeMismatch
		}
	}

	w := uint32(imgCfg.Width)
	h := uint32(imgCfg.Height)
	res.Width = &w
	res.Height = &h

	thumbTempPath, thumbMime, thumbExt, thumbSize, err := generateThumbnailFile(storage, img, res.Mime, cfg.ThumbnailMaxDim)
	if err != nil {
		return mediaValidationResult{}, errcode.ErrInternalConsistency
	}
	res.ThumbnailTempPath = thumbTempPath
	res.ThumbnailMime = &thumbMime
	res.ThumbnailExt = thumbExt
	res.ThumbnailSizeBytes = &thumbSize
	return res, nil
}

func generateThumbnailFile(
	storage *DiskStorage,
	src image.Image,
	origMime string,
	maxDim int,
) (string, string, string, uint64, error) {
	if maxDim <= 0 {
		maxDim = 320
	}
	bounds := src.Bounds()
	srcW := bounds.Dx()
	srcH := bounds.Dy()
	if srcW <= 0 || srcH <= 0 {
		return "", "", "", 0, errors.New("empty image bounds")
	}

	dstW, dstH := fitThumbnailDimensions(srcW, srcH, maxDim)
	dst := image.NewNRGBA(image.Rect(0, 0, dstW, dstH))
	draw.BiLinear.Scale(dst, dst.Bounds(), src, bounds, draw.Over, nil)

	thumbFile, thumbPath, err := storage.CreateTemp("thumb")
	if err != nil {
		return "", "", "", 0, err
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = thumbFile.Close()
			_ = os.Remove(thumbPath)
		}
	}()

	var thumbMime, thumbExt string
	if origMime == "image/jpeg" {
		thumbMime = "image/jpeg"
		thumbExt = ".jpg"
		if err := jpeg.Encode(thumbFile, dst, &jpeg.Options{Quality: 85}); err != nil {
			return "", "", "", 0, err
		}
	} else {
		thumbMime = "image/png"
		thumbExt = ".png"
		if err := png.Encode(thumbFile, dst); err != nil {
			return "", "", "", 0, err
		}
	}

	if err := thumbFile.Sync(); err != nil {
		return "", "", "", 0, err
	}
	info, err := thumbFile.Stat()
	if err != nil {
		return "", "", "", 0, err
	}
	if err := thumbFile.Close(); err != nil {
		return "", "", "", 0, err
	}
	cleanup = false
	return thumbPath, thumbMime, thumbExt, uint64(info.Size()), nil
}

func fitThumbnailDimensions(w, h, maxDim int) (int, int) {
	if w <= maxDim && h <= maxDim {
		return w, h
	}
	if w >= h {
		dstW := maxDim
		dstH := int(math.Round(float64(h) * float64(maxDim) / float64(w)))
		if dstH < 1 {
			dstH = 1
		}
		return dstW, dstH
	}
	dstH := maxDim
	dstW := int(math.Round(float64(w) * float64(maxDim) / float64(h)))
	if dstW < 1 {
		dstW = 1
	}
	return dstW, dstH
}

func processVoiceMedia(
	tempPath string,
	res mediaValidationResult,
	cfg config.UploadConfig,
) (mediaValidationResult, *errcode.Error) {
	f, err := os.Open(tempPath)
	if err != nil {
		return mediaValidationResult{}, errcode.ErrInternalConsistency
	}
	defer func() { _ = f.Close() }()

	durationMs, parseErr := parseAudioDurationMs(f, res.Mime)
	if parseErr != nil || durationMs == 0 {
		return mediaValidationResult{}, errcode.ErrUploadTypeMismatch
	}
	if cfg.MaxVoiceDurationMs > 0 && durationMs > cfg.MaxVoiceDurationMs {
		return mediaValidationResult{}, errcode.ErrUploadTooLarge
	}
	res.DurationMs = &durationMs
	return res, nil
}

func parseAudioDurationMs(r io.ReadSeeker, mime string) (uint32, error) {
	switch mime {
	case "audio/wav":
		return parseWAVDurationMs(r)
	case "audio/ogg":
		return parseOGGDurationMs(r)
	case "audio/mpeg":
		return parseMP3DurationMs(r, 1)
	case "audio/mp4":
		return parseMP4DurationMs(r)
	case "audio/webm":
		return parseWebMDurationMs(r)
	case "audio/aac":
		return parseADTSDurationMs(r)
	default:
		return 0, errors.New("unsupported audio mime")
	}
}

func parseWAVDurationMs(r io.ReadSeeker) (uint32, error) {
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return 0, err
	}
	var riff [12]byte
	if _, err := io.ReadFull(r, riff[:]); err != nil {
		return 0, err
	}
	if string(riff[0:4]) != "RIFF" || string(riff[8:12]) != "WAVE" {
		return 0, errors.New("invalid wav header")
	}

	var (
		hasFmt   bool
		hasData  bool
		byteRate uint32
		dataSize uint32
	)
	for {
		var chunkHdr [8]byte
		_, err := io.ReadFull(r, chunkHdr[:])
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			break
		}
		if err != nil {
			return 0, err
		}
		chunkID := string(chunkHdr[0:4])
		chunkSize := binary.LittleEndian.Uint32(chunkHdr[4:8])

		switch chunkID {
		case "fmt ":
			if chunkSize < 16 || chunkSize > 1024 {
				return 0, errors.New("invalid wav fmt chunk")
			}
			buf := make([]byte, chunkSize)
			if _, err := io.ReadFull(r, buf); err != nil {
				return 0, err
			}
			channels := binary.LittleEndian.Uint16(buf[2:4])
			sampleRate := binary.LittleEndian.Uint32(buf[4:8])
			byteRate = binary.LittleEndian.Uint32(buf[8:12])
			bitsPerSample := binary.LittleEndian.Uint16(buf[14:16])
			if byteRate == 0 && sampleRate > 0 && channels > 0 && bitsPerSample > 0 {
				byteRate = sampleRate * uint32(channels) * uint32(bitsPerSample) / 8
			}
			if sampleRate == 0 || byteRate == 0 {
				return 0, errors.New("invalid wav format")
			}
			hasFmt = true
			if chunkSize%2 == 1 {
				if _, err := r.Seek(1, io.SeekCurrent); err != nil {
					return 0, err
				}
			}
		case "data":
			dataSize = chunkSize
			hasData = true
			skip := int64(chunkSize) + int64(chunkSize%2)
			if _, err := r.Seek(skip, io.SeekCurrent); err != nil {
				return 0, err
			}
		default:
			skip := int64(chunkSize) + int64(chunkSize%2)
			if _, err := r.Seek(skip, io.SeekCurrent); err != nil {
				return 0, err
			}
		}
	}

	if !hasFmt || !hasData || byteRate == 0 || dataSize == 0 {
		return 0, errors.New("incomplete wav stream")
	}
	ms := (uint64(dataSize) * 1000) / uint64(byteRate)
	if ms == 0 {
		ms = 1
	}
	if ms > math.MaxUint32 {
		return math.MaxUint32, nil
	}
	return uint32(ms), nil
}

func parseOGGDurationMs(r io.ReadSeeker) (uint32, error) {
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return 0, err
	}
	var (
		sampleRate  uint64
		preSkip     uint64
		lastGranule int64 = -1
		pages       int
	)
	for {
		var hdr [27]byte
		_, err := io.ReadFull(r, hdr[:])
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if pages > 0 && errors.Is(err, io.ErrUnexpectedEOF) {
				break
			}
			return 0, err
		}
		if string(hdr[0:4]) != "OggS" || hdr[4] != 0 {
			if pages == 0 {
				return 0, errors.New("invalid ogg magic")
			}
			break
		}
		pages++
		granule := int64(binary.LittleEndian.Uint64(hdr[6:14]))
		if granule >= 0 {
			lastGranule = granule
		}
		numSegs := int(hdr[26])
		segTable := make([]byte, numSegs)
		if _, err := io.ReadFull(r, segTable); err != nil {
			return 0, err
		}
		var payloadLen int64
		for _, s := range segTable {
			payloadLen += int64(s)
		}
		if pages == 1 && payloadLen > 0 && payloadLen <= 4096 {
			payload := make([]byte, payloadLen)
			if _, err := io.ReadFull(r, payload); err != nil {
				return 0, err
			}
			if bytes.HasPrefix(payload, []byte("OpusHead")) && len(payload) >= 12 {
				sampleRate = 48000
				preSkip = uint64(binary.LittleEndian.Uint16(payload[10:12]))
			} else if bytes.HasPrefix(payload, []byte("\x01vorbis")) && len(payload) >= 16 {
				sampleRate = uint64(binary.LittleEndian.Uint32(payload[12:16]))
			}
		} else {
			if _, err := r.Seek(payloadLen, io.SeekCurrent); err != nil {
				return 0, err
			}
		}
	}

	if sampleRate == 0 || lastGranule <= 0 {
		return 0, errors.New("missing ogg audio metadata")
	}
	samples := uint64(lastGranule)
	if samples > preSkip {
		samples -= preSkip
	} else {
		samples = 0
	}
	if samples == 0 {
		return 0, errors.New("empty ogg audio")
	}
	ms := (samples * 1000) / sampleRate
	if ms == 0 {
		ms = 1
	}
	if ms > math.MaxUint32 {
		return math.MaxUint32, nil
	}
	return uint32(ms), nil
}

var (
	mpeg1Layer1Bitrates = [16]int{0, 32, 64, 96, 128, 160, 192, 224, 256, 288, 320, 352, 384, 416, 448, 0}
	mpeg1Layer2Bitrates = [16]int{0, 32, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 384, 0}
	mpeg1Layer3Bitrates = [16]int{0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 0}
	mpeg2Layer1Bitrates = [16]int{0, 32, 48, 56, 64, 80, 96, 112, 128, 144, 160, 176, 192, 224, 256, 0}
	mpeg2Layer23Bitrates = [16]int{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160, 0}
	mpegSampleRates     = [4][3]int{
		{11025, 12000, 8000},  // MPEG 2.5
		{0, 0, 0},             // reserved
		{22050, 24000, 16000}, // MPEG 2
		{44100, 48000, 32000}, // MPEG 1
	}
)

func parseMP3DurationMs(r io.ReadSeeker, minFrames int) (uint32, error) {
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return 0, err
	}
	data, err := io.ReadAll(io.LimitReader(r, 20<<20))
	if err != nil {
		return 0, err
	}
	pos := 0
	if len(data) >= 10 && string(data[:3]) == "ID3" {
		tagSize := int(data[6]&0x7F)<<21 | int(data[7]&0x7F)<<14 | int(data[8]&0x7F)<<7 | int(data[9]&0x7F)
		pos = 10 + tagSize
		if data[5]&0x10 != 0 {
			pos += 10
		}
	}
	if pos >= len(data) {
		return 0, errors.New("empty mp3")
	}

	var totalMs float64
	frames := 0
	for pos+4 <= len(data) {
		if pos+3 <= len(data) && string(data[pos:pos+3]) == "TAG" {
			break
		}
		if data[pos] != 0xFF || (data[pos+1]&0xE0) != 0xE0 {
			if frames > 0 {
				break
			}
			pos++
			continue
		}
		b1, b2 := data[pos+1], data[pos+2]
		versionID := int((b1 >> 3) & 0x03)
		layerID := int((b1 >> 1) & 0x03)
		bitrateIdx := int((b2 >> 4) & 0x0F)
		sampleIdx := int((b2 >> 2) & 0x03)
		padding := int((b2 >> 1) & 0x01)

		if versionID == 1 || layerID == 0 || bitrateIdx == 0 || bitrateIdx == 15 || sampleIdx == 3 {
			if frames > 0 {
				break
			}
			pos++
			continue
		}
		sampleRate := mpegSampleRates[versionID][sampleIdx]
		if sampleRate == 0 {
			pos++
			continue
		}

		var bitrateKbps int
		if versionID == 3 { // MPEG 1
			switch layerID {
			case 3:
				bitrateKbps = mpeg1Layer1Bitrates[bitrateIdx]
			case 2:
				bitrateKbps = mpeg1Layer2Bitrates[bitrateIdx]
			case 1:
				bitrateKbps = mpeg1Layer3Bitrates[bitrateIdx]
			}
		} else {
			if layerID == 3 {
				bitrateKbps = mpeg2Layer1Bitrates[bitrateIdx]
			} else {
				bitrateKbps = mpeg2Layer23Bitrates[bitrateIdx]
			}
		}
		if bitrateKbps == 0 {
			pos++
			continue
		}

		var samplesPerFrame int
		var frameLen int
		if layerID == 3 { // Layer I
			samplesPerFrame = 384
			frameLen = (12*(bitrateKbps*1000)/sampleRate + padding) * 4
		} else {
			if versionID == 3 || layerID == 2 {
				samplesPerFrame = 1152
			} else {
				samplesPerFrame = 576
			}
			coeff := 144
			if versionID != 3 && layerID == 1 {
				coeff = 72
			}
			frameLen = coeff*(bitrateKbps*1000)/sampleRate + padding
		}
		if frameLen < 4 || pos+frameLen > len(data) {
			break
		}

		frames++
		totalMs += float64(samplesPerFrame) * 1000.0 / float64(sampleRate)
		pos += frameLen
	}

	if frames < minFrames || totalMs <= 0 {
		return 0, errors.New("no valid mp3 frames")
	}
	ms := uint32(math.Round(totalMs))
	if ms == 0 {
		ms = 1
	}
	return ms, nil
}

func parseMP4DurationMs(r io.ReadSeeker) (uint32, error) {
	end, err := r.Seek(0, io.SeekEnd)
	if err != nil {
		return 0, err
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return 0, err
	}
	return scanMP4Boxes(r, 0, end)
}

func scanMP4Boxes(r io.ReadSeeker, start, end int64) (uint32, error) {
	offset := start
	for offset+8 <= end {
		if _, err := r.Seek(offset, io.SeekStart); err != nil {
			return 0, err
		}
		var hdr [8]byte
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			return 0, err
		}
		size32 := binary.BigEndian.Uint32(hdr[0:4])
		boxType := string(hdr[4:8])
		hdrLen := int64(8)
		boxSize := int64(size32)
		if size32 == 1 {
			var ext [8]byte
			if _, err := io.ReadFull(r, ext[:]); err != nil {
				return 0, err
			}
			boxSize = int64(binary.BigEndian.Uint64(ext[:]))
			hdrLen = 16
		} else if size32 == 0 {
			boxSize = end - offset
		}
		if boxSize < hdrLen || offset+boxSize > end {
			return 0, errors.New("invalid mp4 box size")
		}

		payloadStart := offset + hdrLen
		payloadEnd := offset + boxSize
		switch boxType {
		case "moov":
			if ms, err := scanMP4Boxes(r, payloadStart, payloadEnd); err == nil && ms > 0 {
				return ms, nil
			}
		case "mvhd":
			payloadLen := payloadEnd - payloadStart
			if payloadLen < 20 || payloadLen > 1024 {
				return 0, errors.New("invalid mvhd box")
			}
			buf := make([]byte, payloadLen)
			if _, err := io.ReadFull(r, buf); err != nil {
				return 0, err
			}
			version := buf[0]
			var timescale uint64
			var duration uint64
			if version == 0 && len(buf) >= 20 {
				timescale = uint64(binary.BigEndian.Uint32(buf[12:16]))
				duration = uint64(binary.BigEndian.Uint32(buf[16:20]))
			} else if version == 1 && len(buf) >= 32 {
				timescale = uint64(binary.BigEndian.Uint32(buf[20:24]))
				duration = binary.BigEndian.Uint64(buf[24:32])
			}
			if timescale == 0 || duration == 0 {
				return 0, errors.New("invalid mvhd timing")
			}
			ms := (duration * 1000) / timescale
			if ms == 0 {
				ms = 1
			}
			if ms > math.MaxUint32 {
				return math.MaxUint32, nil
			}
			return uint32(ms), nil
		}
		offset += boxSize
	}
	return 0, errors.New("mp4 mvhd not found")
}

func parseWebMDurationMs(r io.ReadSeeker) (uint32, error) {
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return 0, err
	}
	// Read prefix (Segment/Info typically sit near the beginning of WebM files).
	buf, err := io.ReadAll(io.LimitReader(r, 512*1024))
	if err != nil {
		return 0, err
	}
	if len(buf) < 4 || !bytes.HasPrefix(buf, []byte{0x1A, 0x45, 0xDF, 0xA3}) {
		return 0, errors.New("invalid ebml header")
	}

	timestampScale := uint64(1000000) // default 1ms in ns
	var durationVal float64
	hasDuration := false

	var scanEBML func(data []byte, depth int)
	scanEBML = func(data []byte, depth int) {
		if depth > 4 {
			return
		}
		i := 0
		for i < len(data) {
			id, idLen := readEBMLID(data[i:])
			if idLen == 0 || i+idLen >= len(data) {
				return
			}
			size, sizeLen, unknown := readEBMLSize(data[i+idLen:])
			if sizeLen == 0 {
				return
			}
			valStart := i + idLen + sizeLen
			if unknown {
				// Streaming Segment/Info with unknown size: continue scanning inside.
				if id == 0x18538067 || id == 0x1549A966 {
					scanEBML(data[valStart:], depth+1)
				}
				return
			}
			if valStart+int(size) > len(data) {
				if id == 0x18538067 || id == 0x1549A966 {
					scanEBML(data[valStart:], depth+1)
				}
				return
			}
			payload := data[valStart : valStart+int(size)]
			switch id {
			case 0x18538067, 0x1549A966: // Segment, Info
				scanEBML(payload, depth+1)
			case 0x2AD7B1: // TimestampScale
				if len(payload) > 0 && len(payload) <= 8 {
					var v uint64
					for _, b := range payload {
						v = (v << 8) | uint64(b)
					}
					if v > 0 {
						timestampScale = v
					}
				}
			case 0x4489: // Duration
				if len(payload) == 4 {
					durationVal = float64(math.Float32frombits(binary.BigEndian.Uint32(payload)))
					hasDuration = true
				} else if len(payload) == 8 {
					durationVal = math.Float64frombits(binary.BigEndian.Uint64(payload))
					hasDuration = true
				}
			}
			i = valStart + int(size)
		}
	}

	scanEBML(buf, 0)
	if !hasDuration || durationVal <= 0 {
		return 0, errors.New("webm duration not found")
	}
	msFloat := durationVal * float64(timestampScale) / 1e6
	if msFloat <= 0 {
		return 0, errors.New("invalid webm duration")
	}
	ms := uint32(math.Round(msFloat))
	if ms == 0 {
		ms = 1
	}
	return ms, nil
}

func readEBMLID(b []byte) (uint32, int) {
	if len(b) == 0 || b[0] == 0 {
		return 0, 0
	}
	width := 1
	mask := byte(0x80)
	for width <= 4 && (b[0]&mask) == 0 {
		width++
		mask >>= 1
	}
	if width > 4 || width > len(b) {
		return 0, 0
	}
	var id uint32
	for i := 0; i < width; i++ {
		id = (id << 8) | uint32(b[i])
	}
	return id, width
}

func readEBMLSize(b []byte) (uint64, int, bool) {
	if len(b) == 0 || b[0] == 0 {
		return 0, 0, false
	}
	width := 1
	mask := byte(0x80)
	for width <= 8 && (b[0]&mask) == 0 {
		width++
		mask >>= 1
	}
	if width > 8 || width > len(b) {
		return 0, 0, false
	}
	val := uint64(b[0] & (mask - 1))
	allOnes := (b[0] & (mask - 1)) == (mask - 1)
	for i := 1; i < width; i++ {
		val = (val << 8) | uint64(b[i])
		if b[i] != 0xFF {
			allOnes = false
		}
	}
	return val, width, allOnes
}

var adtsSampleRates = [16]int{
	96000, 88200, 64000, 48000, 44100, 32000, 24000, 22050,
	16000, 12000, 11025, 8000, 7350, 0, 0, 0,
}

func parseADTSDurationMs(r io.ReadSeeker) (uint32, error) {
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return 0, err
	}
	data, err := io.ReadAll(io.LimitReader(r, 20<<20))
	if err != nil {
		return 0, err
	}
	pos := 0
	frames := 0
	var totalMs float64
	for pos+7 <= len(data) {
		if data[pos] != 0xFF || (data[pos+1]&0xF0) != 0xF0 {
			break
		}
		srIdx := int((data[pos+2] >> 2) & 0x0F)
		sr := adtsSampleRates[srIdx]
		frameLen := (int(data[pos+3]&0x03) << 11) | (int(data[pos+4]) << 3) | (int(data[pos+5]) >> 5)
		if sr == 0 || frameLen < 7 || pos+frameLen > len(data) {
			break
		}
		frames++
		totalMs += 1024.0 * 1000.0 / float64(sr)
		pos += frameLen
	}
	if frames == 0 || totalMs <= 0 {
		return 0, errors.New("invalid aac adts stream")
	}
	ms := uint32(math.Round(totalMs))
	if ms == 0 {
		ms = 1
	}
	return ms, nil
}
