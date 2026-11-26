package model

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestModelsMatchFrozenSchema(t *testing.T) {
	tables := map[string]any{
		"users":                User{},
		"user_sessions":        UserSession{},
		"friend_requests":      FriendRequest{},
		"friendships":          Friendship{},
		"groups":               Group{},
		"group_members":        GroupMember{},
		"conversations":        Conversation{},
		"conversation_members": ConversationMember{},
		"messages":             Message{},
		"message_receipts":     MessageReceipt{},
		"assets":               Asset{},
		"audit_logs":           AuditLog{},
	}

	type tabler interface {
		TableName() string
	}
	if len(tables) != 12 {
		t.Fatalf("expected 12 tables, got %d", len(tables))
	}
	for expected, m := range tables {
		if m.(tabler).TableName() != expected {
			t.Fatalf("expected TableName %q, got %q", expected, m.(tabler).TableName())
		}
	}

	// messages.id must explicitly disable autoIncrement.
	msgIDField, ok := reflect.TypeOf(Message{}).FieldByName("ID")
	if !ok {
		t.Fatal("Message.ID missing")
	}
	if !strings.Contains(msgIDField.Tag.Get("gorm"), "autoIncrement:false") {
		t.Fatalf("Message.ID must specify autoIncrement:false, got %q", msgIDField.Tag.Get("gorm"))
	}
	var zeroMsg Message
	if err := zeroMsg.BeforeCreate(nil); !errors.Is(err, ErrMissingSnowflakeID) {
		t.Fatalf("expected ErrMissingSnowflakeID for zero message id, got %v", err)
	}

	// assets internal storage keys must never serialize to JSON.
	thumb := "thumbs/2026/01/thumb.webp"
	asset := Asset{
		AssetID:      "11111111-1111-4111-8111-111111111111",
		OwnerID:      1,
		Purpose:      AssetPurposeImage,
		Filename:     "photo.png",
		Mime:         "image/png",
		SizeBytes:    1024,
		StorageKey:   "uploads/2026/01/secret-disk-key.png",
		ThumbnailKey: &thumb,
		RefType:      AssetRefMessage,
		RefCount:     1,
	}
	rawAsset, err := json.Marshal(asset)
	if err != nil {
		t.Fatalf("marshal asset: %v", err)
	}
	s := string(rawAsset)
	if strings.Contains(s, "storage_key") || strings.Contains(s, "thumbnail_key") || strings.Contains(s, "secret-disk-key") {
		t.Fatalf("internal storage keys leaked in Asset JSON: %s", s)
	}
	if !asset.IsReferencedByMessage() {
		t.Fatal("expected IsReferencedByMessage to be true")
	}

	// audit_logs must be append-only without UpdatedAt or DeletedAt fields.
	auditType := reflect.TypeOf(AuditLog{})
	if _, hasUpdatedAt := auditType.FieldByName("UpdatedAt"); hasUpdatedAt {
		t.Fatal("AuditLog must not define UpdatedAt")
	}
	if _, hasDeletedAt := auditType.FieldByName("DeletedAt"); hasDeletedAt {
		t.Fatal("AuditLog must not define DeletedAt")
	}
}
