package service

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"enterprise-im/backend/internal/config"
	"enterprise-im/backend/internal/model"
	"enterprise-im/backend/internal/pkg/errcode"
	"enterprise-im/backend/internal/repository"
)

// AssetStore defines the persistence operations required by UploadService.
type AssetStore interface {
	Create(ctx context.Context, asset *model.Asset) error
	CreateAvatarAndBindUser(ctx context.Context, asset *model.Asset, userID uint64, avatarURL string, boundAt time.Time) (*model.User, error)
	GetActiveByAssetID(ctx context.Context, assetID string) (*model.Asset, error)
	IsConversationMember(ctx context.Context, conversationID, userID uint64) (bool, error)
	SoftDeleteOwned(ctx context.Context, assetID string, ownerID uint64, deletedAt time.Time) (*model.Asset, error)
}

// AssetDTO matches the frozen public `Asset` object in docs/api.md.
// Internal storage keys and SHA-256 digests are never exposed.
type AssetDTO struct {
	AssetID      string  `json:"asset_id"`
	Purpose      string  `json:"purpose"`
	URL          string  `json:"url"`
	DownloadURL  string  `json:"download_url"`
	Filename     string  `json:"filename"`
	Mime         string  `json:"mime"`
	SizeBytes    uint64  `json:"size_bytes"`
	Width        *uint32 `json:"width,omitempty"`
	Height       *uint32 `json:"height,omitempty"`
	ThumbnailURL *string `json:"thumbnail_url,omitempty"`
	DurationMs   *uint32 `json:"duration_ms,omitempty"`
}

// UploadInput encapsulates a staged temporary file upload request.
type UploadInput struct {
	OwnerID      uint64
	Purpose      string
	RawFilename  string
	DeclaredMime string
	TempPath     string
	SizeBytes    uint64
	SHA256       string
}

// UploadAvatarResult matches POST /api/v1/me/avatar response `data`.
type UploadAvatarResult struct {
	User  SelfUserProfile `json:"user"`
	Asset AssetDTO        `json:"asset"`
}

// DeleteAssetResult matches DELETE /api/v1/uploads/{asset_id} response `data`.
type DeleteAssetResult struct {
	AssetID string `json:"asset_id"`
	Deleted bool   `json:"deleted"`
}

// DownloadOptions controls download variant selection and optional signed URL validation.
type DownloadOptions struct {
	Variant        string
	ForceAttach    bool
	ExpiresParam   string
	SignatureParam string
}

// DownloadHandle holds an authorized open file stream and safe HTTP response metadata.
type DownloadHandle struct {
	File        *os.File
	Filename    string
	Mime        string
	SizeBytes   int64
	ModTime     time.Time
	Disposition string
}

// UploadService implements real disk upload validation, thumbnailing, authorized downloads,
// asset deletion with message-reference checks, and user avatar uploads.
type UploadService struct {
	assets     AssetStore
	presence   PresenceStore
	storage    *DiskStorage
	cfg        config.UploadConfig
	signingKey []byte
	log        *slog.Logger
	now        func() time.Time
}

// NewUploadService creates an UploadService backed by AssetStore and DiskStorage.
func NewUploadService(
	assets AssetStore,
	presence PresenceStore,
	storage *DiskStorage,
	cfg config.UploadConfig,
	signingSecret string,
	log *slog.Logger,
) *UploadService {
	if log == nil {
		log = slog.Default()
	}
	trimmedSecret := strings.TrimSpace(signingSecret)
	var key []byte
	if trimmedSecret != "" {
		sum := sha256.Sum256([]byte("enterprise-im:upload-download:" + trimmedSecret))
		key = sum[:]
	}
	return &UploadService{
		assets:     assets,
		presence:   presence,
		storage:    storage,
		cfg:        cfg,
		signingKey: key,
		log:        log,
		now:        func() time.Time { return time.Now().UTC() },
	}
}

// CreateTempFile allocates a staging file inside the upload temp directory.
func (s *UploadService) CreateTempFile(prefix string) (*os.File, string, error) {
	if s == nil || s.storage == nil {
		return nil, "", errors.New("upload storage unavailable")
	}
	return s.storage.CreateTemp(prefix)
}

