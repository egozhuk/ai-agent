package maxapi

import (
	"encoding/json"
	"testing"
)

func TestMessageHasAttachments(t *testing.T) {
	message := Message{Raw: json.RawMessage(`{"body":{"attachments":[{"type":"file","payload":{"token":"file-token"}}]}}`)}
	if !message.HasAttachments() {
		t.Fatal("HasAttachments() = false")
	}
}

func TestDocumentAttachment(t *testing.T) {
	message := Message{Raw: json.RawMessage(`{"body":{"attachments":[{"type":"file","payload":{"url":"https://cdn.example.test/report.pdf","filename":"report.pdf","mime_type":"application/pdf"}}]}}`)}
	documents := message.DocumentAttachments()
	if len(documents) != 1 || documents[0].URL != "https://cdn.example.test/report.pdf" || documents[0].Name != "report.pdf" {
		t.Fatalf("unexpected documents: %#v", documents)
	}
}

func TestLinkPreviewIsNotDocument(t *testing.T) {
	message := Message{Raw: json.RawMessage(`{"body":{"attachments":[{"type":"link","payload":{"url":"https://github.com/example/project"}}]}}`)}
	if documents := message.DocumentAttachments(); len(documents) != 0 {
		t.Fatalf("link preview was classified as document: %#v", documents)
	}
}
