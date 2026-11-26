package repository

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"

	"enterprise-im/backend/internal/config"
	"enterprise-im/backend/internal/model"
	"enterprise-im/backend/internal/pkg/logger"
)

func TestAssetRepositoryGuards(t *testing.T) {
	ctx := context.Background()
	nilRepo := NewAssetRepository(nil)

	if err := nilRepo.Create(ctx, &model.Asset{AssetID: uuid.NewString(), OwnerID: 1}); err == nil {
		t.Fatal("expected Create on nil db to fail")
	}
	if _, err := nilRepo.CreateAvatarAndBindUser(ctx, &model.Asset{AssetID: uuid.NewString()}, 1, "/api/v1/uploads/x/download", time.Now()); err == nil {
		t.Fatal("expected CreateAvatarAndBindUser on nil db to fail")
	}
	if _, err := nilRepo.GetActiveByAssetID(ctx, uuid.NewString()); err == nil {
		t.Fatal("expected GetActiveByAssetID on nil db to fail")
	}
	if _, err := nilRepo.IsConversationMember(ctx, 1, 1); err == nil {
		t.Fatal("expected IsConversationMember on nil db to fail")
	}
	if _, err := nilRepo.SoftDeleteOwned(ctx, uuid.NewString(), 1, time.Now()); err == nil {
		t.Fatal("expected SoftDeleteOwned on nil db to fail")
	}
}

