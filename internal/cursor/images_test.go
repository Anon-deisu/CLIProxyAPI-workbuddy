package cursor

import (
	"encoding/base64"
	"testing"

	agentv1 "github.com/router-for-me/CLIProxyAPI/v8/internal/cursor/proto/agent/v1"
	"google.golang.org/protobuf/encoding/protowire"
)

var pngBytes = []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 0x01, 0x02}

func TestDecodeInlineImageAnthropicBlock(t *testing.T) {
	img, ok := DecodeInlineImage("image/png", base64.StdEncoding.EncodeToString(pngBytes))
	if !ok {
		t.Fatal("anthropic image block should decode")
	}
	if img.MimeType != "image/png" || string(img.Data) != string(pngBytes) {
		t.Fatalf("decoded wrong: %q %v", img.MimeType, img.Data)
	}
}

func TestDecodeInlineImageDataURL(t *testing.T) {
	url := "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(pngBytes)
	img, ok := DecodeInlineImage("", url)
	if !ok {
		t.Fatal("data URL should decode")
	}
	if img.MimeType != "image/jpeg" {
		t.Fatalf("media type = %q", img.MimeType)
	}
	if string(img.Data) != string(pngBytes) {
		t.Fatal("payload mismatch")
	}
}

func TestDecodeInlineImageRejectsGarbage(t *testing.T) {
	if _, ok := DecodeInlineImage("image/png", "not base64 !!!"); ok {
		t.Fatal("invalid base64 must be rejected")
	}
	if _, ok := DecodeInlineImage("image/png", ""); ok {
		t.Fatal("empty payload must be rejected")
	}
	if _, ok := DecodeInlineImage("", "https://example.com/a.png"); ok {
		t.Fatal("remote URLs cannot be inlined")
	}
}

func TestAttachImagesEncodesSelectedContext(t *testing.T) {
	msg := &agentv1.UserMessage{Text: "what is in this picture?"}
	attachImages(msg, []ChatImage{{MimeType: "image/png", Data: pngBytes}})

	raw := msg.ProtoReflect().GetUnknown()
	if len(raw) == 0 {
		t.Fatal("selected_context was not attached")
	}
	num, typ, n := protowire.ConsumeTag(raw)
	if n < 0 || num != fieldUserMessageSelectedContext || typ != protowire.BytesType {
		t.Fatalf("unexpected outer field num=%d typ=%d", num, typ)
	}
	sc, n2 := protowire.ConsumeBytes(raw[n:])
	if n2 < 0 {
		t.Fatal("selected_context payload unreadable")
	}
	inner, _, n3 := protowire.ConsumeTag(sc)
	if n3 < 0 || inner != fieldSelectedContextImages {
		t.Fatalf("expected selected_images, got field %d", inner)
	}
	// The raw bytes must survive verbatim.
	if !containsBytes(sc, pngBytes) {
		t.Fatal("image payload missing from encoded context")
	}
}

func TestAttachImagesNoopWithoutImages(t *testing.T) {
	msg := &agentv1.UserMessage{Text: "hi"}
	attachImages(msg, nil)
	if len(msg.ProtoReflect().GetUnknown()) != 0 {
		t.Fatal("no images means no unknown fields")
	}
}

func containsBytes(haystack, needle []byte) bool {
	if len(needle) == 0 || len(haystack) < len(needle) {
		return false
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		match := true
		for j := range needle {
			if haystack[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
