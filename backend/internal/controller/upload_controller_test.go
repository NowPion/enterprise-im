package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"enterprise-im/backend/internal/config"
	"enterprise-im/backend/internal/model"
	"enterprise-im/backend/internal/pkg/errcode"
	"enterprise-im/backend/internal/pkg/logger"
	"enterprise-im/backend/internal/repository"
	"enterprise-im/backend/internal/service"
)

type ctrlMemAssetStore struct {
	mu      sync.Mutex
	nextID  uint64
	assets  map[string]model.Asset
	users   map[uint64]model.User
	members map[[2]uint64]bool
}

func newCtrlMemAssetStore() *ctrlMemAssetStore {
	return &ctrlMemAssetStore{
		assets: make(map[string]model.Asset),
		users: map[uint64]model.User{
			10001: {ID: 10001, Username: "alice", Nickname: "Alice", Role: model.UserRoleUser, Status: model.UserStatusActive},
		},
		members: make(map[[2]uint64]bool),
	}
}

func (m *ctrlMemAssetStore) Create(_ context.Context, asset *model.Asset) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextID++
	cp := *asset
	cp.ID = m.nextID
	m.assets[cp.AssetID] = cp
	return nil
}

func (m *ctrlMemAssetStore) CreateAvatarAndBindUser(_ context.Context, asset *model.Asset, userID uint64, avatarURL string, boundAt time.Time) (*model.User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	u := m.users[userID]
	u.AvatarURL = avatarURL
	m.users[userID] = u
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
	cpUser := u
	return &cpUser, nil
}

func (m *ctrlMemAssetStore) GetActiveByAssetID(_ context.Context, assetID string) (*model.Asset, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.assets[assetID]
	if !ok || a.Status != model.AssetStatusActive {
		return nil, repository.ErrAssetNotFound
	}
	cp := a
	return &cp, nil
}

func (m *ctrlMemAssetStore) IsConversationMember(_ context.Context, conversationID, userID uint64) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.members[[2]uint64{conversationID, userID}], nil
}

func (m *ctrlMemAssetStore) SoftDeleteOwned(_ context.Context, assetID string, ownerID uint64, deletedAt time.Time) (*model.Asset, error) {
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
	m.assets[assetID] = a
	cp := a
	return &cp, nil
}

func buildMultipartUpload(t *testing.T, purposeFirst bool, purpose, fieldName, filename, partContentType string, payload []byte) (*bytes.Buffer, string) {
	t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)

	writePurpose := func() {
		if purpose != "" {
			if err := w.WriteField("purpose", purpose); err != nil {
				t.Fatalf("write purpose field: %v", err)
			}
		}
	}
	writeFile := func() {
		if fieldName == "" {
			return
		}
		h := make(textproto.MIMEHeader)
		h.Set("Content-Disposition", `form-data; name="`+fieldName+`"; filename="`+filename+`"`)
		if partContentType != "" {
			h.Set("Content-Type", partContentType)
		}
		part, err := w.CreatePart(h)
		if err != nil {
			t.Fatalf("create multipart file part: %v", err)
		}
		if _, err := part.Write(payload); err != nil {
			t.Fatalf("write multipart file payload: %v", err)
		}
	}

	if purposeFirst {
		writePurpose()
		writeFile()
	} else {
		writeFile()
		writePurpose()
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}
	return &body, w.FormDataContentType()
}

func makeCtrlPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 64, 48))
	img.Set(1, 1, color.RGBA{R: 255, A: 255})
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return b.Bytes()
}