// UploadConfig returns the active UploadConfig limits.
func (s *UploadService) UploadConfig() config.UploadConfig {
	if s == nil {
		return config.UploadConfig{}
	}
	return s.cfg
}

// MaxBytesForPurpose returns the byte cap configured for the given upload purpose.
func (s *UploadService) MaxBytesForPurpose(purpose string) (int64, *errcode.Error) {
	switch purpose {
	case model.AssetPurposeImage:
		return s.cfg.MaxImageBytes, nil
	case model.AssetPurposeFile:
		return s.cfg.MaxFileBytes, nil
	case model.AssetPurposeVoice:
		return s.cfg.MaxVoiceBytes, nil
	case model.AssetPurposeAvatar, model.AssetPurposeGroupAvatar:
		return s.cfg.MaxAvatarBytes, nil
	default:
		return 0, errcode.ErrInvalidValue.WithMessage("purpose must be image, file, voice, avatar, or group_avatar")
	}
}

// Upload validates the staged temp file, generates thumbnails/metadata, atomically promotes
// files into the configured upload volume, and persists the Asset row.
func (s *UploadService) Upload(ctx context.Context, input UploadInput) (AssetDTO, *errcode.Error) {
	if input.TempPath != "" {
		defer func() { _ = os.Remove(input.TempPath) }()
	}
	if input.OwnerID == 0 {
		return AssetDTO{}, errcode.ErrAccessTokenInvalid
	}
	if s == nil || s.assets == nil || s.storage == nil {
		return AssetDTO{}, errcode.ErrInternalConsistency
	}

	prepared, thumbTempPath, bizErr := s.prepareAsset(input)
	if thumbTempPath != "" {
		defer func() { _ = os.Remove(thumbTempPath) }()
	}
	if bizErr != nil {
		return AssetDTO{}, bizErr
	}

	if err := s.storage.PromoteTempFile(input.TempPath, prepared.StorageKey); err != nil {
		s.log.ErrorContext(ctx, "upload_promote_file_failed",
			slog.Uint64("owner_id", input.OwnerID),
			slog.String("asset_id", prepared.AssetID),
			slog.Any("error", err),
		)
		return AssetDTO{}, errcode.ErrInternalConsistency
	}
	if prepared.ThumbnailKey != nil && thumbTempPath != "" {
		if err := s.storage.PromoteTempFile(thumbTempPath, *prepared.ThumbnailKey); err != nil {
			_ = s.storage.DeleteQuiet(prepared.StorageKey)
			s.log.ErrorContext(ctx, "upload_promote_thumbnail_failed",
				slog.Uint64("owner_id", input.OwnerID),
				slog.String("asset_id", prepared.AssetID),
				slog.Any("error", err),
			)
			return AssetDTO{}, errcode.ErrInternalConsistency
		}
	}

	if err := s.assets.Create(ctx, prepared); err != nil {
		_ = s.storage.DeleteQuiet(prepared.StorageKey)
		if prepared.ThumbnailKey != nil {
			_ = s.storage.DeleteQuiet(*prepared.ThumbnailKey)
		}
		mapped := mapMySQLError(err)
		s.log.ErrorContext(ctx, "upload_persist_asset_failed",
			slog.Uint64("owner_id", input.OwnerID),
			slog.String("asset_id", prepared.AssetID),
			slog.Int("code", mapped.Code),
			slog.Any("error", err),
		)
		return AssetDTO{}, mapped
	}

	s.log.InfoContext(ctx, "asset_uploaded",
		slog.Uint64("owner_id", input.OwnerID),
		slog.String("asset_id", prepared.AssetID),
		slog.String("purpose", prepared.Purpose),
		slog.String("mime", prepared.Mime),
		slog.Uint64("size_bytes", prepared.SizeBytes),
	)
	return s.toAssetDTO(prepared), nil
}

