package claude

import (
	"encoding/json"
	"strings"
	"testing"

	"rumbo-claude/protocol"
)

func TestAToolResultShowsItsImagesAfterItsText(t *testing.T) {
	message := protocol.Message{Role: protocol.RoleUser, Content: []protocol.Block{{
		Type: protocol.BlockToolResult, ID: "toolu_1", Text: "[screenshot]",
		Images: []protocol.Image{{MediaType: "image/png", Data: []byte("png bytes")}},
	}}}

	converted, err := blocks(message)

	if err != nil {
		t.Fatal(err)
	}

	sent, _ := json.Marshal(converted)

	var parts []struct {
		Content []struct {
			Type   string `json:"type"`
			Text   string `json:"text"`
			Source struct {
				Type      string `json:"type"`
				MediaType string `json:"media_type"`
				Data      string `json:"data"`
			} `json:"source"`
		} `json:"content"`
	}

	if err := json.Unmarshal(sent, &parts); err != nil || len(parts) != 1 {
		t.Fatalf("sent %s", sent)
	}

	content := parts[0].Content

	if len(content) != 2 || content[0].Text != "[screenshot]" || content[1].Type != "image" {
		t.Fatalf("content = %s", sent)
	}

	if source := content[1].Source; source.Type != "base64" || source.MediaType != "image/png" || source.Data != "cG5nIGJ5dGVz" {
		t.Fatalf("image = %+v", source)
	}

	if strings.Contains(string(sent), "images") {
		t.Fatalf("the protocol's field reached the API: %s", sent)
	}
}

// A person's images come before their words, and images alone send no
// empty text, which the API refuses.
func TestAPersonsImagesComeBeforeTheirWords(t *testing.T) {
	image := protocol.Image{MediaType: "image/png", Data: []byte("png bytes")}

	for text, want := range map[string]string{
		"what is this?": `[{"source":{"data":"cG5nIGJ5dGVz","media_type":"image/png","type":"base64"},"type":"image"},{"text":"what is this?","type":"text"}]`,
		"":              `[{"source":{"data":"cG5nIGJ5dGVz","media_type":"image/png","type":"base64"},"type":"image"}]`,
	} {
		message := protocol.Message{Role: protocol.RoleUser, Content: []protocol.Block{{Type: protocol.BlockText, Text: text, Images: []protocol.Image{image}}}}
		converted, err := blocks(message)

		if err != nil {
			t.Fatal(err)
		}

		if sent, _ := json.Marshal(converted); string(sent) != want {
			t.Errorf("text %q sent %s\nwant %s", text, sent, want)
		}
	}
}
