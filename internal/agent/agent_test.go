package agent

import (
	"testing"
	"time"
)

func TestParseRelativeDelay(t *testing.T) {
	tests := []struct {
		text string
		want time.Duration
	}{
		{"Напомни через 5 минут забрать заказ", 5 * time.Minute},
		{"Поставь через минуту проверить чайник", time.Minute},
		{"Напомни через 2 часа позвонить", 2 * time.Hour},
	}
	for _, test := range tests {
		got, ok := parseRelativeDelay(test.text)
		if !ok || got != test.want {
			t.Errorf("parseRelativeDelay(%q) = %v, %v; want %v", test.text, got, ok, test.want)
		}
	}
}