// UploadAvatar validates and persists an avatar image + thumbnail and updates users.avatar_url.
func (s *UploadService) UploadAvatar(ctx context.Context, input UploadInput) (UploadAvatarResult, *errcode.Error) {
	if input.TempPath != "" {
		defer func() { _ = os.Remove(input.TempPath) }()
	}
	if input.OwnerID == 0 {
		return UploadAvatarResult{}, errcode.ErrAccessTokenInvalid
	}
	if s == nil || s.assets == nil || s.storage == nil {
		return UploadAvatarResult{}, errcode.ErrInternalConsistency
	}

	input.Purpose = model.AssetPurposeAvatar
	prepared, thumbTempPath, bizErr := s.prepareAsset(input)
	if thumbTempPath != "" {
		defer func() { _ = os.Remove(thumbTempPath) }()
	}
	if bizErr != nil {
		return UploadAvatarResult{}, bizErr
	}

	if err := s.storage.PromoteTempFile(input.TempPath, prepared.StorageKey); err != nil {
		s.log.ErrorContext(ctx, "avatar_promote_file_failed",
			slog.Uint64("owner_id", input.OwnerID),
			slog.String("asset_id", prepared.AssetID),
			slog.Any("error", err),
		)
		return UploadAvatarResult{}, errcode.ErrInternalConsistency
	}
	if prepared.ThumbnailKey != nil && thumbTempPath != "" {
		if err := s.storage.PromoteTempFile(thumbTempPath, *prepared.ThumbnailKey); err != nil {
			_ = s.storage.DeleteQuiet(prepared.StorageKey)
			s.log.ErrorContext(ctx, "avatar_promote_thumbnail_failed",
				slog.Uint64("owner_id", input.OwnerID),
				slog.String("asset_id", prepared.AssetID),
				slog.Any("error", err),
			)
			return UploadAvatarResult{}, errcode.ErrInternalConsistency
		}
	}

	avatarURL := assetBaseURL(prepared.AssetID)
	updatedUser, err := s.assets.CreateAvatarAndBindUser(ctx, prepared, input.OwnerID, avatarURL, s.now())
	if err != nil {
		_ = s.storage.DeleteQuiet(prepared.StorageKey)
		if prepared.ThumbnailKey != nil {
			_ = s.storage.DeleteQuiet(*prepared.ThumbnailKey)
		}
		if errors.Is(err, repository.ErrUserDisabled) {
			return UploadAvatarResult{}, errcode.ErrAccountDisabled
		}
		mapped := mapMySQLError(err)
		if strings.Contains(strings.ToLower(err.Error()), "record not found") {
			mapped = errcode.ErrUserNotFound
		}
		s.log.ErrorContext(ctx, "avatar_bind_user_failed",
			slog.Uint64("owner_id", input.OwnerID),
			slog.String("asset_id", prepared.AssetID),
			slog.Int("code", mapped.Code),
			slog.Any("error", err),
		)
		return UploadAvatarResult{}, mapped
	}

	online := false
	if s.presence != nil {
		if ok, pErr := s.presence.IsUserOnline(ctx, updatedUser.ID); pErr == nil {
			online = ok
		}
	}

	s.log.InfoContext(ctx, "user_avatar_updated",
		slog.Uint64("user_id", input.OwnerID),
		slog.String("asset_id", prepared.AssetID),
	)

	return UploadAvatarResult{
		User:  toSelfUserProfile(updatedUser, online),
		Asset: s.toAssetDTO(prepared),
	}, nil
}

// GetAsset returns metadata and a short-lived download_url if the caller is authorized.
func (s *UploadService) GetAsset(ctx context.Context, viewerUserID uint64, assetID string) (AssetDTO, *errcode.Error) {
	if viewerUserID == 0 {
		return AssetDTO{}, errcode.ErrAccessTokenInvalid
	}
	if s == nil || s.assets == nil {
		return AssetDTO{}, errcode.ErrInternalConsistency
	}
	cleanID, idErr := validateAssetUUID(assetID)
	if idErr != nil {
		return AssetDTO{}, idErr
	}

	asset, bizErr := s.loadAuthorizedAsset(ctx, viewerUserID, cleanID)
	if bizErr != nil {
		return AssetDTO{}, bizErr
	}
	return s.toAssetDTO(asset), nil
}

