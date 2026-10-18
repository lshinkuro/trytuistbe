package home

import (
	"testing"
	"time"
)

func TestGreeting(t *testing.T) {
	cases := map[int]string{4: "Selamat pagi ☀️", 10: "Selamat pagi ☀️", 11: "Selamat siang 🌤️", 15: "Selamat sore 🌇", 18: "Selamat malam 🌙", 2: "Selamat malam 🌙"}
	for hour, want := range cases {
		if got := Greeting(time.Date(2026, 10, 17, hour, 0, 0, 0, time.UTC)); got != want {
			t.Errorf("jam %d: %q, harap %q", hour, got, want)
		}
	}
}
