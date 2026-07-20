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

func TestMessageEffectiveTextUsesAudioTranscription(t *testing.T) {
	message := Message{Raw: json.RawMessage(`{"body":{"attachments":[{"type":"audio","payload":{"url":"https://cdn.example.test/voice.ogg"},"transcription":"Поставь напоминание через час"}]}}`)}
	if got := message.EffectiveText(); got != "Поставь напоминание через час" {
		t.Fatalf("unexpected transcription: %q", got)
	}
}

func TestMessageTranscription(t *testing.T) {
	message := Message{Raw: json.RawMessage(`{"body":{"attachments":[{"type":"audio","transcription":"Тест"}]}}`)}
	if got := message.Transcription(); got != "Тест" {
		t.Fatalf("unexpected transcription: %q", got)
	}
}

func TestCallbackFields(t *testing.T) {
	var update Update
	if err := json.Unmarshal([]byte(`{"update_type":"message_callback","callback":{"callback_id":"cb-1","payload":"ui:tasks","user":{"user_id":42}},"message":{"sender":{"user_id":377029406},"body":{"attachments":[{"type":"inline_keyboard","payload":{"buttons":[[{"type":"callback","text":"Other","payload":"ui:other"}]]}}]}}}`), &update); err != nil {
		t.Fatal(err)
	}
	if update.CallbackID() != "cb-1" || update.CallbackPayload() != "ui:tasks" {
		t.Fatalf("unexpected callback: %q %q", update.CallbackID(), update.CallbackPayload())
	}
	if update.EffectiveUserID() != 42 {
		t.Fatalf("unexpected callback user: %d", update.EffectiveUserID())
	}
}