// PrepareDownload authorizes access, optionally verifies signed download query parameters,
// and opens the underlying asset or thumbnail file for safe streaming.
func (s *UploadService) PrepareDownload(
	ctx context.Context,
	viewerUserID uint64,
	assetID string,
	opts DownloadOptions,
) (DownloadHandle, *errcode.Error) {
	if viewerUserID == 0 {
		return DownloadHandle{}, errcode.ErrAccessTokenInvalid
	}
	if s == nil || s.assets == nil || s.storage == nil {
		return DownloadHandle{}, errcode.ErrInternalConsistency
	}
	cleanID, idErr := validateAssetUUID(assetID)
	if idErr != nil {
		return DownloadHandle{}, idErr
	}

	if opts.ExpiresParam != "" || opts.SignatureParam != "" {
		if !s.verifySignedDownload(cleanID, opts.ExpiresParam, opts.SignatureParam) {
			return DownloadHandle{}, errcode.ErrAssetAccessDenied
		}
	}

	asset, bizErr := s.loadAuthorizedAsset(ctx, viewerUserID, cleanID)
	if bizErr != nil {
		return DownloadHandle{}, bizErr
	}

	storageKey := asset.StorageKey
	mimeType := asset.Mime
	filename := asset.Filename
	if strings.EqualFold(strings.TrimSpace(opts.Variant), "thumbnail") {
		if asset.ThumbnailKey == nil || *asset.ThumbnailKey == "" || asset.ThumbnailMime == nil {
			return DownloadHandle{}, errcode.ErrAssetNotFound
		}
		storageKey = *asset.ThumbnailKey
		mimeType = *asset.ThumbnailMime
		filename = thumbnailDownloadFilename(asset.Filename, *asset.ThumbnailMime)
	}

	f, info, err := s.storage.OpenForRead(storageKey)
	if err != nil {
		if errors.Is(err, ErrStorageObjectNotFound) {
			return DownloadHandle{}, errcode.ErrAssetNotFound
		}
		s.log.ErrorContext(ctx, "asset_open_for_download_failed",
			slog.String("asset_id", cleanID),
			slog.Uint64("viewer_user_id", viewerUserID),
			slog.Any("error", err),
		)
		return DownloadHandle{}, errcode.ErrInternalConsistency
	}

	disposition := "inline"
	if opts.ForceAttach || (asset.Purpose == model.AssetPurposeFile && !strings.EqualFold(strings.TrimSpace(opts.Variant), "thumbnail")) {
		disposition = "attachment"
	}

	return DownloadHandle{
		File:        f,
		Filename:    filename,
		Mime:        mimeType,
		SizeBytes:   info.Size(),
		ModTime:     info.ModTime().UTC(),
		Disposition: disposition,
	}, nil
}

// DeleteAsset deletes an asset owned by ownerUserID if not referenced by any message.
func (s *UploadService) DeleteAsset(ctx context.Context, ownerUserID uint64, assetID string) (DeleteAssetResult, *errcode.Error) {
	if ownerUserID == 0 {
		return DeleteAssetResult{}, errcode.ErrAccessTokenInvalid
	}
	if s == nil || s.assets == nil || s.storage == nil {
		return DeleteAssetResult{}, errcode.ErrInternalConsistency
	}
	cleanID, idErr := validateAssetUUID(assetID)
	if idErr != nil {
		return DeleteAssetResult{}, idErr
	}

	deleted, err := s.assets.SoftDeleteOwned(ctx, cleanID, ownerUserID, s.now())
	if err != nil {
		switch {
		case errors.Is(err, repository.ErrAssetNotFound):
			return DeleteAssetResult{}, errcode.ErrAssetNotFound
		case errors.Is(err, repository.ErrAssetForbidden):
			return DeleteAssetResult{}, errcode.ErrAssetAccessDenied
		case errors.Is(err, repository.ErrAssetReferencedByMessage):
			return DeleteAssetResult{}, errcode.ErrAssetReferenced
		default:
			mapped := mapMySQLError(err)
			s.log.ErrorContext(ctx, "asset_delete_failed",
				slog.String("asset_id", cleanID),
				slog.Uint64("owner_id", ownerUserID),
				slog.Int("code", mapped.Code),
				slog.Any("error", err),
			)
			return DeleteAssetResult{}, mapped
		}
	}

	if err := s.storage.DeleteQuiet(deleted.StorageKey); err != nil {
		s.log.WarnContext(ctx, "asset_delete_disk_file_failed",
			slog.String("asset_id", cleanID),
			slog.Any("error", err),
		)
	}
	if deleted.ThumbnailKey != nil && *deleted.ThumbnailKey != "" {
		if err := s.storage.DeleteQuiet(*deleted.ThumbnailKey); err != nil {
			s.log.WarnContext(ctx, "asset_delete_disk_thumbnail_failed",
				slog.String("asset_id", cleanID),
				slog.Any("error", err),
			)
		}
	}

	s.log.InfoContext(ctx, "asset_deleted",
		slog.String("asset_id", cleanID),
		slog.Uint64("owner_id", ownerUserID),
	)
	return DeleteAssetResult{
		AssetID: cleanID,
		Deleted: true,
	}, nil
}

