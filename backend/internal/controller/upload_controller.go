package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/gin-gonic/gin"

	"enterprise-im/backend/internal/model"
	"enterprise-im/backend/internal/pkg/errcode"
	"enterprise-im/backend/internal/service"
)

const multipartOverheadBytes int64 = 1 << 20 // 1 MiB allowance for multipart headers and boundaries

// UploadController handles HTTP endpoints for file uploads, metadata, authorized downloads,
// asset deletion, and user avatar updates.
type UploadController struct {
	uploadSvc *service.UploadService
}

// NewUploadController creates an UploadController backed by UploadService.
func NewUploadController(uploadSvc *service.UploadService) *UploadController {
	return &UploadController{uploadSvc: uploadSvc}
}

// Upload handles POST /api/v1/uploads.
func (h *UploadController) Upload(c *gin.Context) {
	if h == nil || h.uploadSvc == nil {
		errcode.Fail(c, errcode.ErrInternalConsistency)
		return
	}
	userID, _, _, authErr := extractAuthIdentity(c)
	if authErr != nil {
		errcode.Fail(c, authErr)
		return
	}

	input, parseErr := h.parseMultipartUpload(c, userID, "")
	if parseErr != nil {
		errcode.Fail(c, parseErr)
		return
	}

	dto, bizErr := h.uploadSvc.Upload(c.Request.Context(), input)
	if bizErr != nil {
		errcode.Fail(c, bizErr)
		return
	}
	errcode.Created(c, dto)
}

// UploadMyAvatar handles POST /api/v1/me/avatar.
func (h *UploadController) UploadMyAvatar(c *gin.Context) {
	if h == nil || h.uploadSvc == nil {
		errcode.Fail(c, errcode.ErrInternalConsistency)
		return
	}
	userID, _, _, authErr := extractAuthIdentity(c)
	if authErr != nil {
		errcode.Fail(c, authErr)
		return
	}

	input, parseErr := h.parseMultipartUpload(c, userID, model.AssetPurposeAvatar)
	if parseErr != nil {
		errcode.Fail(c, parseErr)
		return
	}

	res, bizErr := h.uploadSvc.UploadAvatar(c.Request.Context(), input)
	if bizErr != nil {
		errcode.Fail(c, bizErr)
		return
	}
	errcode.OK(c, res)
}

// GetAsset handles GET /api/v1/uploads/:asset_id.
func (h *UploadController) GetAsset(c *gin.Context) {
	if h == nil || h.uploadSvc == nil {
		errcode.Fail(c, errcode.ErrInternalConsistency)
		return
	}
	userID, _, _, authErr := extractAuthIdentity(c)
	if authErr != nil {
		errcode.Fail(c, authErr)
		return
	}

	dto, bizErr := h.uploadSvc.GetAsset(c.Request.Context(), userID, c.Param("asset_id"))
	if bizErr != nil {
		errcode.Fail(c, bizErr)
		return
	}
	errcode.OK(c, dto)
}

// DownloadAsset handles GET /api/v1/uploads/:asset_id/download.
func (h *UploadController) DownloadAsset(c *gin.Context) {
	if h == nil || h.uploadSvc == nil {
		errcode.Fail(c, errcode.ErrInternalConsistency)
		return
	}
	userID, _, _, authErr := extractAuthIdentity(c)
	if authErr != nil {
		errcode.Fail(c, authErr)
		return
	}

	rawDownload := strings.TrimSpace(c.Query("download"))
	forceAttach := rawDownload == "1" || strings.EqualFold(rawDownload, "true")

	handle, bizErr := h.uploadSvc.PrepareDownload(c.Request.Context(), userID, c.Param("asset_id"), service.DownloadOptions{
		Variant:        c.Query("variant"),
		ForceAttach:    forceAttach,
		ExpiresParam:   c.Query("expires"),
		SignatureParam: c.Query("sig"),
	})
	if bizErr != nil {
		errcode.Fail(c, bizErr)
		return
	}
	defer func() { _ = handle.File.Close() }()

	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Cache-Control", "private, no-store")
	c.Header("Content-Type", handle.Mime)
	c.Header("Content-Disposition", formatContentDisposition(handle.Disposition, handle.Filename))
	http.ServeContent(c.Writer, c.Request, handle.Filename, handle.ModTime, handle.File)
}

// DeleteAsset handles DELETE /api/v1/uploads/:asset_id.
func (h *UploadController) DeleteAsset(c *gin.Context) {
	if h == nil || h.uploadSvc == nil {
		errcode.Fail(c, errcode.ErrInternalConsistency)
		return
	}
	userID, _, _, authErr := extractAuthIdentity(c)
	if authErr != nil {
		errcode.Fail(c, authErr)
		return
	}

	res, bizErr := h.uploadSvc.DeleteAsset(c.Request.Context(), userID, c.Param("asset_id"))
	if bizErr != nil {
		errcode.Fail(c, bizErr)
		return
	}
	errcode.OK(c, res)
}

