package tests

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"enterprise-im/backend/internal/model"
	"enterprise-im/backend/internal/pkg/errcode"
)

func TestStage6GroupLifecyclePermissionsAndBroadcast(t *testing.T) {
	rig := newStage6Rig(t)
	expectDBAvailable(t, rig.db)

	alice := rig.registerAndLogin("s6_grp_owner", "Alice Owner")
	bob := rig.registerAndLogin("s6_grp_admin", "Bob Admin")
	charlie := rig.registerAndLogin("s6_grp_mem", "Charlie Member")
	dave := rig.registerAndLogin("s6_grp_out", "Dave Outsider")

	// 1. Invalid group creation without at least 1 other member must fail.
	badGrpSt, _, badGrpEnv, _ := rig.doRequest(http.MethodPost, "/api/v1/groups", alice.AccessToken, map[string]any{
		"name":       "Empty Group",
		"member_ids": []string{alice.IDStr},
	})
	assertStage6BizErr(t, "create group with self only", badGrpSt, badGrpEnv, errcode.ErrInvalidParams.Code, errcode.ErrInvalidValue.Code)

	// 2. Owner Alice creates a group with Bob and Charlie (total 3 members).
	createSt, _, createEnv, _ := rig.doRequest(http.MethodPost, "/api/v1/groups", alice.AccessToken, map[string]any{
		"name":         "Stage 6 QA Group",
		"member_ids":   []string{bob.IDStr, charlie.IDStr},
		"announcement": "Initial rules",
	})
	assertStage6OK(t, "create group", createSt, createEnv, http.StatusCreated)
	grp, convID := extractGroupCreateData(t, createEnv.Data)
	if grp.ID == "" || convID == "" || grp.OwnerID != alice.IDStr || grp.Status != model.GroupStatusActive || grp.MemberCount != 3 {
		t.Fatalf("unexpected created group payload: grp=%+v convID=%q raw=%s", grp, convID, string(createEnv.Data))
	}
	rig.trackGroup(mustParseUint64(t, grp.ID))
	rig.trackConv(mustParseUint64(t, convID))

	// Creation system message must be present in conversation history.
	initHistSt, _, initHistEnv, _ := rig.doRequest(http.MethodGet, "/api/v1/conversations/"+convID+"/messages", alice.AccessToken, nil)
	assertStage6OK(t, "group initial history", initHistSt, initHistEnv, http.StatusOK)
	initHist := decodeStage6JSON[stage6MessagePageDTO](t, initHistEnv.Data)
	if len(initHist.Items) == 0 || initHist.Items[0].MsgType != model.MessageTypeSystem {
		t.Fatalf("expected group creation system message in history, got %+v", initHist.Items)
	}

	// Members see group in /api/v1/groups and /api/v1/conversations; outsider Dave does not.
	for _, member := range []stage6User{alice, bob, charlie} {
		gListSt, _, gListEnv, _ := rig.doRequest(http.MethodGet, "/api/v1/groups", member.AccessToken, nil)
		assertStage6OK(t, "list groups for "+member.Username, gListSt, gListEnv, http.StatusOK)
		var gPage struct {
			Items []stage6GroupDTO `json:"items"`
		}
		gPage = decodeStage6JSON[struct {
			Items []stage6GroupDTO `json:"items"`
		}](t, gListEnv.Data)
		foundGrp := false
		for _, item := range gPage.Items {
			if item.ID == grp.ID && item.ConversationID == convID {
				foundGrp = true
				break
			}
		}
		if !foundGrp {
			t.Fatalf("member %s missing group %s in /api/v1/groups: %s", member.Username, grp.ID, string(gListEnv.Data))
		}
	}

	// 3. Non-member Dave is blocked with 15002 / 13002 / 18005 across group and conversation endpoints.
	daveWS := rig.dialWS(dave)
	for _, tc := range []struct {
		name   string
		method string
		path   string
		body   any
		codes  []int
	}{
		{"get group", http.MethodGet, "/api/v1/groups/" + grp.ID, nil, []int{errcode.ErrNotGroupMember.Code}},
		{"get members", http.MethodGet, "/api/v1/groups/" + grp.ID + "/members", nil, []int{errcode.ErrNotGroupMember.Code}},
		{"patch group", http.MethodPatch, "/api/v1/groups/" + grp.ID, map[string]string{"name": "Hack"}, []int{errcode.ErrNotGroupMember.Code}},
		{"invite member", http.MethodPost, "/api/v1/groups/" + grp.ID + "/members", map[string]any{"user_ids": []string{dave.IDStr}}, []int{errcode.ErrNotGroupMember.Code}},
		{"remove member", http.MethodDelete, "/api/v1/groups/" + grp.ID + "/members/" + charlie.IDStr, nil, []int{errcode.ErrNotGroupMember.Code}},
		{"leave group", http.MethodPost, "/api/v1/groups/" + grp.ID + "/leave", nil, []int{errcode.ErrNotGroupMember.Code}},
		{"read history", http.MethodGet, "/api/v1/conversations/" + convID + "/messages", nil, []int{errcode.ErrNotConversationMember.Code, errcode.ErrNotGroupMember.Code}},
		{"http send", http.MethodPost, "/api/v1/conversations/" + convID + "/messages", map[string]any{"client_msg_id": uuid.NewString(), "msg_type": "text", "content": "intrude", "extra": map[string]any{}}, []int{errcode.ErrNotConversationMember.Code, errcode.ErrNotGroupMember.Code}},
	} {
		st, _, env, _ := rig.doRequest(tc.method, tc.path, dave.AccessToken, tc.body)
		assertStage6BizErr(t, "non-member "+tc.name, st, env, tc.codes...)
	}
	daveErrAck := daveWS.sendMsgAndAwaitAck(convID, uuid.NewString(), model.MessageTypeText, "ws intrude", map[string]any{})
	if daveErrAck.Type != "error" {
		t.Fatalf("expected WS error for non-member msg.send, got %s: %s", daveErrAck.Type, string(daveErrAck.Data))
	}
	daveErr := decodeWSError(t, daveErrAck)
	if daveErr.Code != errcode.ErrWSConvAccessDenied.Code && daveErr.Code != errcode.ErrNotConversationMember.Code && daveErr.Code != errcode.ErrNotGroupMember.Code {
		t.Fatalf("expected 18005/13002/15002 for non-member WS msg.send, got %+v", daveErr)
	}

	// 4. Ordinary member Charlie can update own group nickname, but receives 15003 on management operations.
	nickSt, _, nickEnv, _ := rig.doRequest(http.MethodPatch, "/api/v1/groups/"+grp.ID+"/members/me", charlie.AccessToken, map[string]string{
		"nickname_in_group": "QA-Charlie",
	})
	assertStage6OK(t, "member update nickname_in_group", nickSt, nickEnv, http.StatusOK)

	for _, tc := range []struct {
		name   string
		method string
		path   string
		body   any
	}{
		{"patch group", http.MethodPatch, "/api/v1/groups/" + grp.ID, map[string]string{"name": "Charlie Rename"}},
		{"invite member", http.MethodPost, "/api/v1/groups/" + grp.ID + "/members", map[string]any{"user_ids": []string{dave.IDStr}}},
		{"remove member", http.MethodDelete, "/api/v1/groups/" + grp.ID + "/members/" + bob.IDStr, nil},
		{"change role", http.MethodPatch, "/api/v1/groups/" + grp.ID + "/members/" + bob.IDStr + "/role", map[string]string{"role": model.GroupRoleAdmin}},
		{"transfer owner", http.MethodPost, "/api/v1/groups/" + grp.ID + "/transfer", map[string]string{"target_user_id": charlie.IDStr}},
		{"disband group", http.MethodDelete, "/api/v1/groups/" + grp.ID, nil},
		{"view audit", http.MethodGet, "/api/v1/groups/" + grp.ID + "/audit", nil},
	} {
		st, _, env, _ := rig.doRequest(tc.method, tc.path, charlie.AccessToken, tc.body)
		assertStage6BizErr(t, "member role denied "+tc.name, st, env, errcode.ErrGroupRoleDenied.Code)
	}

	// 5. Role management: Alice promotes Bob to admin; owner cannot be demoted (15009); admin still cannot disband/transfer (15003).
	promSt, _, promEnv, _ := rig.doRequest(http.MethodPatch, "/api/v1/groups/"+grp.ID+"/members/"+bob.IDStr+"/role", alice.AccessToken, map[string]string{
		"role": model.GroupRoleAdmin,
	})
	assertStage6OK(t, "promote bob to admin", promSt, promEnv, http.StatusOK)

	demoteOwnerSt, _, demoteOwnerEnv, _ := rig.doRequest(http.MethodPatch, "/api/v1/groups/"+grp.ID+"/members/"+alice.IDStr+"/role", alice.AccessToken, map[string]string{
		"role": model.GroupRoleMember,
	})
	assertStage6BizErr(t, "demote group owner", demoteOwnerSt, demoteOwnerEnv, errcode.ErrCannotDemoteOwner.Code)

	adminDisbandSt, _, adminDisbandEnv, _ := rig.doRequest(http.MethodDelete, "/api/v1/groups/"+grp.ID, bob.AccessToken, nil)
	assertStage6BizErr(t, "admin cannot disband group", adminDisbandSt, adminDisbandEnv, errcode.ErrGroupRoleDenied.Code)

	adminTransferSt, _, adminTransferEnv, _ := rig.doRequest(http.MethodPost, "/api/v1/groups/"+grp.ID+"/transfer", bob.AccessToken, map[string]string{
		"target_user_id": charlie.IDStr,
	})
	assertStage6BizErr(t, "admin cannot transfer ownership", adminTransferSt, adminTransferEnv, errcode.ErrGroupRoleDenied.Code)

	// 6. Admin Bob updates group name & announcement, invites Dave, reads audit log; duplicate invite returns 15006.
	updSt, _, updEnv, _ := rig.doRequest(http.MethodPatch, "/api/v1/groups/"+grp.ID, bob.AccessToken, map[string]string{
		"name":         "Stage 6 QA Renamed",
		"announcement": "Release check in progress",
	})
	assertStage6OK(t, "admin patch group name & announcement", updSt, updEnv, http.StatusOK)
	updGrp := decodeWrappedOrFlat[stage6GroupDTO](t, updEnv.Data, "group")
	if updGrp.Name != "Stage 6 QA Renamed" || updGrp.Announcement == nil || *updGrp.Announcement != "Release check in progress" {
		t.Fatalf("unexpected updated group: %+v raw=%s", updGrp, string(updEnv.Data))
	}

	invSt, _, invEnv, _ := rig.doRequest(http.MethodPost, "/api/v1/groups/"+grp.ID+"/members", bob.AccessToken, map[string]any{
		"user_ids": []string{dave.IDStr},
	})
	assertStage6OK(t, "admin invite dave", invSt, invEnv, http.StatusOK, http.StatusCreated)

	dupInvSt, _, dupInvEnv, _ := rig.doRequest(http.MethodPost, "/api/v1/groups/"+grp.ID+"/members", bob.AccessToken, map[string]any{
		"user_ids": []string{dave.IDStr},
	})
	assertStage6BizErr(t, "invite existing member", dupInvSt, dupInvEnv, errcode.ErrAlreadyGroupMember.Code)

	auditSt, _, auditEnv, _ := rig.doRequest(http.MethodGet, "/api/v1/groups/"+grp.ID+"/audit", bob.AccessToken, nil)
	assertStage6OK(t, "admin get group audit", auditSt, auditEnv, http.StatusOK)

	// Verify group members list reflects roles, group nickname, and all 4 active members.
	gmSt, _, gmEnv, _ := rig.doRequest(http.MethodGet, "/api/v1/groups/"+grp.ID+"/members?limit=20", alice.AccessToken, nil)
	assertStage6OK(t, "list group members", gmSt, gmEnv, http.StatusOK)
	var gmPage struct {
		Items []stage6GroupMemberDTO `json:"items"`
	}
	gmPage = decodeStage6JSON[struct {
		Items []stage6GroupMemberDTO `json:"items"`
	}](t, gmEnv.Data)
	roleByUser := make(map[string]stage6GroupMemberDTO, len(gmPage.Items))
	for _, m := range gmPage.Items {
		roleByUser[m.UserID] = m
	}
	if len(roleByUser) != 4 ||
		roleByUser[alice.IDStr].Role != model.GroupRoleOwner ||
		roleByUser[bob.IDStr].Role != model.GroupRoleAdmin ||
		roleByUser[charlie.IDStr].Role != model.GroupRoleMember ||
		roleByUser[charlie.IDStr].NicknameInGroup != "QA-Charlie" ||
		roleByUser[dave.IDStr].Role != model.GroupRoleMember {
		t.Fatalf("unexpected group members list: %+v", gmPage.Items)
	}

	// 7. Real-time group chat broadcast: Alice, Bob, Charlie, Dave online -> Alice sends -> all peers receive msg.push.
	aliceWS := rig.dialWS(alice)
	bobWS := rig.dialWS(bob)
	charlieWS := rig.dialWS(charlie)

	grpMsg1Content := "group broadcast to all 4 active members"
	grpMsg1Ack := aliceWS.sendMsgAndAwaitAck(convID, uuid.NewString(), model.MessageTypeText, grpMsg1Content, map[string]any{})
	if grpMsg1Ack.Type != "msg.ack" {
		t.Fatalf("expected msg.ack for group message, got %s: %s", grpMsg1Ack.Type, string(grpMsg1Ack.Data))
	}
	grpMsg1ID := decodeStage6JSON[struct {
		MsgID string `json:"msg_id"`
	}](t, grpMsg1Ack.Data).MsgID

	for _, peer := range []struct {
		name string
		ws   *stage6WSClient
	}{
		{"bob", bobWS},
		{"charlie", charlieWS},
		{"dave", daveWS},
	} {
		peer.ws.awaitFrame(peer.name+" group msg.push 1", stage6IOTimeout, func(f stage6WSFrame) bool {
			if f.Type != "msg.push" {
				return false
			}
			pt, m := decodeMsgPushMessage(t, f)
			return pt == "message" && m.MsgID == grpMsg1ID && m.Content == grpMsg1Content
		})
	}

	// 8. Remove Charlie -> removing non-existent returns 15007; Charlie immediately blocked from reading/sending
	//    and receives NO new msg.push broadcast when Alice sends a subsequent message.
	remSt, _, remEnv, _ := rig.doRequest(http.MethodDelete, "/api/v1/groups/"+grp.ID+"/members/"+charlie.IDStr, bob.AccessToken, nil)
	assertStage6OK(t, "admin remove charlie", remSt, remEnv, http.StatusOK)

	remAgainSt, _, remAgainEnv, _ := rig.doRequest(http.MethodDelete, "/api/v1/groups/"+grp.ID+"/members/"+charlie.IDStr, bob.AccessToken, nil)
	assertStage6BizErr(t, "remove already removed member", remAgainSt, remAgainEnv, errcode.ErrGroupMemberNotFound.Code)

	charlieHistSt, _, charlieHistEnv, _ := rig.doRequest(http.MethodGet, "/api/v1/conversations/"+convID+"/messages", charlie.AccessToken, nil)
	assertStage6BizErr(t, "removed member read history", charlieHistSt, charlieHistEnv, errcode.ErrNotConversationMember.Code, errcode.ErrNotGroupMember.Code)

	charlieSendSt, _, charlieSendEnv, _ := rig.doRequest(http.MethodPost, "/api/v1/conversations/"+convID+"/messages", charlie.AccessToken, map[string]any{
		"client_msg_id": uuid.NewString(),
		"msg_type":      model.MessageTypeText,
		"content":       "post-removal http send",
		"extra":         map[string]any{},
	})
	assertStage6BizErr(t, "removed member HTTP send", charlieSendSt, charlieSendEnv, errcode.ErrNotConversationMember.Code, errcode.ErrNotGroupMember.Code)

	charlieWSErrFrame := charlieWS.sendMsgAndAwaitAck(convID, uuid.NewString(), model.MessageTypeText, "post-removal ws send", map[string]any{})
	if charlieWSErrFrame.Type != "error" {
		t.Fatalf("expected WS error for removed member msg.send, got %s: %s", charlieWSErrFrame.Type, string(charlieWSErrFrame.Data))
	}
	charlieWSErr := decodeWSError(t, charlieWSErrFrame)
	if charlieWSErr.Code != errcode.ErrWSConvAccessDenied.Code && charlieWSErr.Code != errcode.ErrNotConversationMember.Code && charlieWSErr.Code != errcode.ErrNotGroupMember.Code {
		t.Fatalf("expected 18005/13002/15002 for removed member WS send, got %+v", charlieWSErr)
	}

	grpMsg2Content := "post-removal broadcast only to remaining members"
	grpMsg2Ack := aliceWS.sendMsgAndAwaitAck(convID, uuid.NewString(), model.MessageTypeText, grpMsg2Content, map[string]any{})
	if grpMsg2Ack.Type != "msg.ack" {
		t.Fatalf("expected msg.ack for group message 2, got %s: %s", grpMsg2Ack.Type, string(grpMsg2Ack.Data))
	}
	grpMsg2ID := decodeStage6JSON[struct {
		MsgID string `json:"msg_id"`
	}](t, grpMsg2Ack.Data).MsgID

	bobWS.awaitFrame("bob group msg.push 2", stage6IOTimeout, func(f stage6WSFrame) bool {
		if f.Type != "msg.push" {
			return false
		}
		pt, m := decodeMsgPushMessage(t, f)
		return pt == "message" && m.MsgID == grpMsg2ID
	})
	daveWS.awaitFrame("dave group msg.push 2", stage6IOTimeout, func(f stage6WSFrame) bool {
		if f.Type != "msg.push" {
			return false
		}
		pt, m := decodeMsgPushMessage(t, f)
		return pt == "message" && m.MsgID == grpMsg2ID
	})
	charlieWS.expectNoFrame("removed charlie must not receive post-removal msg.push", stage6QuietTimeout, func(f stage6WSFrame) bool {
		if f.Type != "msg.push" {
			return false
		}
		_, m := decodeMsgPushMessage(t, f)
		return m.MsgID == grpMsg2ID || m.Content == grpMsg2Content
	})

	// 9. Member leave & owner leave prohibition (15008).
	daveLeaveSt, _, daveLeaveEnv, _ := rig.doRequest(http.MethodPost, "/api/v1/groups/"+grp.ID+"/leave", dave.AccessToken, nil)
	assertStage6OK(t, "member dave leave group", daveLeaveSt, daveLeaveEnv, http.StatusOK)

	daveAfterLeaveSt, _, daveAfterLeaveEnv, _ := rig.doRequest(http.MethodGet, "/api/v1/groups/"+grp.ID, dave.AccessToken, nil)
	assertStage6BizErr(t, "left member get group", daveAfterLeaveSt, daveAfterLeaveEnv, errcode.ErrNotGroupMember.Code)

	ownerLeaveSt, _, ownerLeaveEnv, _ := rig.doRequest(http.MethodPost, "/api/v1/groups/"+grp.ID+"/leave", alice.AccessToken, nil)
	assertStage6BizErr(t, "owner leave before transfer/disband", ownerLeaveSt, ownerLeaveEnv, errcode.ErrOwnerLeaveForbidden.Code)

	// 10. Transfer ownership: ineligible target -> 15010/15007; valid transfer to Bob -> Alice can leave.
	badTransferSt, _, badTransferEnv, _ := rig.doRequest(http.MethodPost, "/api/v1/groups/"+grp.ID+"/transfer", alice.AccessToken, map[string]string{
		"target_user_id": dave.IDStr,
	})
	assertStage6BizErr(t, "transfer to non-member", badTransferSt, badTransferEnv, errcode.ErrTransferIneligible.Code, errcode.ErrGroupMemberNotFound.Code)

	trSt, _, trEnv, _ := rig.doRequest(http.MethodPost, "/api/v1/groups/"+grp.ID+"/transfer", alice.AccessToken, map[string]string{
		"target_user_id": bob.IDStr,
	})
	assertStage6OK(t, "transfer ownership to bob", trSt, trEnv, http.StatusOK)

	getAfterTrSt, _, getAfterTrEnv, _ := rig.doRequest(http.MethodGet, "/api/v1/groups/"+grp.ID, bob.AccessToken, nil)
	assertStage6OK(t, "get group after transfer", getAfterTrSt, getAfterTrEnv, http.StatusOK)
	afterTrGrp := decodeWrappedOrFlat[stage6GroupDTO](t, getAfterTrEnv.Data, "group")
	if afterTrGrp.OwnerID != bob.IDStr {
		t.Fatalf("expected owner_id=%s after transfer, got %s", bob.IDStr, afterTrGrp.OwnerID)
	}

	// Old owner Alice can no longer disband (15003), and can now leave (200).
	aliceDisbandSt, _, aliceDisbandEnv, _ := rig.doRequest(http.MethodDelete, "/api/v1/groups/"+grp.ID, alice.AccessToken, nil)
	assertStage6BizErr(t, "former owner disband", aliceDisbandSt, aliceDisbandEnv, errcode.ErrGroupRoleDenied.Code)

	aliceLeaveSt, _, aliceLeaveEnv, _ := rig.doRequest(http.MethodPost, "/api/v1/groups/"+grp.ID+"/leave", alice.AccessToken, nil)
	assertStage6OK(t, "former owner leave after transfer", aliceLeaveSt, aliceLeaveEnv, http.StatusOK)

	// 11. Re-invite Charlie, then new owner Bob disbands group -> 15004 on subsequent ops, read-only history preserved!
	reInvSt, _, reInvEnv, _ := rig.doRequest(http.MethodPost, "/api/v1/groups/"+grp.ID+"/members", bob.AccessToken, map[string]any{
		"user_ids": []string{charlie.IDStr},
	})
	assertStage6OK(t, "new owner re-invite charlie", reInvSt, reInvEnv, http.StatusOK, http.StatusCreated)

	preDisbandSt, _, preDisbandEnv, _ := rig.doRequest(http.MethodGet, "/api/v1/conversations/"+convID+"/messages?limit=50", bob.AccessToken, nil)
	assertStage6OK(t, "pre-disband group history", preDisbandSt, preDisbandEnv, http.StatusOK)
	preDisband := decodeStage6JSON[stage6MessagePageDTO](t, preDisbandEnv.Data)
	var preDisbandMaxSeq uint64
	if len(preDisband.Items) > 0 {
		preDisbandMaxSeq = preDisband.Items[len(preDisband.Items)-1].seqUint64(t)
	}

	disbandSt, _, disbandEnv, _ := rig.doRequest(http.MethodDelete, "/api/v1/groups/"+grp.ID, bob.AccessToken, nil)
	assertStage6OK(t, "new owner disband group", disbandSt, disbandEnv, http.StatusOK)

	charlieWS.awaitFrame("charlie group.disbanded notice or msg.push", stage6IOTimeout, func(f stage6WSFrame) bool {
		if f.Type == "system.notice" {
			var n struct {
				Event string `json:"event"`
			}
			return json.Unmarshal(f.Data, &n) == nil && n.Event == "group.disbanded"
		}
		if f.Type == "msg.push" {
			pt, m := decodeMsgPushMessage(t, f)
			return pt == "message" && m.MsgType == model.MessageTypeSystem && m.seqUint64(t) > preDisbandMaxSeq
		}
		return false
	})

	// Operations on disbanded group must reject with 15004 (or 13003 on conversation send).
	disPatchSt, _, disPatchEnv, _ := rig.doRequest(http.MethodPatch, "/api/v1/groups/"+grp.ID, bob.AccessToken, map[string]string{"name": "Ghost"})
	assertStage6BizErr(t, "patch disbanded group", disPatchSt, disPatchEnv, errcode.ErrGroupDisbanded.Code)

	disInvSt, _, disInvEnv, _ := rig.doRequest(http.MethodPost, "/api/v1/groups/"+grp.ID+"/members", bob.AccessToken, map[string]any{"user_ids": []string{dave.IDStr}})
	assertStage6BizErr(t, "invite into disbanded group", disInvSt, disInvEnv, errcode.ErrGroupDisbanded.Code)

	disRepeatSt, _, disRepeatEnv, _ := rig.doRequest(http.MethodDelete, "/api/v1/groups/"+grp.ID, bob.AccessToken, nil)
	assertStage6BizErr(t, "repeat disband", disRepeatSt, disRepeatEnv, errcode.ErrGroupDisbanded.Code)

	disSendSt, _, disSendEnv, _ := rig.doRequest(http.MethodPost, "/api/v1/conversations/"+convID+"/messages", bob.AccessToken, map[string]any{
		"client_msg_id": uuid.NewString(),
		"msg_type":      model.MessageTypeText,
		"content":       "send to disbanded",
		"extra":         map[string]any{},
	})
	assertStage6BizErr(t, "HTTP send to disbanded group", disSendSt, disSendEnv, errcode.ErrGroupDisbanded.Code, errcode.ErrConversationReadOnly.Code)

	disWSSend := bobWS.sendMsgAndAwaitAck(convID, uuid.NewString(), model.MessageTypeText, "ws send to disbanded", map[string]any{})
	if disWSSend.Type != "error" {
		t.Fatalf("expected WS error sending to disbanded group, got %s: %s", disWSSend.Type, string(disWSSend.Data))
	}
	disWSErr := decodeWSError(t, disWSSend)
	if disWSErr.Code != errcode.ErrGroupDisbanded.Code && disWSErr.Code != errcode.ErrConversationReadOnly.Code && disWSErr.Code != errcode.ErrWSStateConflict.Code {
		t.Fatalf("expected 15004/13003/18004 sending to disbanded group over WS, got %+v", disWSErr)
	}

	// Active members at disband time can still read history including final disband system message.
	finalHistSt, _, finalHistEnv, _ := rig.doRequest(http.MethodGet, "/api/v1/conversations/"+convID+"/messages?limit=50", bob.AccessToken, nil)
	assertStage6OK(t, "read disbanded group history", finalHistSt, finalHistEnv, http.StatusOK)
	finalHist := decodeStage6JSON[stage6MessagePageDTO](t, finalHistEnv.Data)
	verifySeqStrictlyIncreasing(t, finalHist.Items)
	if len(finalHist.Items) == 0 || finalHist.Items[len(finalHist.Items)-1].MsgType != model.MessageTypeSystem {
		t.Fatalf("expected final disband system message in disbanded group history, got %+v", finalHist.Items)
	}
}