func TestUploadControllerEndpoints(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rootDir := filepath.Join(t.TempDir(), "uploads")
	storage, err := service.NewDiskStorage(rootDir, "")
	if err != nil {
		t.Fatalf("NewDiskStorage: %v", err)
	}
	log, _ := logger.New(config.LogConfig{Level: "error", Format: "json"}, nil)
	svc := service.NewUploadService(newCtrlMemAssetStore(), nil, storage, config.UploadConfig{
		Dir:                rootDir,
		MaxImageBytes:      1024 * 1024,
		MaxFileBytes:       2 * 1024 * 1024,
		MaxVoiceBytes:      1024 * 1024,
		MaxAvatarBytes:     1024, // 1 KiB cap to test 16002 easily
		MaxImageWidth:      4096,
		MaxImageHeight:     4096,
		MaxImagePixels:     4000000,
		MaxVoiceDurationMs: 60000,
		ThumbnailMaxDim:    128,
		DownloadURLTTL:     10 * time.Minute,
	}, "controller-test-secret", log)
	ctrl := NewUploadController(svc)

	r := gin.New()
	r.Use(func(c *gin.Context) {
		uid := c.GetHeader("X-User-ID")
		if uid != "" {
			errcode.SetUserID(c, uid)
		}
		c.Next()
	})
	r.POST("/api/v1/uploads", ctrl.Upload)
	r.GET("/api/v1/uploads/:asset_id", ctrl.GetAsset)
	r.GET("/api/v1/uploads/:asset_id/download", ctrl.DownloadAsset)
	r.DELETE("/api/v1/uploads/:asset_id", ctrl.DeleteAsset)
	r.POST("/api/v1/me/avatar", ctrl.UploadMyAvatar)

	pngBytes := makeCtrlPNG(t)

	// 1. File part arrives BEFORE purpose part, UTF-8 filename -> 201 Created, sanitized UTF-8 Content-Disposition on download.
	body, ct := buildMultipartUpload(t, false, "image", "file", "../../dir/测试图片.png", "image/png", pngBytes)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/v1/uploads", body)
	req.Header.Set("Content-Type", ct)
	req.Header.Set("X-User-ID", "10001")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d body=%s", w.Code, w.Body.String())
	}

	var uploadEnv struct {
		Code int              `json:"code"`
		Data service.AssetDTO `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &uploadEnv); err != nil {
		t.Fatalf("unmarshal upload response: %v", err)
	}
	if uploadEnv.Code != 0 || uploadEnv.Data.Filename != "测试图片.png" {
		t.Fatalf("unexpected upload response: %+v", uploadEnv)
	}

	// Download via returned download_url
	wDl := httptest.NewRecorder()
	reqDl, _ := http.NewRequest(http.MethodGet, uploadEnv.Data.DownloadURL, nil)
	reqDl.Header.Set("X-User-ID", "10001")
	r.ServeHTTP(wDl, reqDl)
	if wDl.Code != http.StatusOK {
		t.Fatalf("download expected 200, got %d body=%s", wDl.Code, wDl.Body.String())
	}
	if !bytes.Equal(wDl.Body.Bytes(), pngBytes) {
		t.Fatal("downloaded bytes mismatch")
	}
	cd := wDl.Header().Get("Content-Disposition")
	if !strings.Contains(cd, "filename*=UTF-8''") || strings.Contains(cd, "..") || wDl.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("unexpected download headers: Content-Disposition=%q nosniff=%q", cd, wDl.Header().Get("X-Content-Type-Options"))
	}

	// 2. Missing file field -> 16001
	missBody, missCT := buildMultipartUpload(t, true, "image", "", "", "", nil)
	wMiss := httptest.NewRecorder()
	reqMiss, _ := http.NewRequest(http.MethodPost, "/api/v1/uploads", missBody)
	reqMiss.Header.Set("Content-Type", missCT)
	reqMiss.Header.Set("X-User-ID", "10001")
	r.ServeHTTP(wMiss, reqMiss)
	if wMiss.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing file, got %d body=%s", wMiss.Code, wMiss.Body.String())
	}
	var missEnv errcode.Envelope
	_ = json.Unmarshal(wMiss.Body.Bytes(), &missEnv)
	if missEnv.Code != errcode.ErrUploadInvalidFile.Code {
		t.Fatalf("expected 16001, got %d", missEnv.Code)
	}

	// 3. Avatar exceeding MaxAvatarBytes (1024B) -> 16002
	bigAvatar := make([]byte, 2048)
	copy(bigAvatar, pngBytes)
	avBody, avCT := buildMultipartUpload(t, true, "", "file", "avatar.png", "image/png", bigAvatar)
	wAv := httptest.NewRecorder()
	reqAv, _ := http.NewRequest(http.MethodPost, "/api/v1/me/avatar", avBody)
	reqAv.Header.Set("Content-Type", avCT)
	reqAv.Header.Set("X-User-ID", "10001")
	r.ServeHTTP(wAv, reqAv)
	if wAv.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413 for oversized avatar, got %d body=%s", wAv.Code, wAv.Body.String())
	}
	var avEnv errcode.Envelope
	_ = json.Unmarshal(wAv.Body.Bytes(), &avEnv)
	if avEnv.Code != errcode.ErrUploadTooLarge.Code {
		t.Fatalf("expected 16002, got %d", avEnv.Code)
	}
}
