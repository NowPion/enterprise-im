package service

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"

	"enterprise-im/backend/internal/config"
	"enterprise-im/backend/internal/model"
	"enterprise-im/backend/internal/pkg/errcode"
	"enterprise-im/backend/internal/pkg/logger"
	"enterprise-im/backend/internal/repository"
)

type memAssetStore struct {
	mu      sync.Mutex
	nextID  uint64
	assets  map[string]model.Asset
	users   map[uint64]model.User
	members map[[2]uint64]bool
}

func newMemAssetStore() *memAssetStore {
	return &memAssetStore{
		nextID: 1,
		assets: make(map[string]model.Asset),
		users: map[uint64]model.User{
			10001: {ID: 10001, Username: "alice", Nickname: "Alice", Role: model.UserRoleUser, Status: model.UserStatusActive},
			10002: {ID: 10002, Username: "bob", Nickname: "Bob", Role: model.UserRoleUser, Status: model.UserStatusActive},
			10003: {ID: 10003, Username: "carol", Nickname: "Carol", Role: model.UserRoleUser, Status: model.UserStatusActive},
		},
		members: map[[2]uint64]bool{
			{90001, 10001}: true,
			{90001, 10002}: true,
		},
	}
}

func (m *memAssetStore) Create(_ context.Context, asset *model.Asset) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextID++
	cp := *asset
	cp.ID = m.nextID
	cp.CreatedAt = time.Now().UTC()
	cp.UpdatedAt = cp.CreatedAt
	m.assets[cp.AssetID] = cp
	return nil
}

func (m *memAssetStore) CreateAvatarAndBindUser(_ context.Context, asset *model.Asset, userID uint64, avatarURL string, boundAt time.Time) (*model.User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.users[userID]
	if !ok {
		return nil, gorm.ErrRecordNotFound
	}
	if u.Status != model.UserStatusActive {
		return nil, repository.ErrUserDisabled
	}
	for id, existing := range m.assets {
		if existing.RefType == model.AssetRefUserAvatar && existing.RefID != nil && *existing.RefID == userID && existing.Status == model.AssetStatusActive {
			existing.RefType = model.AssetRefNone
			existing.RefID = nil
			existing.RefCount = 0
			m.assets[id] = existing
		}
	}
	m.nextID++
	refID := userID
	ts := boundAt.UTC()
	cp := *asset
	cp.ID = m.nextID
	cp.OwnerID = userID
	cp.RefType = model.AssetRefUserAvatar
	cp.RefID = &refID
	cp.RefCount = 1
	cp.BoundAt = &ts
	cp.Status = model.AssetStatusActive
	m.assets[cp.AssetID] = cp

	u.AvatarURL = avatarURL
	u.UpdatedAt = ts
	m.users[userID] = u
	userCopy := u
	return &userCopy, nil
}

func (m *memAssetStore) GetActiveByAssetID(_ context.Context, assetID string) (*model.Asset, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.assets[assetID]
	if !ok || a.Status != model.AssetStatusActive {
		return nil, repository.ErrAssetNotFound
	}
	cp := a
	return &cp, nil
}

func (m *memAssetStore) IsConversationMember(_ context.Context, conversationID, userID uint64) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.members[[2]uint64{conversationID, userID}], nil
}

func (m *memAssetStore) SoftDeleteOwned(_ context.Context, assetID string, ownerID uint64, deletedAt time.Time) (*model.Asset, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.assets[assetID]
	if !ok || a.Status != model.AssetStatusActive {
		return nil, repository.ErrAssetNotFound
	}
	if a.OwnerID != ownerID {
		return nil, repository.ErrAssetForbidden
	}
	if a.IsReferencedByMessage() {
		return nil, repository.ErrAssetReferencedByMessage
	}
	ts := deletedAt.UTC()
	a.Status = model.AssetStatusDeleted
	a.DeletedAt = &ts
	a.RefType = model.AssetRefNone
	a.RefID = nil
	a.RefConversationID = nil
	a.RefCount = 0
	m.assets[assetID] = a
	cp := a
	return &cp, nil
}

func (m *memAssetStore) bindToMessage(assetID string, msgID, convID uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a := m.assets[assetID]
	now := time.Now().UTC()
	a.RefType = model.AssetRefMessage
	a.RefID = &msgID
	a.RefConversationID = &convID
	a.RefCount = 1
	a.BoundAt = &now
	m.assets[assetID] = a
}

