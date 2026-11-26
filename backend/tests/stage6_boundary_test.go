package tests

import (
	"net/http"
	"testing"

	"github.com/google/uuid"

	"enterprise-im/backend/internal/model"
	"enterprise-im/backend/internal/pkg/errcode"
)

func TestStage6BoundaryNonMemberAccessAndInvalidAttachments(t *testing.T) {
	rig := newStage6Rig(t)
	expectDBAvailable(t, rig.db)

	alice := rig.registerAndLogin("s6_bnd_a", "Alice Boundary")
	bob := rig.registerAndLogin("s6_bnd_b", "Bob Boundary")
	outsider := rig.registerAndLogin("s6_bnd_c", "Outsider")

	_, convID, acceptSysMsgID := rig.makeFriends(alice, bob)

	// Send one legitimate message by Alice.
	okSt, _, okEnv, _ := rig.doRequest(http.MethodPost, "/api/v1/conversations/"+convID+"/messages", alice.AccessToken, map[string]any{
		"client_msg_id": uuid.NewString(),
		"msg_type":      model.MessageTypeText,
		"content":       "legitimate single chat message",
		"extra":         map[string]any{},
	})
	assertStage6OK(t, "seed single chat message", okSt, okEnv, http.StatusCreated)
	legitMsg := decodeStage6JSON[struct {
		Message stage6MessageDTO `json:"message"`
	}](t, okEnv.Data).Message

	// 1. Non-member cannot inspect conversation, members, history, sync, send HTTP/WS, or recall.
	outsiderWS := rig.dialWS(outsider)
	for _, tc := range []struct {
		name   string
		method string
		path   string
		body   any
		codes  []int
	}{
		{"conversation detail", http.MethodGet, "/api/v1/conversations/" + convID, nil, []int{errcode.ErrNotConversationMember.Code}},
		{"conversation members", http.MethodGet, "/api/v1/conversations/" + convID + "/members", nil, []int{errcode.ErrNotConversationMember.Code}},
		{"conversation history", http.MethodGet, "/api/v1/conversations/" + convID + "/messages", nil, []int{errcode.ErrNotConversationMember.Code}},
		{"conversation sync", http.MethodGet, "/api/v1/conversations/" + convID + "/sync?after_seq=0", nil, []int{errcode.ErrNotConversationMember.Code}},
		{"http send message", http.MethodPost, "/api/v1/conversations/" + convID + "/messages", map[string]any{"client_msg_id": uuid.NewString(), "msg_type": model.MessageTypeText, "content": "nope", "extra": map[string]any{}}, []int{errcode.ErrNotConversationMember.Code}},
		{"recall foreign message", http.MethodPost, "/api/v1/conversations/" + convID + "/messages/" + legitMsg.MsgID + "/recall", nil, []int{errcode.ErrNotConversationMember.Code, errcode.ErrMessageAccessDenied.Code}},
	} {
		st, _, env, _ := rig.doRequest(tc.method, tc.path, outsider.AccessToken, tc.body)
		assertStage6BizErr(t, "outsider "+tc.name, st, env, tc.codes...)
	}

	wsDeny := outsiderWS.sendMsgAndAwaitAck(convID, uuid.NewString(), model.MessageTypeText, "outsider ws msg", map[string]any{})
	if wsDeny.Type != "error" {
		t.Fatalf("expected WS error for outsider msg.send, got %s: %s", wsDeny.Type, string(wsDeny.Data))
	}
	wsDenyErr := decodeWSError(t, wsDeny)
	if wsDenyErr.Code != errcode.ErrWSConvAccessDenied.Code && wsDenyErr.Code != errcode.ErrNotConversationMember.Code {
		t.Fatalf("expected 18005 or 13002 for outsider WS send, got %+v", wsDenyErr)
	}

	// Non-existent conversation returns 13001.
	noConvSt, _, noConvEnv, _ := rig.doRequest(http.MethodGet, "/api/v1/conversations/999999999999/messages", alice.AccessToken, nil)
	assertStage6BizErr(t, "unknown conversation", noConvSt, noConvEnv, errcode.ErrConversationNotFound.Code)

	// 2. Illegal uploads & MIME/content mismatches (16001 / 16003).
	missingFileSt, missingFileEnv, _, _ := rig.uploadTempFile(alice.AccessToken, model.AssetPurposeImage, "", "", nil)
	assertStage6BizErr(t, "upload missing file field", missingFileSt, missingFileEnv, errcode.ErrUploadInvalidFile.Code)

	badHTML := []byte("<html><body><script>alert(1)</script></body></html>")
	badMimeSt, badMimeEnv, _, _ := rig.uploadTempFile(alice.AccessToken, model.AssetPurposeImage, "evil.html", "text/html", badHTML)
	assertStage6BizErr(t, "upload disallowed html as image", badMimeSt, badMimeEnv, errcode.ErrUploadInvalidFile.Code, errcode.ErrUploadTypeMismatch.Code)

	fakeImg := []byte("not-a-real-png-binary-payload")
	spoofSt, spoofEnv, _, _ := rig.uploadTempFile(alice.AccessToken, model.AssetPurposeImage, "spoof.png", "image/png", fakeImg)
	assertStage6BizErr(t, "upload spoofed text with image/png header", spoofSt, spoofEnv, errcode.ErrUploadTypeMismatch.Code, errcode.ErrUploadInvalidFile.Code)

	pngBytes := makeStage6SamplePNG(8, 8)
	purposeMismatchSt, purposeMismatchEnv, _, _ := rig.uploadTempFile(alice.AccessToken, model.AssetPurposeVoice, "not-audio.png", "image/png", pngBytes)
	assertStage6BizErr(t, "upload png with purpose=voice", purposeMismatchSt, purposeMismatchEnv, errcode.ErrUploadInvalidFile.Code, errcode.ErrUploadTypeMismatch.Code)

	// 3. Message type / attachment mismatches (14006).
	upFileSt, upFileEnv, fileAsset, _ := rig.uploadTempFile(alice.AccessToken, model.AssetPurposeFile, "doc.pdf", "application/pdf", makeStage6SamplePDF())
	assertStage6OK(t, "upload valid pdf", upFileSt, upFileEnv, http.StatusCreated)

	upImgSt, upImgEnv, imgAsset, _ := rig.uploadTempFile(alice.AccessToken, model.AssetPurposeImage, "pic.png", "image/png", pngBytes)
	assertStage6OK(t, "upload valid png", upImgSt, upImgEnv, http.StatusCreated)

	bobFileSt, bobFileEnv, bobAsset, _ := rig.uploadTempFile(bob.AccessToken, model.AssetPurposeFile, "bob.pdf", "application/pdf", makeStage6SamplePDF())
	assertStage6OK(t, "upload valid pdf by bob", bobFileSt, bobFileEnv, http.StatusCreated)

	aliceWS := rig.dialWS(alice)

	for _, tc := range []struct {
		name    string
		msgType string
		content string
		extra   map[string]any
		codes   []int
	}{
		{
			name:    "image msg referencing file asset",
			msgType: model.MessageTypeImage,
			content: "[图片]",
			extra:   map[string]any{"asset_id": fileAsset.AssetID},
			codes:   []int{errcode.ErrInvalidMessagePayload.Code},
		},
		{
			name:    "voice msg referencing image asset",
			msgType: model.MessageTypeVoice,
			content: "[语音]",
			extra:   map[string]any{"asset_id": imgAsset.AssetID},
			codes:   []int{errcode.ErrInvalidMessagePayload.Code},
		},
		{
			name:    "file msg referencing unknown asset_id",
			msgType: model.MessageTypeFile,
			content: "[文件]",
			extra:   map[string]any{"asset_id": uuid.NewString()},
			codes:   []int{errcode.ErrInvalidMessagePayload.Code, errcode.ErrAssetNotFound.Code},
		},
		{
			name:    "image msg missing asset_id",
			msgType: model.MessageTypeImage,
			content: "[图片]",
			extra:   map[string]any{},
			codes:   []int{errcode.ErrInvalidMessagePayload.Code},
		},
		{
			name:    "empty text message",
			msgType: model.MessageTypeText,
			content: "",
			extra:   map[string]any{},
			codes:   []int{errcode.ErrInvalidMessagePayload.Code, errcode.ErrInvalidParams.Code, errcode.ErrInvalidValue.Code},
		},
		{
			name:    "alice referencing bob's asset",
			msgType: model.MessageTypeFile,
			content: "[文件]",
			extra:   map[string]any{"asset_id": bobAsset.AssetID},
			codes:   []int{errcode.ErrInvalidMessagePayload.Code, errcode.ErrAssetAccessDenied.Code},
		},
	} {
		st, _, env, _ := rig.doRequest(http.MethodPost, "/api/v1/conversations/"+convID+"/messages", alice.AccessToken, map[string]any{
			"client_msg_id": uuid.NewString(),
			"msg_type":      tc.msgType,
			"content":       tc.content,
			"extra":         tc.extra,
		})
		assertStage6BizErr(t, "HTTP "+tc.name, st, env, tc.codes...)
	}

	wsMismatchClientID := uuid.NewString()
	wsMismatchFrame := aliceWS.sendMsgAndAwaitAck(convID, wsMismatchClientID, model.MessageTypeImage, "[图片]", map[string]any{
		"asset_id": fileAsset.AssetID,
	})
	if wsMismatchFrame.Type != "error" {
		t.Fatalf("expected WS error on image/file attachment mismatch, got %s: %s", wsMismatchFrame.Type, string(wsMismatchFrame.Data))
	}
	wsMismatchErr := decodeWSError(t, wsMismatchFrame)
	if wsMismatchErr.Code != errcode.ErrInvalidMessagePayload.Code {
		t.Fatalf("expected 14006 on WS attachment mismatch, got %+v", wsMismatchErr)
	}
	if n, err := countMessagesByClientID(t.Context(), rig.db, alice.ID, wsMismatchClientID); err != nil || n != 0 {
		t.Fatalf("rejected WS mismatch persisted %d rows (err=%v)", n, err)
	}

	// 4. Unfriend boundary: conversation hidden in default list, history still readable by ID, new sends rejected.
	delFriendSt, _, delFriendEnv, _ := rig.doRequest(http.MethodDelete, "/api/v1/friends/"+bob.IDStr, alice.AccessToken, nil)
	assertStage6OK(t, "delete friendship", delFriendSt, delFriendEnv, http.StatusOK)

	listAfterDelSt, _, listAfterDelEnv, _ := rig.doRequest(http.MethodGet, "/api/v1/conversations", alice.AccessToken, nil)
	assertStage6OK(t, "list conversations after unfriend", listAfterDelSt, listAfterDelEnv, http.StatusOK)
	afterDelPage := decodeStage6JSON[stage6ConversationPageDTO](t, listAfterDelEnv.Data)
	for _, c := range afterDelPage.Items {
		if c.ID == convID && c.IsVisible {
			t.Fatalf("expected unfriended conversation %s hidden in default list, got %+v", convID, c)
		}
	}

	histAfterDelSt, _, histAfterDelEnv, _ := rig.doRequest(http.MethodGet, "/api/v1/conversations/"+convID+"/messages", alice.AccessToken, nil)
	assertStage6OK(t, "read preserved history after unfriend", histAfterDelSt, histAfterDelEnv, http.StatusOK)
	histAfterDel := decodeStage6JSON[stage6MessagePageDTO](t, histAfterDelEnv.Data)
	if _, ok := findMessageInPage(histAfterDel.Items, acceptSysMsgID); !ok {
		t.Fatalf("expected preserved friend-accept system message %s after unfriend", acceptSysMsgID)
	}

	sendAfterDelSt, _, sendAfterDelEnv, _ := rig.doRequest(http.MethodPost, "/api/v1/conversations/"+convID+"/messages", alice.AccessToken, map[string]any{
		"client_msg_id": uuid.NewString(),
		"msg_type":      model.MessageTypeText,
		"content":       "send after unfriend",
		"extra":         map[string]any{},
	})
	assertStage6BizErr(t, "HTTP send after unfriend", sendAfterDelSt, sendAfterDelEnv,
		errcode.ErrConversationReadOnly.Code, errcode.ErrNotFriends.Code, errcode.ErrMessageAccessDenied.Code)

	wsAfterDelID := uuid.NewString()
	wsAfterDelFrame := aliceWS.sendMsgAndAwaitAck(convID, wsAfterDelID, model.MessageTypeText, "ws send after unfriend", map[string]any{})
	if wsAfterDelFrame.Type != "error" {
		t.Fatalf("expected WS error sending after unfriend, got %s: %s", wsAfterDelFrame.Type, string(wsAfterDelFrame.Data))
	}
	wsAfterDelErr := decodeWSError(t, wsAfterDelFrame)
	if wsAfterDelErr.Code != errcode.ErrConversationReadOnly.Code &&
		wsAfterDelErr.Code != errcode.ErrNotFriends.Code &&
		wsAfterDelErr.Code != errcode.ErrWSConvAccessDenied.Code &&
		wsAfterDelErr.Code != errcode.ErrWSStateConflict.Code {
		t.Fatalf("expected 13003/12007/18005/18004 for WS send after unfriend, got %+v", wsAfterDelErr)
	}
	if n, err := countMessagesByClientID(t.Context(), rig.db, alice.ID, wsAfterDelID); err != nil || n != 0 {
		t.Fatalf("rejected WS send after unfriend persisted %d rows (err=%v)", n, err)
	}

	// 5. Removed or left group members must immediately lose read/download access to group attachments (16004).
	grpCreateSt, _, grpCreateEnv, _ := rig.doRequest(http.MethodPost, "/api/v1/groups", alice.AccessToken, map[string]any{
		"name":       "Attachment Boundary Group",
		"member_ids": []string{bob.IDStr, outsider.IDStr},
	})
	assertStage6OK(t, "create attachment boundary group", grpCreateSt, grpCreateEnv, http.StatusCreated)
	grp, grpConvID := extractGroupCreateData(t, grpCreateEnv.Data)
	rig.trackGroup(mustParseUint64(t, grp.ID))
	rig.trackConv(mustParseUint64(t, grpConvID))

	grpPDFBytes := makeStage6SamplePDF()
	grpUpSt, grpUpEnv, grpAsset, _ := rig.uploadTempFile(alice.AccessToken, model.AssetPurposeFile, "group-secret.pdf", "application/pdf", grpPDFBytes)
	assertStage6OK(t, "upload group boundary pdf", grpUpSt, grpUpEnv, http.StatusCreated)

	grpMsgSt, _, grpMsgEnv, _ := rig.doRequest(http.MethodPost, "/api/v1/conversations/"+grpConvID+"/messages", alice.AccessToken, map[string]any{
		"client_msg_id": uuid.NewString(),
		"msg_type":      model.MessageTypeFile,
		"content":       "[文件] group-secret.pdf",
		"extra":         map[string]any{"asset_id": grpAsset.AssetID},
	})
	assertStage6OK(t, "send group attachment message", grpMsgSt, grpMsgEnv, http.StatusCreated)

	rig.assertAssetAccessible(bob, grpAsset.AssetID, grpPDFBytes, "active group member bob")
	rig.assertAssetAccessible(outsider, grpAsset.AssetID, grpPDFBytes, "active group member outsider")

	remBobSt, _, remBobEnv, _ := rig.doRequest(http.MethodDelete, "/api/v1/groups/"+grp.ID+"/members/"+bob.IDStr, alice.AccessToken, nil)
	assertStage6OK(t, "remove bob from boundary group", remBobSt, remBobEnv, http.StatusOK)
	rig.assertAssetDenied(bob, grpAsset.AssetID, "removed group member bob")

	leaveOutSt, _, leaveOutEnv, _ := rig.doRequest(http.MethodPost, "/api/v1/groups/"+grp.ID+"/leave", outsider.AccessToken, nil)
	assertStage6OK(t, "outsider leave boundary group", leaveOutSt, leaveOutEnv, http.StatusOK)
	rig.assertAssetDenied(outsider, grpAsset.AssetID, "left group member outsider")
	rig.assertAssetAccessible(alice, grpAsset.AssetID, grpPDFBytes, "owner alice after member removal/leave")
}