func TestLiveAssetRepositoryIntegration(t *testing.T) {
	mysqlPass := os.Getenv("MYSQL_PASSWORD")
	if mysqlPass == "" {
		mysqlPass = os.Getenv("IM_MYSQL_PASSWORD")
	}
	if mysqlPass == "" {
		t.Skip("skipping live asset repository integration test: MYSQL_PASSWORD not set")
	}

	host := envOrDefault("MYSQL_HOST", "127.0.0.1")
	port := 3306
	if p := os.Getenv("MYSQL_PORT"); p != "" {
		if parsed, err := strconv.Atoi(p); err == nil {
			port = parsed
		}
	}
	user := envOrDefault("MYSQL_USER", "root")
	dbName := envOrDefault("MYSQL_DATABASE", "enterprise_im")

	log, err := logger.New(config.LogConfig{Level: "warn", Format: "json"}, nil)
	if err != nil {
		t.Fatalf("new logger: %v", err)
	}
	db, err := NewMySQL(config.MySQLConfig{
		Host:            host,
		Port:            port,
		User:            user,
		Password:        mysqlPass,
		Database:        dbName,
		Charset:         "utf8mb4",
		MaxOpenConns:    5,
		MaxIdleConns:    2,
		ConnMaxLifetime: 5 * time.Minute,
		ConnMaxIdleTime: 2 * time.Minute,
		DialTimeout:     5 * time.Second,
	}, log)
	if err != nil {
		t.Fatalf("connect mysql: %v", err)
	}
	defer func() { _ = CloseMySQL(db) }()

	ctx := context.Background()
	repo := NewAssetRepository(db)

	suffix := uuid.NewString()[:8]
	u1 := model.User{
		Username:     "asset_u1_" + suffix,
		PasswordHash: "$2a$10$8uLi1z7yKc9N28SrYAXW1uRurpmO9uk66JQjVxSE1Z8bSAtcbeE82",
		Nickname:     "AssetU1",
		Role:         model.UserRoleUser,
		Status:       model.UserStatusActive,
	}
	u2 := model.User{
		Username:     "asset_u2_" + suffix,
		PasswordHash: "$2a$10$8uLi1z7yKc9N28SrYAXW1uRurpmO9uk66JQjVxSE1Z8bSAtcbeE82",
		Nickname:     "AssetU2",
		Role:         model.UserRoleUser,
		Status:       model.UserStatusActive,
	}
	if err := db.Create(&u1).Error; err != nil {
		t.Fatalf("create u1: %v", err)
	}
	if err := db.Create(&u2).Error; err != nil {
		t.Fatalf("create u2: %v", err)
	}
	defer func() {
		_ = db.Where("owner_id IN ?", []uint64{u1.ID, u2.ID}).Delete(&model.Asset{}).Error
		_ = db.Where("id IN ?", []uint64{u1.ID, u2.ID}).Delete(&model.User{}).Error
	}()

	assetID1 := uuid.NewString()
	w, h := uint32(100), uint32(80)
	tk := fmt.Sprintf("thumbnails/2026/11/26/%s_thumb.png", assetID1)
	tm := "image/png"
	tsize := uint64(512)
	a1 := &model.Asset{
		AssetID:            assetID1,
		OwnerID:            u1.ID,
		Purpose:            model.AssetPurposeImage,
		Filename:           "photo.png",
		Mime:               "image/png",
		SizeBytes:          1024,
		SHA256:             "0000000000000000000000000000000000000000000000000000000000000000",
		Width:              &w,
		Height:             &h,
		StorageKey:         fmt.Sprintf("image/2026/11/26/%s.png", assetID1),
		ThumbnailKey:       &tk,
		ThumbnailMime:      &tm,
		ThumbnailSizeBytes: &tsize,
		RefType:            model.AssetRefNone,
		Status:             model.AssetStatusActive,
	}
	if err := repo.Create(ctx, a1); err != nil {
		t.Fatalf("Create asset: %v", err)
	}

	got, err := repo.GetActiveByAssetID(ctx, assetID1)
	if err != nil {
		t.Fatalf("GetActiveByAssetID: %v", err)
	}
	if got.OwnerID != u1.ID || got.Filename != "photo.png" {
		t.Fatalf("unexpected fetched asset: %+v", got)
	}

	// Non-owner cannot delete
	if _, err := repo.SoftDeleteOwned(ctx, assetID1, u2.ID, time.Now()); !errors.Is(err, ErrAssetForbidden) {
		t.Fatalf("expected ErrAssetForbidden for non-owner, got %v", err)
	}

	// Owner can soft delete unreferenced asset
	if _, err := repo.SoftDeleteOwned(ctx, assetID1, u1.ID, time.Now()); err != nil {
		t.Fatalf("SoftDeleteOwned: %v", err)
	}
	if _, err := repo.GetActiveByAssetID(ctx, assetID1); !errors.Is(err, ErrAssetNotFound) {
		t.Fatalf("expected ErrAssetNotFound after soft delete, got %v", err)
	}

	// Message-referenced asset cannot be deleted
	assetID2 := uuid.NewString()
	refID := uint64(888001)
	convID := uint64(999001)
	now := time.Now().UTC()
	a2 := &model.Asset{
		AssetID:           assetID2,
		OwnerID:           u1.ID,
		Purpose:           model.AssetPurposeFile,
		Filename:          "doc.pdf",
		Mime:              "application/pdf",
		SizeBytes:         2048,
		SHA256:            "1111111111111111111111111111111111111111111111111111111111111111",
		StorageKey:        fmt.Sprintf("file/2026/11/26/%s.pdf", assetID2),
		RefType:           model.AssetRefMessage,
		RefID:             &refID,
		RefConversationID: &convID,
		RefCount:          1,
		BoundAt:           &now,
		Status:            model.AssetStatusActive,
	}
	if err := repo.Create(ctx, a2); err != nil {
		t.Fatalf("Create referenced asset: %v", err)
	}
	if _, err := repo.SoftDeleteOwned(ctx, assetID2, u1.ID, time.Now()); !errors.Is(err, ErrAssetReferencedByMessage) {
		t.Fatalf("expected ErrAssetReferencedByMessage, got %v", err)
	}

	// Avatar upload binds user and updates avatar_url
	avatarID := uuid.NewString()
	avatarURL := fmt.Sprintf("/api/v1/uploads/%s/download", avatarID)
	avatarAsset := &model.Asset{
		AssetID:    avatarID,
		Purpose:    model.AssetPurposeAvatar,
		Filename:   "avatar.png",
		Mime:       "image/png",
		SizeBytes:  512,
		SHA256:     "2222222222222222222222222222222222222222222222222222222222222222",
		Width:      &w,
		Height:     &h,
		StorageKey: fmt.Sprintf("avatar/2026/11/26/%s.png", avatarID),
	}
	updatedUser, err := repo.CreateAvatarAndBindUser(ctx, avatarAsset, u1.ID, avatarURL, now)
	if err != nil {
		t.Fatalf("CreateAvatarAndBindUser: %v", err)
	}
	if updatedUser.AvatarURL != avatarURL {
		t.Fatalf("expected avatar_url %q, got %q", avatarURL, updatedUser.AvatarURL)
	}
}