func newTestUploadService(t *testing.T) (*UploadService, *memAssetStore, string) {
	t.Helper()
	rootDir := filepath.Join(t.TempDir(), "uploads")
	storage, err := NewDiskStorage(rootDir, "")
	if err != nil {
		t.Fatalf("NewDiskStorage: %v", err)
	}
	store := newMemAssetStore()
	log, _ := logger.New(config.LogConfig{Level: "error", Format: "json"}, nil)
	cfg := config.UploadConfig{
		Dir:                rootDir,
		MaxImageBytes:      2 << 20,
		MaxFileBytes:       4 << 20,
		MaxVoiceBytes:      2 << 20,
		MaxAvatarBytes:     1 << 20,
		MaxImageWidth:      2048,
		MaxImageHeight:     2048,
		MaxImagePixels:     1000000,
		MaxVoiceDurationMs: 10000,
		ThumbnailMaxDim:    128,
		DownloadURLTTL:     15 * time.Minute,
	}
	svc := NewUploadService(store, nil, storage, cfg, "test-upload-signing-secret-32-bytes", log)
	return svc, store, rootDir
}

func writeStageTemp(t *testing.T, svc *UploadService, data []byte) string {
	t.Helper()
	f, path, err := svc.CreateTempFile("test")
	if err != nil {
		t.Fatalf("CreateTempFile: %v", err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		t.Fatalf("write temp: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close temp: %v", err)
	}
	return path
}

func makePNGBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: 20, G: 120, B: 220, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

func makeJPEGBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: 200, G: 80, B: 40, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 85}); err != nil {
		t.Fatalf("encode jpeg: %v", err)
	}
	return buf.Bytes()
}

func makeGIFBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewPaletted(image.Rect(0, 0, w, h), color.Palette{color.Black, color.White})
	var buf bytes.Buffer
	if err := gif.Encode(&buf, img, nil); err != nil {
		t.Fatalf("encode gif: %v", err)
	}
	return buf.Bytes()
}

func makeWAVBytes(sampleRate, channels, bitsPerSample, durationMs int) []byte {
	byteRate := sampleRate * channels * bitsPerSample / 8
	blockAlign := channels * bitsPerSample / 8
	dataSize := byteRate * durationMs / 1000
	if dataSize%blockAlign != 0 {
		dataSize += blockAlign - (dataSize % blockAlign)
	}

	var buf bytes.Buffer
	buf.WriteString("RIFF")
	_ = binary.Write(&buf, binary.LittleEndian, uint32(36+dataSize))
	buf.WriteString("WAVE")
	buf.WriteString("fmt ")
	_ = binary.Write(&buf, binary.LittleEndian, uint32(16))
	_ = binary.Write(&buf, binary.LittleEndian, uint16(1)) // PCM
	_ = binary.Write(&buf, binary.LittleEndian, uint16(channels))
	_ = binary.Write(&buf, binary.LittleEndian, uint32(sampleRate))
	_ = binary.Write(&buf, binary.LittleEndian, uint32(byteRate))
	_ = binary.Write(&buf, binary.LittleEndian, uint16(blockAlign))
	_ = binary.Write(&buf, binary.LittleEndian, uint16(bitsPerSample))
	buf.WriteString("data")
	_ = binary.Write(&buf, binary.LittleEndian, uint32(dataSize))
	buf.Write(make([]byte, dataSize))
	return buf.Bytes()
}

func makeOGGOpusBytes(durationMs int) []byte {
	var buf bytes.Buffer
	writePage := func(headerType byte, granule uint64, seq uint32, payload []byte) {
		buf.WriteString("OggS")
		buf.WriteByte(0)          // version
		buf.WriteByte(headerType) // flags
		_ = binary.Write(&buf, binary.LittleEndian, granule)
		_ = binary.Write(&buf, binary.LittleEndian, uint32(1)) // serial
		_ = binary.Write(&buf, binary.LittleEndian, seq)
		_ = binary.Write(&buf, binary.LittleEndian, uint32(0)) // crc (not validated by parser)
		buf.WriteByte(1)                                       // 1 segment
		buf.WriteByte(byte(len(payload)))
		buf.Write(payload)
	}

	opusHead := make([]byte, 19)
	copy(opusHead[0:8], "OpusHead")
	opusHead[8] = 1
	opusHead[9] = 1
	binary.LittleEndian.PutUint16(opusHead[10:12], 312) // pre-skip
	binary.LittleEndian.PutUint32(opusHead[12:16], 48000)

	writePage(0x02, 0, 0, opusHead)
	granule := uint64(312) + uint64(durationMs)*48
	writePage(0x04, granule, 1, []byte{0xF8, 0xFF, 0xFE})
	return buf.Bytes()
}

