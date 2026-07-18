package store

import (
	"path/filepath"
	"testing"
	"time"

	"AI-agent/internal/mistral"
)

func TestSQLiteStoreRoundTrip(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	const userID int64 = 42
	if err := st.AppendMessages(userID, mistral.Message{Role: "user", Content: "Привет"}); err != nil {
		t.Fatal(err)
	}
	if got := st.History(userID); len(got) != 1 || got[0].Content != "Привет" {
		t.Fatalf("unexpected history: %#v", got)
	}
	if err := st.AddUsage(userID, mistral.Usage{PromptTokens: 2, CompletionTokens: 3, TotalTokens: 5}); err != nil {
		t.Fatal(err)
	}
	if got := st.Usage(userID); got.TotalTokens != 5 {
		t.Fatalf("unexpected usage: %#v", got)
	}

	task := Task{ID: "task-1", UserID: userID, Kind: "reminder", Text: "Проверка", NextRunAt: time.Now().UTC().Add(-time.Minute), Active: true, CreatedAt: time.Now().UTC()}
	if err := st.SaveTask(task); err != nil {
		t.Fatal(err)
	}
	if got := st.DueTasks(time.Now().UTC()); len(got) != 1 || got[0].ID != task.ID {
		t.Fatalf("unexpected due tasks: %#v", got)
	}

	account := Account{Provider: "google", ConnectedAt: time.Now().UTC(), Scopes: []string{"mail"}, EncryptedAccessToken: "encrypted"}
	if err := st.SaveAccount(userID, account); err != nil {
		t.Fatal(err)
	}
	if got, ok := st.Account(userID, "google"); !ok || got.EncryptedAccessToken != account.EncryptedAccessToken {
		t.Fatalf("unexpected account: %#v, %v", got, ok)
	}
}

func TestActiveDocumentIsIsolatedByUser(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.SaveDocument(Document{ID: "doc-1", UserID: 11, Name: "one.pdf", URL: "https://example.test/one.pdf"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := st.ActiveDocument(12); ok {
		t.Fatal("user 12 can see user 11 document")
	}
	document, ok := st.ActiveDocument(11)
	if !ok || document.URL != "https://example.test/one.pdf" {
		t.Fatalf("unexpected document: %#v, %v", document, ok)
	}
	if err := st.ClearDocuments(11); err != nil {
		t.Fatal(err)
	}
	if _, ok := st.ActiveDocument(11); ok {
		t.Fatal("document was not cleared")
	}
}
