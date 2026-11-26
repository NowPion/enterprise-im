package tests

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"enterprise-im/backend/internal/model"
	"enterprise-im/backend/internal/pkg/errcode"
)

func TestStage6SixMessageTypesAndRealUploads(t *testing.T) {
	rig := newStage6Rig(t)
	expectDBAvailable(t, rig.db)

	alice := rig.registerAndLogin("s6_msg_a", "Alice Msg")
	bob := rig.registerAndLogin("s6_msg_b", "Bob Msg")
	charlie := rig.registerAndLogin("s6_msg_c", "Charlie Stranger")

	_, convID, acceptSysMsgID := rig.makeFriends(alice, bob)

	aliceWS := rig.dialWS(alice)
	bobWS := rig.dialWS(bob)

	// 1. Type: text (UTF-8 / Chinese + ASCII).
	textClientID := uuid.NewString()
	textContent := "企业即时通讯第六阶段测试消息 · Text"
	textAck := aliceWS.sendMsgAndAwaitAck(convID, textClientID, model.MessageTypeText, textContent, map[string]any{})
	if textAck.Type != "msg.ack" {
		t.Fatalf("expected msg.ack for text, got %s: %s", textAck.Type, string(textAck.Data))
	}
	textAckData := decodeStage6JSON[struct {
		MsgID string `json:"msg_id"`
		Seq   string `json:"seq"`
	}](t, textAck.Data)
	bobWS.awaitFrame("bob text msg.push", stage6IOTimeout, func(f stage6WSFrame) bool {
		if f.Type != "msg.push" {
			return false
		}
		pt, m := decodeMsgPushMessage(t, f)
		return pt == "message" && m.MsgID == textAckData.MsgID && m.MsgType == model.MessageTypeText && m.Content == textContent
	})

	// 2. Type: image (write real PNG temp file -> POST /api/v1/uploads -> send msg -> authorized download & metadata).
	pngBytes := makeStage6SamplePNG(16, 16)
	upImgSt, upImgEnv, imgAsset, upImgRaw := rig.uploadTempFile(alice.AccessToken, model.AssetPurposeImage, "qa-pic.png", "image/png", pngBytes)
	assertStage6OK(t, "upload image asset", upImgSt, upImgEnv, http.StatusCreated)
	assertNoJSONStorageKeyLeak(t, "POST /api/v1/uploads image", upImgRaw)
	if imgAsset.AssetID == "" || imgAsset.Purpose != model.AssetPurposeImage || imgAsset.Mime != "image/png" ||
		imgAsset.SizeBytes != uint64(len(pngBytes)) || imgAsset.Width == nil || *imgAsset.Width != 16 ||
		imgAsset.Height == nil || *imgAsset.Height != 16 || imgAsset.URL == "" ||
		imgAsset.ThumbnailURL == nil || *imgAsset.ThumbnailURL == "" {
		t.Fatalf("unexpected uploaded image asset metadata: %+v raw=%s", imgAsset, string(upImgRaw))
	}

	imgClientID := uuid.NewString()
	imgAck := aliceWS.sendMsgAndAwaitAck(convID, imgClientID, model.MessageTypeImage, "[图片]", map[string]any{
		"asset_id": imgAsset.AssetID,
	})
	if imgAck.Type != "msg.ack" {
		t.Fatalf("expected msg.ack for image, got %s: %s", imgAck.Type, string(imgAck.Data))
	}
	imgAckData := decodeStage6JSON[struct {
		MsgID string `json:"msg_id"`
	}](t, imgAck.Data)
	bobImgFrame := bobWS.awaitFrame("bob image msg.push", stage6IOTimeout, func(f stage6WSFrame) bool {
		if f.Type != "msg.push" {
			return false
		}
		pt, m := decodeMsgPushMessage(t, f)
		return pt == "message" && m.MsgID == imgAckData.MsgID
	})
	_, bobImgMsg := decodeMsgPushMessage(t, bobImgFrame)
	if bobImgMsg.MsgType != model.MessageTypeImage || bobImgMsg.extraMap(t)["asset_id"] != imgAsset.AssetID {
		t.Fatalf("bob image msg.push mismatch: %+v extra=%s", bobImgMsg, string(bobImgMsg.Extra))
	}

	// Receiver Bob can inspect asset metadata & download exact original PNG bytes.
	metaSt, _, metaEnv, metaRaw := rig.doRequest(http.MethodGet, "/api/v1/uploads/"+imgAsset.AssetID, bob.AccessToken, nil)
	assertStage6OK(t, "bob get image asset metadata", metaSt, metaEnv, http.StatusOK)
	assertNoJSONStorageKeyLeak(t, "GET /api/v1/uploads/{asset_id}", metaRaw)
	dlPath := "/api/v1/uploads/" + imgAsset.AssetID + "/download"
	dlSt, dlHdr, dlBody := rig.downloadAuthorized(dlPath, bob.AccessToken)
	if dlSt != http.StatusOK || !bytes.Equal(dlBody, pngBytes) {
		t.Fatalf("bob download image failed: status=%d len=%d want=%d", dlSt, len(dlBody), len(pngBytes))
	}
	if !strings.Contains(dlHdr.Get("Content-Disposition"), "qa-pic.png") {
		t.Fatalf("expected Content-Disposition with sanitized filename, got %q", dlHdr.Get("Content-Disposition"))
	}

	// Unauthenticated download & stranger download must fail closed; referenced asset deletion must return 16006.
	unauthDlSt, _, _ := rig.downloadAuthorized(dlPath, "")
	if unauthDlSt != http.StatusUnauthorized {
		t.Fatalf("expected 401 for unauthenticated download, got %d", unauthDlSt)
	}
	strangerMetaSt, _, strangerMetaEnv, _ := rig.doRequest(http.MethodGet, "/api/v1/uploads/"+imgAsset.AssetID, charlie.AccessToken, nil)
	assertStage6BizErr(t, "stranger get asset metadata", strangerMetaSt, strangerMetaEnv, errcode.ErrAssetAccessDenied.Code)

	strangerDlSt, _, strangerDlBody := rig.downloadAuthorized(dlPath, charlie.AccessToken)
	var strangerDlEnv stage6Envelope
	_ = json.Unmarshal(strangerDlBody, &strangerDlEnv)
	assertStage6BizErr(t, "stranger download asset", strangerDlSt, strangerDlEnv, errcode.ErrAssetAccessDenied.Code)

	delRefSt, _, delRefEnv, _ := rig.doRequest(http.MethodDelete, "/api/v1/uploads/"+imgAsset.AssetID, alice.AccessToken, nil)
	assertStage6BizErr(t, "delete referenced image asset", delRefSt, delRefEnv, errcode.ErrAssetReferenced.Code)

	// Unused asset can be deleted cleanly by its owner and becomes 16005 afterward.
	unusedSt, unusedEnv, unusedAsset, _ := rig.uploadTempFile(alice.AccessToken, model.AssetPurposeImage, "unused.png", "image/png", pngBytes)
	assertStage6OK(t, "upload unused image", unusedSt, unusedEnv, http.StatusCreated)
	delUnusedSt, _, delUnusedEnv, _ := rig.doRequest(http.MethodDelete, "/api/v1/uploads/"+unusedAsset.AssetID, alice.AccessToken, nil)
	assertStage6OK(t, "delete unreferenced asset", delUnusedSt, delUnusedEnv, http.StatusOK)
	afterDelSt, _, afterDelEnv, _ := rig.doRequest(http.MethodGet, "/api/v1/uploads/"+unusedAsset.AssetID, alice.AccessToken, nil)
	assertStage6BizErr(t, "get deleted asset", afterDelSt, afterDelEnv, errcode.ErrAssetNotFound.Code)

	// 3. Type: file (write real PDF temp file -> POST /api/v1/uploads -> send HTTP message -> authorized download).
	pdfBytes := makeStage6SamplePDF()
	upFileSt, upFileEnv, fileAsset, upFileRaw := rig.uploadTempFile(alice.AccessToken, model.AssetPurposeFile, "design-spec.pdf", "application/pdf", pdfBytes)
	assertStage6OK(t, "upload pdf file asset", upFileSt, upFileEnv, http.StatusCreated)
	assertNoJSONStorageKeyLeak(t, "POST /api/v1/uploads file", upFileRaw)
	if fileAsset.AssetID == "" || fileAsset.Purpose != model.AssetPurposeFile || fileAsset.SizeBytes != uint64(len(pdfBytes)) {
		t.Fatalf("unexpected file asset metadata: %+v", fileAsset)
	}

	fileSendSt, _, fileSendEnv, _ := rig.doRequest(http.MethodPost, "/api/v1/conversations/"+convID+"/messages", alice.AccessToken, map[string]any{
		"client_msg_id": uuid.NewString(),
		"msg_type":      model.MessageTypeFile,
		"content":       "[文件] design-spec.pdf",
		"extra":         map[string]any{"asset_id": fileAsset.AssetID},
	})
	assertStage6OK(t, "HTTP send file message", fileSendSt, fileSendEnv, http.StatusCreated)
	fileMsg := decodeStage6JSON[struct {
		Message stage6MessageDTO `json:"message"`
	}](t, fileSendEnv.Data).Message
	if fileMsg.MsgType != model.MessageTypeFile || fileMsg.extraMap(t)["asset_id"] != fileAsset.AssetID {
		t.Fatalf("unexpected file message payload: %+v", fileMsg)
	}
	bobWS.awaitFrame("bob file msg.push", stage6IOTimeout, func(f stage6WSFrame) bool {
		if f.Type != "msg.push" {
			return false
		}
		pt, m := decodeMsgPushMessage(t, f)
		return pt == "message" && m.MsgID == fileMsg.MsgID && m.MsgType == model.MessageTypeFile &&
			m.extraMap(t)["asset_id"] == fileAsset.AssetID
	})

	fileDlSt, fileDlHdr, fileDlBody := rig.downloadAuthorized("/api/v1/uploads/"+fileAsset.AssetID+"/download", bob.AccessToken)
	if fileDlSt != http.StatusOK || sha256Hex(fileDlBody) != sha256Hex(pdfBytes) {
		t.Fatalf("bob download file failed: status=%d sha_got=%s sha_want=%s", fileDlSt, sha256Hex(fileDlBody), sha256Hex(pdfBytes))
	}
	if !strings.Contains(fileDlHdr.Get("Content-Disposition"), "design-spec.pdf") {
		t.Fatalf("expected Content-Disposition with filename, got %q", fileDlHdr.Get("Content-Disposition"))
	}

	// 4. Type: voice (write real WAV temp file -> POST /api/v1/uploads -> send WS message -> authorized download).
	wavBytes := makeStage6SampleWAV(1)
	upVoiceSt, upVoiceEnv, voiceAsset, upVoiceRaw := rig.uploadTempFile(bob.AccessToken, model.AssetPurposeVoice, "voice-note.wav", "audio/wav", wavBytes)
	assertStage6OK(t, "upload voice asset", upVoiceSt, upVoiceEnv, http.StatusCreated)
	assertNoJSONStorageKeyLeak(t, "POST /api/v1/uploads voice", upVoiceRaw)
	if voiceAsset.AssetID == "" || voiceAsset.Purpose != model.AssetPurposeVoice || voiceAsset.SizeBytes != uint64(len(wavBytes)) ||
		voiceAsset.DurationMs == nil || *voiceAsset.DurationMs == 0 {
		t.Fatalf("unexpected voice asset metadata: %+v raw=%s", voiceAsset, string(upVoiceRaw))
	}

	voiceAck := bobWS.sendMsgAndAwaitAck(convID, uuid.NewString(), model.MessageTypeVoice, "[语音]", map[string]any{
		"asset_id": voiceAsset.AssetID,
	})
	if voiceAck.Type != "msg.ack" {
		t.Fatalf("expected msg.ack for voice, got %s: %s", voiceAck.Type, string(voiceAck.Data))
	}
	voiceAckData := decodeStage6JSON[struct {
		MsgID string `json:"msg_id"`
	}](t, voiceAck.Data)
	aliceWS.awaitFrame("alice voice msg.push", stage6IOTimeout, func(f stage6WSFrame) bool {
		if f.Type != "msg.push" {
			return false
		}
		pt, m := decodeMsgPushMessage(t, f)
		return pt == "message" && m.MsgID == voiceAckData.MsgID && m.MsgType == model.MessageTypeVoice &&
			m.extraMap(t)["asset_id"] == voiceAsset.AssetID
	})

	voiceDlSt, _, voiceDlBody := rig.downloadAuthorized("/api/v1/uploads/"+voiceAsset.AssetID+"/download", alice.AccessToken)
	if voiceDlSt != http.StatusOK || sha256Hex(voiceDlBody) != sha256Hex(wavBytes) {
		t.Fatalf("alice download voice failed: status=%d len=%d want=%d", voiceDlSt, len(voiceDlBody), len(wavBytes))
	}

	// 5. Type: recall (HTTP recall in window, WS recall in window, repeat 14004, non-sender 14003, expired 14005).
	recallSt, _, recallEnv, _ := rig.doRequest(http.MethodPost, "/api/v1/conversations/"+convID+"/messages/"+textAckData.MsgID+"/recall", alice.AccessToken, nil)
	assertStage6OK(t, "HTTP recall in window", recallSt, recallEnv, http.StatusOK)

	bobWS.awaitFrame("bob recall msg.push or notice for HTTP recall", stage6IOTimeout, func(f stage6WSFrame) bool {
		if f.Type == "msg.push" {
			pt, m := decodeMsgPushMessage(t, f)
			return pt == "message" && m.MsgType == model.MessageTypeRecall
		}
		if f.Type == "system.notice" {
			var n struct {
				Event string `json:"event"`
			}
			return json.Unmarshal(f.Data, &n) == nil && n.Event == "message.recalled"
		}
		return false
	})

	// Repeat recall must return 14004 ErrMessageAlreadyRecalled.
	reRecallSt, _, reRecallEnv, _ := rig.doRequest(http.MethodPost, "/api/v1/conversations/"+convID+"/messages/"+textAckData.MsgID+"/recall", alice.AccessToken, nil)
	assertStage6BizErr(t, "repeat recall", reRecallSt, reRecallEnv, errcode.ErrMessageAlreadyRecalled.Code)

	// Send another text message by Alice and recall it via WS msg.recall.
	wsRecallTargetAck := aliceWS.sendMsgAndAwaitAck(convID, uuid.NewString(), model.MessageTypeText, "message to recall via WS", map[string]any{})
	if wsRecallTargetAck.Type != "msg.ack" {
		t.Fatalf("expected msg.ack before WS recall, got %s: %s", wsRecallTargetAck.Type, string(wsRecallTargetAck.Data))
	}
	wsRecallTargetID := decodeStage6JSON[struct {
		MsgID string `json:"msg_id"`
	}](t, wsRecallTargetAck.Data).MsgID
	bobWS.awaitFrame("bob receives message before WS recall", stage6IOTimeout, func(f stage6WSFrame) bool {
		if f.Type != "msg.push" {
			return false
		}
		pt, m := decodeMsgPushMessage(t, f)
		return pt == "message" && m.MsgID == wsRecallTargetID
	})

	recallReqID := "recall-" + uuid.NewString()
	aliceWS.send("msg.recall", recallReqID, map[string]any{
		"conversation_id": convID,
		"msg_id":          wsRecallTargetID,
	})
	recallRespFrame := aliceWS.awaitFrame("ws msg.recall ack", stage6IOTimeout, func(f stage6WSFrame) bool {
		return (f.Type == "msg.recall" || f.Type == "error") && f.ReqID == recallReqID
	})
	if recallRespFrame.Type != "msg.recall" {
		t.Fatalf("expected ws msg.recall response, got %s: %s", recallRespFrame.Type, string(recallRespFrame.Data))
	}
	recallResp := decodeStage6JSON[struct {
		ConversationID string `json:"conversation_id"`
		RecalledMsgID  string `json:"recalled_msg_id"`
		RecallMsgID    string `json:"recall_msg_id"`
		Seq            string `json:"seq"`
		RecalledAt     string `json:"recalled_at"`
	}](t, recallRespFrame.Data)
	if recallResp.ConversationID != convID || recallResp.RecalledMsgID != wsRecallTargetID ||
		recallResp.RecallMsgID == "" || recallResp.Seq == "" || !isValidRFC3339Millis(recallResp.RecalledAt) {
		t.Fatalf("unexpected ws msg.recall response payload: %+v", recallResp)
	}
	bobWS.awaitFrame("bob recall msg.push or notice for WS recall", stage6IOTimeout, func(f stage6WSFrame) bool {
		if f.Type == "msg.push" {
			pt, m := decodeMsgPushMessage(t, f)
			return pt == "message" && m.MsgType == model.MessageTypeRecall && m.MsgID == recallResp.RecallMsgID
		}
		if f.Type == "system.notice" {
			var n struct {
				Event   string `json:"event"`
				Payload struct {
					RecalledMsgID string `json:"recalled_msg_id"`
				} `json:"payload"`
			}
			return json.Unmarshal(f.Data, &n) == nil && n.Event == "message.recalled" && n.Payload.RecalledMsgID == wsRecallTargetID
		}
		return false
	})

	// Non-sender Bob cannot recall Alice's image message -> 14003 ErrMessageAccessDenied.
	nonSenderSt, _, nonSenderEnv, _ := rig.doRequest(http.MethodPost, "/api/v1/conversations/"+convID+"/messages/"+imgAckData.MsgID+"/recall", bob.AccessToken, nil)
	assertStage6BizErr(t, "non-sender recall", nonSenderSt, nonSenderEnv, errcode.ErrMessageAccessDenied.Code)

	// Expired recall window -> send fresh message, backdate created_at by 24h in MySQL, recall must return 14005.
	expSt, _, expEnv, _ := rig.doRequest(http.MethodPost, "/api/v1/conversations/"+convID+"/messages", alice.AccessToken, map[string]any{
		"client_msg_id": uuid.NewString(),
		"msg_type":      model.MessageTypeText,
		"content":       "old message outside recall window",
		"extra":         map[string]any{},
	})
	assertStage6OK(t, "send message for expired recall test", expSt, expEnv, http.StatusCreated)
	expMsg := decodeStage6JSON[struct {
		Message stage6MessageDTO `json:"message"`
	}](t, expEnv.Data).Message

	expiredAt := time.Now().UTC().Add(-24 * time.Hour).Truncate(time.Millisecond)
	if err := rig.db.WithContext(t.Context()).Model(&model.Message{}).
		Where("id = ?", mustParseUint64(t, expMsg.MsgID)).
		Update("created_at", expiredAt).Error; err != nil {
		t.Fatalf("backdate message created_at in MySQL: %v", err)
	}
	expRecallSt, _, expRecallEnv, _ := rig.doRequest(http.MethodPost, "/api/v1/conversations/"+convID+"/messages/"+expMsg.MsgID+"/recall", alice.AccessToken, nil)
	assertStage6BizErr(t, "expired window recall", expRecallSt, expRecallEnv, errcode.ErrRecallWindowExpired.Code)

	// 6. Type: system + verify all 6 types appear accurately in conversation history.
	histSt, _, histEnv, _ := rig.doRequest(http.MethodGet, "/api/v1/conversations/"+convID+"/messages?limit=50", alice.AccessToken, nil)
	assertStage6OK(t, "full single chat history", histSt, histEnv, http.StatusOK)
	histPage := decodeStage6JSON[stage6MessagePageDTO](t, histEnv.Data)
	verifySeqStrictlyIncreasing(t, histPage.Items)

	seenTypes := map[string]bool{}
	for _, m := range histPage.Items {
		seenTypes[m.MsgType] = true
		if m.MsgID == textAckData.MsgID || m.MsgID == wsRecallTargetID {
			if m.Status != model.MessageStatusRecalled || m.RecalledAt == nil {
				t.Fatalf("expected recalled message %s to have status=recalled & recalled_at, got %+v", m.MsgID, m)
			}
		}
		if m.MsgID == expMsg.MsgID && m.Status != model.MessageStatusNormal {
			t.Fatalf("expected expired-recall message %s to stay normal, got %s", m.MsgID, m.Status)
		}
	}
	for _, wantType := range []string{
		model.MessageTypeSystem,
		model.MessageTypeText,
		model.MessageTypeImage,
		model.MessageTypeFile,
		model.MessageTypeVoice,
		model.MessageTypeRecall,
	} {
		if !seenTypes[wantType] {
			t.Fatalf("expected message type %q in conversation history; seen=%v", wantType, seenTypes)
		}
	}
	if sys, ok := findMessageInPage(histPage.Items, acceptSysMsgID); !ok || sys.MsgType != model.MessageTypeSystem {
		t.Fatalf("expected friend-accept system message %s in history", acceptSysMsgID)
	}
}
