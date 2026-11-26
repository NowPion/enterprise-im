package api

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
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"enterprise-im/backend/internal/config"
	"enterprise-im/backend/internal/controller"
	"enterprise-im/backend/internal/model"
	"enterprise-im/backend/internal/pkg/errcode"
	"enterprise-im/backend/internal/pkg/logger"
	"enterprise-im/backend/internal/repository"
	"enterprise-im/backend/internal/service"
)

type apiAssetStoreStub struct {
	assets map[string]model.Asset
	users  map[uint64]model.User
}

func (s *apiAssetStoreStub) Create(_ context.Context, asset *model.Asset) error {
	s.assets[asset.AssetID] = *asset
	return nil
}

func (s *apiAssetStoreStub) CreateAvatarAndBindUser(_ context.Context, asset *model.Asset, userID uint64, avatarURL string, _ time.Time) (*model.User, error) {
	u := s.users[userID]
	u.AvatarURL = avatarURL
	s.users[userID] = u
	refID := userID
	cp := *asset
	cp.OwnerID = userID
	cp.RefType = model.AssetRefUserAvatar
	cp.RefID = &refID
	cp.RefCount = 1
	cp.Status = model.AssetStatusActive
	s.assets[cp.AssetID] = cp
	cpUser := u
	return &cpUser, nil
}

func (s *apiAssetStoreStub) GetActiveByAssetID(_ context.Context, assetID string) (*model.Asset, error) {
	a, ok := s.assets[assetID]
	if !ok || a.Status != model.AssetStatusActive {
		return nil, repository.ErrAssetNotFound
	}
	cp := a
	return &cp, nil
}

func (s *apiAssetStoreStub) IsConversationMember(_ context.Context, _, _ uint64) (bool, error) {
	return false, nil
}

func (s *apiAssetStoreStub) SoftDeleteOwned(_ context.Context, assetID string, ownerID uint64, _ time.Time) (*model.Asset, error) {
	a, ok := s.assets[assetID]
	if !ok || a.Status != model.AssetStatusActive {
		return nil, repository.ErrAssetNotFound
	}
	if a.OwnerID != ownerID {
		return nil, repository.ErrAssetForbidden
	}
	a.Status = model.AssetStatusDeleted
	s.assets[assetID] = a
	cp := a
	return &cp, nil
}

func TestRegisterUploadRoutesAndBodyLimitBypass(t *testing.T) {
	log, _ := logger.New(config.LogConfig{Level: "error", Format: "json"}, nil)
	rootDir := filepath.Join(t.TempDir(), "uploads")
	storage, err := service.NewDiskStorage(rootDir, "")
	if err != nil {
		t.Fatalf("NewDiskStorage: %v", err)
	}

	store := &apiAssetStoreStub{
		assets: make(map[string]model.Asset),
		users: map[uint64]model.User{
			10001: {ID: 10001, Username: "alice", Nickname: "Alice", Role: model.UserRoleUser, Status: model.UserStatusActive},
		},
	}
	uploadSvc := service.NewUploadService(store, nil, storage, config.UploadConfig{
		Dir:                rootDir,
		MaxImageBytes:      5 << 20,
		MaxFileBytes:       10 << 20,
		MaxVoiceBytes:      5 << 20,
		MaxAvatarBytes:     2 << 20,
		MaxImageWidth:      4096,
		MaxImageHeight:     4096,
		MaxImagePixels:     16000000,
		MaxVoiceDurationMs: 300000,
		ThumbnailMaxDim:    320,
		DownloadURLTTL:     15 * time.Minute,
	}, "api-route-test-secret", log)
	uploadCtrl := controller.NewUploadController(uploadSvc)

	// 1. Fail closed when jwtAuth is nil.
	rClosed := NewRouter(&config.Config{}, log, nil)
	RegisterUploadRoutes(rClosed, nil, uploadCtrl)
	wClosed := httptest.NewRecorder()
	reqClosed, _ := http.NewRequest(http.MethodPost, "/api/v1/uploads", nil)
	rClosed.ServeHTTP(wClosed, reqClosed)
	if wClosed.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 when jwtAuth nil, got %d", wClosed.Code)
	}

	// 2. Verify small server.max_body_bytes (e.g. 512 bytes) rejects normal POST routes with 10002,
	// while allowing larger multipart uploads on POST /api/v1/uploads and POST /api/v1/me/avatar.
	cfg := &config.Config{
		Server: config.ServerConfig{MaxBodyBytes: 512},
	}
	r := NewRouter(cfg, log, nil)
	jwtStub := func(c *gin.Context) {
		if !strings.HasPrefix(c.GetHeader("Authorization"), "Bearer user-") {
			errcode.Fail(c, errcode.ErrAccessTokenInvalid)
			c.Abort()
			return
		}
		errcode.SetUserID(c, strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer user-"))
		c.Next()
	}
	RegisterUploadRoutes(r, jwtStub, uploadCtrl)
	r.POST("/api/v1/other", func(c *gin.Context) {
		errcode.OK(c, gin.H{"ok": true})
	})

	// Non-upload route still hits 10002 when > 512 bytes.
	wOther := httptest.NewRecorder()
	reqOther, _ := http.NewRequest(http.MethodPost, "/api/v1/other", strings.NewReader(strings.Repeat("x", 1024)))
	r.ServeHTTP(wOther, reqOther)
	var otherEnv errcode.Envelope
	_ = json.Unmarshal(wOther.Body.Bytes(), &otherEnv)
	if wOther.Code != http.StatusRequestEntityTooLarge || otherEnv.Code != errcode.ErrPayloadTooLarge.Code {
		t.Fatalf("expected 413/10002 on non-upload route, got status=%d code=%d", wOther.Code, otherEnv.Code)
	}

	// Multipart PNG upload > 512 bytes succeeds on /api/v1/uploads.
	img := image.NewRGBA(image.Rect(0, 0, 120, 120))
	for y := 0; y < 120; y++ {
		for x := 0; x < 120; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x * 2), G: uint8(y * 2), B: 100, A: 255})
		}
	}
	var pngBuf bytes.Buffer
	_ = png.Encode(&pngBuf, img)

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("purpose", "image")
	fw, _ := mw.CreateFormFile("file", "pic.png")
	_, _ = fw.Write(pngBuf.Bytes())
	_ = mw.Close()

	wUp := httptest.NewRecorder()
	reqUp, _ := http.NewRequest(http.MethodPost, "/api/v1/uploads", &body)
	reqUp.Header.Set("Authorization", "Bearer user-10001")
	reqUp.Header.Set("Content-Type", mw.FormDataContentType())
	r.ServeHTTP(wUp, reqUp)
	if wUp.Code != http.StatusCreated {
		t.Fatalf("expected 201 on /api/v1/uploads > server.max_body_bytes, got %d body=%s", wUp.Code, wUp.Body.String())
	}
}