func (h *UploadController) parseMultipartUpload(c *gin.Context, ownerID uint64, fixedPurpose string) (service.UploadInput, *errcode.Error) {
	if c == nil || c.Request == nil || c.Request.Body == nil {
		return service.UploadInput{}, errcode.ErrUploadInvalidFile
	}

	maxFileCap := h.uploadSvc.UploadConfig().MaxPossibleBytes()
	if fixedPurpose != "" {
		if capForFixed, err := h.uploadSvc.MaxBytesForPurpose(fixedPurpose); err == nil && capForFixed > 0 {
			maxFileCap = capForFixed
		}
	}
	if maxFileCap <= 0 {
		return service.UploadInput{}, errcode.ErrInternalConsistency
	}
	maxReqBytes := maxFileCap + multipartOverheadBytes

	if c.Request.ContentLength > maxReqBytes {
		return service.UploadInput{}, errcode.ErrUploadTooLarge
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxReqBytes)

	mr, err := c.Request.MultipartReader()
	if err != nil {
		if isMaxBytesError(err) {
			return service.UploadInput{}, errcode.ErrUploadTooLarge
		}
		return service.UploadInput{}, errcode.ErrUploadInvalidFile
	}

	var (
		purpose      = fixedPurpose
		hasFile      bool
		rawFilename  string
		declaredMime string
		tempPath     string
		sizeBytes    uint64
		shaHex       string
	)
	cleanupTemp := true
	defer func() {
		if cleanupTemp && tempPath != "" {
			_ = os.Remove(tempPath)
		}
	}()

	for {
		part, nextErr := mr.NextPart()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			if isMaxBytesError(nextErr) {
				return service.UploadInput{}, errcode.ErrUploadTooLarge
			}
			return service.UploadInput{}, errcode.ErrUploadInvalidFile
		}

		formName := part.FormName()
		switch {
		case formName == "purpose" && fixedPurpose == "":
			buf, readErr := io.ReadAll(io.LimitReader(part, 65))
			_ = part.Close()
			if readErr != nil {
				if isMaxBytesError(readErr) {
					return service.UploadInput{}, errcode.ErrUploadTooLarge
				}
				return service.UploadInput{}, errcode.ErrInvalidParams
			}
			if len(buf) > 64 {
				return service.UploadInput{}, errcode.ErrInvalidValue.WithMessage("invalid upload purpose")
			}
			purpose = strings.TrimSpace(string(buf))

		case formName == "file" && !hasFile:
			hasFile = true
			rawFilename = part.FileName()
			declaredMime = part.Header.Get("Content-Type")

			tmpFile, tmpName, tmpErr := h.uploadSvc.CreateTempFile("upload")
			if tmpErr != nil {
				_ = part.Close()
				return service.UploadInput{}, errcode.ErrInternalConsistency
			}
			tempPath = tmpName

			hasher := sha256.New()
			written, copyErr := io.Copy(io.MultiWriter(tmpFile, hasher), io.LimitReader(part, maxFileCap+1))
			_ = part.Close()
			if copyErr != nil {
				_ = tmpFile.Close()
				if isMaxBytesError(copyErr) {
					return service.UploadInput{}, errcode.ErrUploadTooLarge
				}
				return service.UploadInput{}, errcode.ErrUploadInvalidFile
			}
			if written > maxFileCap {
				_ = tmpFile.Close()
				return service.UploadInput{}, errcode.ErrUploadTooLarge
			}
			if err := tmpFile.Sync(); err != nil {
				_ = tmpFile.Close()
				return service.UploadInput{}, errcode.ErrInternalConsistency
			}
			if err := tmpFile.Close(); err != nil {
				return service.UploadInput{}, errcode.ErrInternalConsistency
			}
			sizeBytes = uint64(written)
			shaHex = hex.EncodeToString(hasher.Sum(nil))

		default:
			_ = part.Close()
		}
	}

	if purpose == "" {
		purpose = strings.TrimSpace(c.Query("purpose"))
	}
	if purpose == "" {
		return service.UploadInput{}, errcode.ErrInvalidParams.WithMessage("purpose is required")
	}
	purposeMax, purposeErr := h.uploadSvc.MaxBytesForPurpose(purpose)
	if purposeErr != nil {
		return service.UploadInput{}, purposeErr
	}
	if !hasFile || sizeBytes == 0 {
		return service.UploadInput{}, errcode.ErrUploadInvalidFile
	}
	if purposeMax > 0 && sizeBytes > uint64(purposeMax) {
		return service.UploadInput{}, errcode.ErrUploadTooLarge
	}

	cleanupTemp = false
	return service.UploadInput{
		OwnerID:      ownerID,
		Purpose:      purpose,
		RawFilename:  rawFilename,
		DeclaredMime: declaredMime,
		TempPath:     tempPath,
		SizeBytes:    sizeBytes,
		SHA256:       shaHex,
	}, nil
}

func isMaxBytesError(err error) bool {
	if err == nil {
		return false
	}
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		return true
	}
	return strings.Contains(strings.ToLower(err.Error()), "request body too large")
}

func formatContentDisposition(disposition, filename string) string {
	if disposition != "attachment" {
		disposition = "inline"
	}
	var ascii strings.Builder
	needsUTF8 := false
	for _, r := range filename {
		if r >= 0x20 && r <= 0x7e && r != '"' && r != '\\' && r != ';' {
			ascii.WriteRune(r)
		} else {
			if r > 0x7e {
				needsUTF8 = true
			}
			ascii.WriteByte('_')
		}
	}
	fallback := strings.TrimSpace(ascii.String())
	if fallback == "" {
		fallback = "download"
	}
	if !needsUTF8 && fallback == filename {
		return fmt.Sprintf(`%s; filename="%s"`, disposition, fallback)
	}
	return fmt.Sprintf(`%s; filename="%s"; filename*=UTF-8''%s`, disposition, fallback, url.PathEscape(filename))
}
