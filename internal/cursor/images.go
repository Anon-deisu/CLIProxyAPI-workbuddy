package cursor

import (
	"encoding/base64"
	"strings"

	"github.com/google/uuid"
	agentv1 "github.com/router-for-me/CLIProxyAPI/v8/internal/cursor/proto/agent/v1"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// ChatImage is one inline image attached to a user turn.
type ChatImage struct {
	MimeType string
	Data     []byte
}

// Field numbers from the full Cursor Agent schema (proto_src/agent/v1/agent.proto).
// The compiled MVP schema omits them, so the bytes are appended as unknown
// fields instead of regenerating the whole descriptor set.
const (
	fieldUserMessageSelectedContext = 3 // UserMessage.selected_context
	fieldSelectedContextImages      = 1 // SelectedContext.selected_images
	fieldSelectedImageUUID          = 2 // SelectedImage.uuid
	fieldSelectedImageMimeType      = 7 // SelectedImage.mime_type
	fieldSelectedImageData          = 8 // SelectedImage.data
)

// DecodeInlineImage parses the two shapes clients send: an Anthropic
// image block source and an OpenAI data URL.
func DecodeInlineImage(mediaType, base64Data string) (ChatImage, bool) {
	base64Data = strings.TrimSpace(base64Data)
	if base64Data == "" {
		return ChatImage{}, false
	}
	if strings.HasPrefix(base64Data, "data:") {
		semi := strings.Index(base64Data, ";")
		comma := strings.Index(base64Data, ",")
		if semi < 0 || comma < 0 || comma < semi {
			return ChatImage{}, false
		}
		mediaType = strings.TrimPrefix(base64Data[:semi], "data:")
		base64Data = base64Data[comma+1:]
	}
	raw, err := base64.StdEncoding.DecodeString(base64Data)
	if err != nil || len(raw) == 0 {
		return ChatImage{}, false
	}
	if strings.TrimSpace(mediaType) == "" {
		mediaType = "image/png"
	}
	return ChatImage{MimeType: mediaType, Data: raw}, true
}

// collectImages gathers every inline image from the active turn's messages.
func collectImages(messages []ChatMessage) []ChatImage {
	var out []ChatImage
	for _, msg := range messages {
		if msg.Role != "user" {
			continue
		}
		out = append(out, msg.Images...)
	}
	return out
}

// attachImages puts inline images on a UserMessage using the selected_context
// shape Cursor's desktop client uses.
func attachImages(msg *agentv1.UserMessage, images []ChatImage) {
	if msg == nil || len(images) == 0 {
		return
	}
	var selectedContext []byte
	for _, img := range images {
		if len(img.Data) == 0 {
			continue
		}
		var image []byte
		image = protowire.AppendTag(image, fieldSelectedImageUUID, protowire.BytesType)
		image = protowire.AppendString(image, uuid.NewString())
		image = protowire.AppendTag(image, fieldSelectedImageMimeType, protowire.BytesType)
		image = protowire.AppendString(image, img.MimeType)
		image = protowire.AppendTag(image, fieldSelectedImageData, protowire.BytesType)
		image = protowire.AppendBytes(image, img.Data)

		selectedContext = protowire.AppendTag(selectedContext, fieldSelectedContextImages, protowire.BytesType)
		selectedContext = protowire.AppendBytes(selectedContext, image)
	}
	if len(selectedContext) == 0 {
		return
	}
	var unknown []byte
	unknown = protowire.AppendTag(unknown, fieldUserMessageSelectedContext, protowire.BytesType)
	unknown = protowire.AppendBytes(unknown, selectedContext)

	reflected := msg.ProtoReflect()
	unknown = append(unknown, reflected.GetUnknown()...)
	reflected.SetUnknown(protoreflect.RawFields(unknown))
}