func (s *UploadService) prepareAsset(input UploadInput) (*model.Asset, string, *errcode.Error) {
	purpose := strings.TrimSpace(input.Purpose)
	if purpose == "" {
		return nil, "", errcode.ErrInvalidParams.WithMessage("purpose is required")
	}
	maxBytes, purposeErr := s.MaxBytesForPurpose(purpose)
	if purposeErr != nil {
		return nil, "", purposeErr
	}
	if input.TempPath == "" || input.SizeBytes == 0 {
		return nil, "", errcode.ErrUploadInvalidFile
	}
	if maxBytes > 0 && input.SizeBytes > uint64(maxBytes) {
		return nil, "", errcode.ErrUploadTooLarge
	}

	validated, bizErr := inspectAndValidateMedia(s.storage, input.TempPath, purpose, input.DeclaredMime, input.RawFilename, s.cfg)
	if bizErr != nil {
		return nil, validated.ThumbnailTempPath, bizErr
	}

	assetID := uuid.NewString()
	now := s.now()
	datePath := now.Format("2006/01/02")
	storageKey := fmt.Sprintf("%s/%s/%s%s", purpose, datePath, assetID, validated.Ext)
	cleanedName := SanitizeFilename(input.RawFilename, validated.Ext)

	var thumbKey *string
	if validated.ThumbnailTempPath != "" {
		k := fmt.Sprintf("thumbnails/%s/%s_thumb%s", datePath, assetID, validated.ThumbnailExt)
		thumbKey = &k
	}

	asset := &model.Asset{
		AssetID:            assetID,
		OwnerID:            input.OwnerID,
		Purpose:            purpose,
		Filename:           cleanedName,
		Mime:               validated.Mime,
		SizeBytes:          input.SizeBytes,
		SHA256:             input.SHA256,
		Width:              validated.Width,
		Height:             validated.Height,
		DurationMs:         validated.DurationMs,
		StorageKey:         storageKey,
		ThumbnailKey:       thumbKey,
		ThumbnailMime:      validated.ThumbnailMime,
		ThumbnailSizeBytes: validated.ThumbnailSizeBytes,
		RefType:            model.AssetRefNone,
		RefCount:           0,
		Status:             model.AssetStatusActive,
	}
	return asset, validated.ThumbnailTempPath, nil
}

