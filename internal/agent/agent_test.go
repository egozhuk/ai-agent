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

func TestParseReminderInput(t *testing.T) {
	runAt, text, err := parseReminderInput("через 30 минут позвонить маме", time.FixedZone("MSK", 3*60*60))
	if err != nil || text != "позвонить маме" || runAt.Before(time.Now().Add(29*time.Minute)) {
		t.Fatalf("unexpected relative reminder: %v, %q, %v", runAt, text, err)
	}

	runAt, text, err = parseReminderInput("2026-07-25 10:00 забрать заказ", time.FixedZone("MSK", 3*60*60))
	if err != nil || text != "забрать заказ" || runAt.IsZero() {
		t.Fatalf("unexpected absolute reminder: %v, %q, %v", runAt, text, err)
	}
}

func TestInferFixedTimezone(t *testing.T) {
	zone, err := inferFixedTimezone("15:00", time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC))
	if err != nil || zone != "UTC+03:00" {
		t.Fatalf("unexpected timezone: %q, %v", zone, err)
	}
	zone, err = inferFixedTimezone("09:00", time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC))
	if err != nil || zone != "UTC-03:00" {
		t.Fatalf("unexpected negative timezone: %q, %v", zone, err)
	}
}