func makeMP3Bytes(frames int) []byte {
	// MPEG1 Layer III, 128 kbps (idx 9), 44100 Hz (idx 0), no padding:
	// frameLen = 144 * 128000 / 44100 = 417 bytes, 1152 samples (~26.122ms/frame).
	frameLen := 417
	frame := make([]byte, frameLen)
	frame[0] = 0xFF
	frame[1] = 0xFB // MPEG1, Layer III, no CRC
	frame[2] = 0x90 // 128kbps, 44100Hz, no padding
	frame[3] = 0x00

	var buf bytes.Buffer
	for i := 0; i < frames; i++ {
		buf.Write(frame)
	}
	return buf.Bytes()
}

func makeM4ABytes(timescale, duration uint32) []byte {
	var ftyp bytes.Buffer
	ftyp.Write([]byte{0, 0, 0, 20})
	ftyp.WriteString("ftypM4A ")
	ftyp.Write([]byte{0, 0, 0, 0})
	ftyp.WriteString("M4A ")

	mvhdPayload := make([]byte, 100)
	mvhdPayload[0] = 0 // version 0
	binary.BigEndian.PutUint32(mvhdPayload[12:16], timescale)
	binary.BigEndian.PutUint32(mvhdPayload[16:20], duration)

	var mvhd bytes.Buffer
	_ = binary.Write(&mvhd, binary.BigEndian, uint32(8+len(mvhdPayload)))
	mvhd.WriteString("mvhd")
	mvhd.Write(mvhdPayload)

	var moov bytes.Buffer
	_ = binary.Write(&moov, binary.BigEndian, uint32(8+mvhd.Len()))
	moov.WriteString("moov")
	moov.Write(mvhd.Bytes())

	var out bytes.Buffer
	out.Write(ftyp.Bytes())
	out.Write(moov.Bytes())
	return out.Bytes()
}

func makeWebMBytes(durationMs float64) []byte {
	// EBML Header + Segment(unknown size) + Info + TimestampScale(1ms) + Duration(float64)
	var info bytes.Buffer
	// TimestampScale 0x2AD7B1, size 3 (0x83), value 1000000 (0x0F4240)
	info.Write([]byte{0x2A, 0xD7, 0xB1, 0x83, 0x0F, 0x42, 0x40})
	// Duration 0x4489, size 8 (0x88), float64
	info.Write([]byte{0x44, 0x89, 0x88})
	_ = binary.Write(&info, binary.BigEndian, math.Float64bits(durationMs))

	var segment bytes.Buffer
	// Info ID 0x1549A966
	segment.Write([]byte{0x15, 0x49, 0xA9, 0x66})
	segment.WriteByte(byte(0x80 | info.Len()))
	segment.Write(info.Bytes())

	var out bytes.Buffer
	// EBML header ID 0x1A45DFA3, size 12, DocType "webm" (0x4282, len 4, "webm")
	ebmlBody := []byte{0x42, 0x82, 0x84, 'w', 'e', 'b', 'm'}
	out.Write([]byte{0x1A, 0x45, 0xDF, 0xA3, byte(0x80 | len(ebmlBody))})
	out.Write(ebmlBody)
	// Segment ID 0x18538067, unknown size 0x01FFFFFFFFFFFFFF
	out.Write([]byte{0x18, 0x53, 0x80, 0x67, 0x01, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF})
	out.Write(segment.Bytes())
	return out.Bytes()
}

