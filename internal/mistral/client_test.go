package mistral

import (
	"encoding/json"
	"testing"
)

func TestMessageUnmarshalSupportsContentBlocks(t *testing.T) {
	var message Message
	if err := json.Unmarshal([]byte(`{"role":"assistant","content":[{"type":"text","text":"Первая часть"},{"type":"text","text":"Вторая часть"}]}`), &message); err != nil {
		t.Fatal(err)
	}
	if message.Content != "Первая часть\nВторая часть" {
		t.Fatalf("unexpected content: %q", message.Content)
	}
}