func (s *UploadService) loadAuthorizedAsset(ctx context.Context, viewerUserID uint64, assetID string) (*model.Asset, *errcode.Error) {
	asset, err := s.assets.GetActiveByAssetID(ctx, assetID)
	if err != nil {
		if errors.Is(err, repository.ErrAssetNotFound) {
			return nil, errcode.ErrAssetNotFound
		}
		mapped := mapMySQLError(err)
		s.log.ErrorContext(ctx, "asset_lookup_failed",
			slog.String("asset_id", assetID),
			slog.Uint64("viewer_user_id", viewerUserID),
			slog.Int("code", mapped.Code),
			slog.Any("error", err),
		)
		return nil, mapped
	}

	if asset.OwnerID == viewerUserID {
		return asset, nil
	}

	switch asset.RefType {
	case model.AssetRefMessage:
		if asset.RefCount == 0 || asset.RefConversationID == nil || *asset.RefConversationID == 0 {
			return nil, errcode.ErrAssetAccessDenied
		}
		isMember, memErr := s.assets.IsConversationMember(ctx, *asset.RefConversationID, viewerUserID)
		if memErr != nil {
			mapped := mapMySQLError(memErr)
			s.log.ErrorContext(ctx, "asset_membership_check_failed",
				slog.String("asset_id", assetID),
				slog.Uint64("viewer_user_id", viewerUserID),
				slog.Int("code", mapped.Code),
				slog.Any("error", memErr),
			)
			return nil, mapped
		}
		if !isMember {
			return nil, errcode.ErrAssetAccessDenied
		}
		return asset, nil

	case model.AssetRefUserAvatar, model.AssetRefGroupAvatar:
		if asset.RefCount == 0 {
			return nil, errcode.ErrAssetAccessDenied
		}
		return asset, nil

	default:
		return nil, errcode.ErrAssetAccessDenied
	}
}

func (s *UploadService) toAssetDTO(a *model.Asset) AssetDTO {
	baseURL := assetBaseURL(a.AssetID)
	dto := AssetDTO{
		AssetID:     a.AssetID,
		Purpose:     a.Purpose,
		URL:         baseURL,
		DownloadURL: s.buildDownloadURL(a.AssetID),
		Filename:    a.Filename,
		Mime:        a.Mime,
		SizeBytes:   a.SizeBytes,
		Width:       a.Width,
		Height:      a.Height,
		DurationMs:  a.DurationMs,
	}
	if a.ThumbnailKey != nil && *a.ThumbnailKey != "" {
		thumbURL := baseURL + "?variant=thumbnail"
		dto.ThumbnailURL = &thumbURL
	}
	return dto
}

func (s *UploadService) buildDownloadURL(assetID string) string {
	base := assetBaseURL(assetID)
	if len(s.signingKey) == 0 || s.cfg.DownloadURLTTL <= 0 {
		return base
	}
	expires := s.now().Add(s.cfg.DownloadURLTTL).Unix()
	sig := s.signDownload(assetID, expires)
	return fmt.Sprintf("%s?expires=%d&sig=%s", base, expires, sig)
}

func (s *UploadService) signDownload(assetID string, expiresUnix int64) string {
	mac := hmac.New(sha256.New, s.signingKey)
	_, _ = fmt.Fprintf(mac, "%s:%d", assetID, expiresUnix)
	return hex.EncodeToString(mac.Sum(nil))
}

func (s *UploadService) verifySignedDownload(assetID, expiresStr, sigHex string) bool {
	if len(s.signingKey) == 0 {
		return true
	}
	if expiresStr == "" || sigHex == "" {
		return false
	}
	expiresUnix, err := strconv.ParseInt(strings.TrimSpace(expiresStr), 10, 64)
	if err != nil || expiresUnix < s.now().Unix() {
		return false
	}
	expected := s.signDownload(assetID, expiresUnix)
	return subtle.ConstantTimeCompare([]byte(strings.ToLower(strings.TrimSpace(sigHex))), []byte(expected)) == 1
}

func assetBaseURL(assetID string) string {
	return "/api/v1/uploads/" + assetID + "/download"
}

func validateAssetUUID(raw string) (string, *errcode.Error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", errcode.ErrInvalidValue.WithMessage("asset_id is required")
	}
	parsed, err := uuid.Parse(trimmed)
	if err != nil {
		return "", errcode.ErrInvalidValue.WithMessage("invalid asset_id")
	}
	return parsed.String(), nil
}

func thumbnailDownloadFilename(origFilename, thumbMime string) string {
	ext := ".png"
	if thumbMime == "image/jpeg" {
		ext = ".jpg"
	}
	dot := strings.LastIndex(origFilename, ".")
	stem := origFilename
	if dot > 0 {
		stem = origFilename[:dot]
	}
	if stem == "" {
		stem = "thumb"
	}
	return stem + "_thumb" + ext
}