func TestUploadServiceImageFileVoiceAndAuthLifecycle(t *testing.T) {
	ctx := context.Background()
	svc, store, rootDir := newTestUploadService(t)

	// 1. Image upload (PNG 400x200) -> proportional thumbnail (128x64), real files on disk, no storage_key leaked in JSON.
	pngData := makePNGBytes(t, 400, 200)
	imgDTO, bizErr := svc.Upload(ctx, UploadInput{
		OwnerID:      10001,
		Purpose:      model.AssetPurposeImage,
		RawFilename:  "../../evil/dir/photo.png",
		DeclaredMime: "image/png",
		TempPath:     writeStageTemp(t, svc, pngData),
		SizeBytes:    uint64(len(pngData)),
	})
	if bizErr != nil {
		t.Fatalf("upload image failed: %v", bizErr)
	}
	if imgDTO.Filename != "photo.png" || imgDTO.Mime != "image/png" || imgDTO.Width == nil || *imgDTO.Width != 400 || imgDTO.Height == nil || *imgDTO.Height != 200 {
		t.Fatalf("unexpected image dto: %+v", imgDTO)
	}
	if imgDTO.ThumbnailURL == nil || !strings.Contains(*imgDTO.ThumbnailURL, "variant=thumbnail") {
		t.Fatalf("expected thumbnail_url, got %+v", imgDTO.ThumbnailURL)
	}
	rawJSON, _ := json.Marshal(imgDTO)
	if strings.Contains(string(rawJSON), "storage_key") || strings.Contains(string(rawJSON), "thumbnail_key") || strings.Contains(string(rawJSON), rootDir) {
		t.Fatalf("internal path or storage key leaked in Asset JSON: %s", string(rawJSON))
	}

	// Verify temp dir is clean and physical files exist.
	storedAsset, _ := store.GetActiveByAssetID(ctx, imgDTO.AssetID)
	if _, err := os.Stat(filepath.Join(rootDir, filepath.FromSlash(storedAsset.StorageKey))); err != nil {
		t.Fatalf("expected original file on disk: %v", err)
	}
	if _, err := os.Stat(filepath.Join(rootDir, filepath.FromSlash(*storedAsset.ThumbnailKey))); err != nil {
		t.Fatalf("expected thumbnail file on disk: %v", err)
	}
	tmpEntries, _ := os.ReadDir(filepath.Join(rootDir, ".tmp"))
	if len(tmpEntries) != 0 {
		t.Fatalf("expected empty .tmp dir after upload, got %d entries", len(tmpEntries))
	}

	// Verify thumbnail decoded dimensions are 128x64.
	thumbHandle, thumbErr := svc.PrepareDownload(ctx, 10001, imgDTO.AssetID, DownloadOptions{Variant: "thumbnail"})
	if thumbErr != nil {
		t.Fatalf("PrepareDownload thumbnail: %v", thumbErr)
	}
	thumbImg, _, decErr := image.Decode(thumbHandle.File)
	_ = thumbHandle.File.Close()
	if decErr != nil {
		t.Fatalf("decode thumbnail: %v", decErr)
	}
	if thumbImg.Bounds().Dx() != 128 || thumbImg.Bounds().Dy() != 64 {
		t.Fatalf("expected 128x64 thumbnail, got %dx%d", thumbImg.Bounds().Dx(), thumbImg.Bounds().Dy())
	}

	// 2. Unreferenced asset: non-owner gets 16004 on GetAsset, PrepareDownload, and DeleteAsset.
	if _, err := svc.GetAsset(ctx, 10002, imgDTO.AssetID); err == nil || err.Code != errcode.ErrAssetAccessDenied.Code {
		t.Fatalf("expected 16004 for non-owner GetAsset, got %v", err)
	}
	if _, err := svc.PrepareDownload(ctx, 10002, imgDTO.AssetID, DownloadOptions{}); err == nil || err.Code != errcode.ErrAssetAccessDenied.Code {
		t.Fatalf("expected 16004 for non-owner PrepareDownload, got %v", err)
	}
	if _, err := svc.DeleteAsset(ctx, 10002, imgDTO.AssetID); err == nil || err.Code != errcode.ErrAssetAccessDenied.Code {
		t.Fatalf("expected 16004 for non-owner DeleteAsset, got %v", err)
	}

	// 3. Bind to message in conv 90001 (members: 10001, 10002; non-member: 10003).
	store.bindToMessage(imgDTO.AssetID, 777001, 90001)
	if _, err := svc.GetAsset(ctx, 10002, imgDTO.AssetID); err != nil {
		t.Fatalf("expected member 10002 to access bound asset, got %v", err)
	}
	dlHandle, dlErr := svc.PrepareDownload(ctx, 10002, imgDTO.AssetID, DownloadOptions{})
	if dlErr != nil {
		t.Fatalf("expected member 10002 download to succeed, got %v", dlErr)
	}
	gotBytes, _ := io.ReadAll(dlHandle.File)
	_ = dlHandle.File.Close()
	if !bytes.Equal(gotBytes, pngData) {
		t.Fatal("downloaded bytes mismatch original PNG")
	}
	if _, err := svc.GetAsset(ctx, 10003, imgDTO.AssetID); err == nil || err.Code != errcode.ErrAssetAccessDenied.Code {
		t.Fatalf("expected 16004 for non-member 10003, got %v", err)
	}

	// Owner cannot delete message-referenced asset -> 16006.
	if _, err := svc.DeleteAsset(ctx, 10001, imgDTO.AssetID); err == nil || err.Code != errcode.ErrAssetReferenced.Code {
		t.Fatalf("expected 16006 for referenced asset delete, got %v", err)
	}

	// 4. Unreferenced JPEG upload -> owner deletes -> disk files removed -> subsequent GetAsset returns 16005.
	jpgData := makeJPEGBytes(t, 64, 64)
	jpgDTO, bizErr := svc.Upload(ctx, UploadInput{
		OwnerID:      10001,
		Purpose:      model.AssetPurposeImage,
		RawFilename:  "avatar.jpg",
		DeclaredMime: "image/jpeg",
		TempPath:     writeStageTemp(t, svc, jpgData),
		SizeBytes:    uint64(len(jpgData)),
	})
	if bizErr != nil {
		t.Fatalf("upload jpeg failed: %v", bizErr)
	}
	jpgAsset, _ := store.GetActiveByAssetID(ctx, jpgDTO.AssetID)
	delRes, delErr := svc.DeleteAsset(ctx, 10001, jpgDTO.AssetID)
	if delErr != nil || !delRes.Deleted {
		t.Fatalf("delete unreferenced asset failed: res=%+v err=%v", delRes, delErr)
	}
	if _, err := os.Stat(filepath.Join(rootDir, filepath.FromSlash(jpgAsset.StorageKey))); !os.IsNotExist(err) {
		t.Fatalf("expected disk file removed after delete, stat err=%v", err)
	}
	if _, err := svc.GetAsset(ctx, 10001, jpgDTO.AssetID); err == nil || err.Code != errcode.ErrAssetNotFound.Code {
		t.Fatalf("expected 16005 after delete, got %v", err)
	}

	// 5. GIF upload + PDF file upload + Voice uploads (WAV, OGG, MP3, M4A, WebM).
	gifData := makeGIFBytes(t, 32, 32)
	if dto, err := svc.Upload(ctx, UploadInput{
		OwnerID:     10001,
		Purpose:     model.AssetPurposeImage,
		RawFilename: "anim.gif",
		TempPath:    writeStageTemp(t, svc, gifData),
		SizeBytes:   uint64(len(gifData)),
	}); err != nil || dto.Mime != "image/gif" {
		t.Fatalf("gif upload failed: dto=%+v err=%v", dto, err)
	}

	pdfData := []byte("%PDF-1.4\n1 0 obj\n<< /Type /Catalog >>\nendobj\ntrailer\n<< /Root 1 0 R >>\n%%EOF\n")
	pdfDTO, pdfErr := svc.Upload(ctx, UploadInput{
		OwnerID:     10001,
		Purpose:     model.AssetPurposeFile,
		RawFilename: "spec.pdf",
		TempPath:    writeStageTemp(t, svc, pdfData),
		SizeBytes:   uint64(len(pdfData)),
	})
	if pdfErr != nil || pdfDTO.Mime != "application/pdf" {
		t.Fatalf("pdf upload failed: dto=%+v err=%v", pdfDTO, pdfErr)
	}
	pdfDl, _ := svc.PrepareDownload(ctx, 10001, pdfDTO.AssetID, DownloadOptions{})
	_ = pdfDl.File.Close()
	if pdfDl.Disposition != "attachment" {
		t.Fatalf("expected attachment disposition for file purpose, got %q", pdfDl.Disposition)
	}

	voiceCases := []struct {
		name     string
		data     []byte
		wantMime string
		minMs    uint32
		maxMs    uint32
	}{
		{"wav", makeWAVBytes(16000, 1, 16, 1500), "audio/wav", 1450, 1550},
		{"ogg", makeOGGOpusBytes(2300), "audio/ogg", 2250, 2350},
		{"mp3", makeMP3Bytes(40), "audio/mpeg", 1000, 1100},
		{"m4a", makeM4ABytes(48000, 96000), "audio/mp4", 1950, 2050},
		{"webm", makeWebMBytes(1800), "audio/webm", 1750, 1850},
	}
	for _, vc := range voiceCases {
		dto, err := svc.Upload(ctx, UploadInput{
			OwnerID:     10001,
			Purpose:     model.AssetPurposeVoice,
			RawFilename: "clip." + vc.name,
			TempPath:    writeStageTemp(t, svc, vc.data),
			SizeBytes:   uint64(len(vc.data)),
		})
		if err != nil {
			t.Fatalf("voice %s upload failed: %v", vc.name, err)
		}
		if dto.Mime != vc.wantMime || dto.DurationMs == nil || *dto.DurationMs < vc.minMs || *dto.DurationMs > vc.maxMs {
			t.Fatalf("voice %s unexpected result: mime=%q duration=%v", vc.name, dto.Mime, dto.DurationMs)
		}
	}

	// 6. Avatar upload binds user_avatar and allows authenticated other user to read.
	avatarRes, avErr := svc.UploadAvatar(ctx, UploadInput{
		OwnerID:     10001,
		RawFilename: "me.png",
		TempPath:    writeStageTemp(t, svc, pngData),
		SizeBytes:   uint64(len(pngData)),
	})
	if avErr != nil {
		t.Fatalf("UploadAvatar failed: %v", avErr)
	}
	if avatarRes.User.AvatarURL != avatarRes.Asset.URL {
		t.Fatalf("expected user.avatar_url %q == asset.url %q", avatarRes.User.AvatarURL, avatarRes.Asset.URL)
	}
	if _, err := svc.GetAsset(ctx, 10003, avatarRes.Asset.AssetID); err != nil {
		t.Fatalf("expected authenticated user to read bound avatar, got %v", err)
	}
}

func TestUploadServiceRejections(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newTestUploadService(t)

	// Disallowed MIME for purpose=image (16001)
	txtData := []byte("plain text note")
	if _, err := svc.Upload(ctx, UploadInput{
		OwnerID:      10001,
		Purpose:      model.AssetPurposeImage,
		RawFilename:  "note.txt",
		DeclaredMime: "text/plain",
		TempPath:     writeStageTemp(t, svc, txtData),
		SizeBytes:    uint64(len(txtData)),
	}); err == nil || err.Code != errcode.ErrUploadInvalidFile.Code {
		t.Fatalf("expected 16001 for text file with purpose=image, got %v", err)
	}

	// Disallowed dangerous MIME for purpose=file (HTML / script -> 16001)
	htmlData := []byte("<!DOCTYPE html><html><body><script>alert(1)</script></body></html>")
	if _, err := svc.Upload(ctx, UploadInput{
		OwnerID:      10001,
		Purpose:      model.AssetPurposeFile,
		RawFilename:  "page.html",
		DeclaredMime: "text/html",
		TempPath:     writeStageTemp(t, svc, htmlData),
		SizeBytes:    uint64(len(htmlData)),
	}); err == nil || err.Code != errcode.ErrUploadInvalidFile.Code {
		t.Fatalf("expected 16001 for html upload, got %v", err)
	}

	// Type mismatch: text disguised as .png (16003)
	if _, err := svc.Upload(ctx, UploadInput{
		OwnerID:      10001,
		Purpose:      model.AssetPurposeImage,
		RawFilename:  "fake.png",
		DeclaredMime: "image/png",
		TempPath:     writeStageTemp(t, svc, txtData),
		SizeBytes:    uint64(len(txtData)),
	}); err == nil || err.Code != errcode.ErrUploadTypeMismatch.Code {
		t.Fatalf("expected 16003 for fake.png with text content, got %v", err)
	}

	// Type mismatch: JPEG bytes renamed to .png (16003)
	jpgData := makeJPEGBytes(t, 64, 64)
	if _, err := svc.Upload(ctx, UploadInput{
		OwnerID:      10001,
		Purpose:      model.AssetPurposeImage,
		RawFilename:  "wrong.png",
		DeclaredMime: "image/jpeg",
		TempPath:     writeStageTemp(t, svc, jpgData),
		SizeBytes:    uint64(len(jpgData)),
	}); err == nil || err.Code != errcode.ErrUploadTypeMismatch.Code {
		t.Fatalf("expected 16003 for jpeg bytes with .png extension, got %v", err)
	}

	// Type mismatch: corrupt PNG magic bytes (16003)
	corruptPNG := []byte("\x89PNG\r\n\x1a\nnot-a-valid-ihdr-chunk")
	if _, err := svc.Upload(ctx, UploadInput{
		OwnerID:     10001,
		Purpose:     model.AssetPurposeImage,
		RawFilename: "corrupt.png",
		TempPath:    writeStageTemp(t, svc, corruptPNG),
		SizeBytes:   uint64(len(corruptPNG)),
	}); err == nil || err.Code != errcode.ErrUploadTypeMismatch.Code {
		t.Fatalf("expected 16003 for corrupt png, got %v", err)
	}

	// Pixel limit exceeded (1200x1000 = 1.2M > 1M max_image_pixels -> 16002)
	bigPixelsPNG := makePNGBytes(t, 1200, 1000)
	if _, err := svc.Upload(ctx, UploadInput{
		OwnerID:     10001,
		Purpose:     model.AssetPurposeImage,
		RawFilename: "huge_pixels.png",
		TempPath:    writeStageTemp(t, svc, bigPixelsPNG),
		SizeBytes:   uint64(len(bigPixelsPNG)),
	}); err == nil || err.Code != errcode.ErrUploadTooLarge.Code {
		t.Fatalf("expected 16002 for exceeding pixel limit, got %v", err)
	}

	// Voice duration limit exceeded (12000ms > 10000ms -> 16002)
	longWAV := makeWAVBytes(8000, 1, 8, 12000)
	if _, err := svc.Upload(ctx, UploadInput{
		OwnerID:     10001,
		Purpose:     model.AssetPurposeVoice,
		RawFilename: "long.wav",
		TempPath:    writeStageTemp(t, svc, longWAV),
		SizeBytes:   uint64(len(longWAV)),
	}); err == nil || err.Code != errcode.ErrUploadTooLarge.Code {
		t.Fatalf("expected 16002 for exceeding voice duration limit, got %v", err)
	}

	// Signed download URL validation: valid vs tampered/expired signature
	pngData := makePNGBytes(t, 32, 32)
	dto, err := svc.Upload(ctx, UploadInput{
		OwnerID:     10001,
		Purpose:     model.AssetPurposeImage,
		RawFilename: "signed.png",
		TempPath:    writeStageTemp(t, svc, pngData),
		SizeBytes:   uint64(len(pngData)),
	})
	if err != nil {
		t.Fatalf("upload signed.png: %v", err)
	}
	parts := strings.SplitN(dto.DownloadURL, "?", 2)
	if len(parts) != 2 {
		t.Fatalf("expected signed query on download_url, got %q", dto.DownloadURL)
	}
	if _, err := svc.PrepareDownload(ctx, 10001, dto.AssetID, DownloadOptions{
		ExpiresParam:   "1",
		SignatureParam: "deadbeef",
	}); err == nil || err.Code != errcode.ErrAssetAccessDenied.Code {
		t.Fatalf("expected 16004 for expired/invalid signed download_url, got %v", err)
	}

	// Storage path traversal rejection
	if _, pathErr := svc.storage.ResolveSafeKey("../outside.png"); !errors.Is(pathErr, ErrStorageInvalidPath) {
		t.Fatalf("expected ErrStorageInvalidPath, got %v", pathErr)
	}
	if _, pathErr := svc.storage.ResolveSafeKey("/etc/passwd"); !errors.Is(pathErr, ErrStorageInvalidPath) {
		t.Fatalf("expected ErrStorageInvalidPath for absolute path, got %v", pathErr)
	}
}
