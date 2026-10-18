package catalog

import (
	"testing"
	"time"
)

var wib = time.FixedZone("WIB", 7*3600)

func at(h, m int) time.Time { return time.Date(2026, 10, 18, h, m, 0, 0, wib) }

func TestIsOpenAt(t *testing.T) {
	day := Store{IsOpen: true, IsActive: true, OpensAt: 8 * 60, ClosesAt: 22 * 60}
	night := Store{IsOpen: true, IsActive: true, OpensAt: 18 * 60, ClosesAt: 2 * 60}
	allDay := Store{IsOpen: true, IsActive: true}
	cases := []struct {
		name string
		s    Store
		t    time.Time
		want bool
	}{
		{"buka pagi", day, at(8, 0), true},
		{"tutup tepat jam tutup", day, at(22, 0), false},
		{"sebelum buka", day, at(7, 59), false},
		{"lewat tengah malam, malam", night, at(23, 30), true},
		{"lewat tengah malam, dini hari", night, at(1, 59), true},
		{"lewat tengah malam, siang", night, at(12, 0), false},
		{"24 jam", allDay, at(3, 0), true},
		{"tutup sementara", Store{IsOpen: false, IsActive: true}, at(12, 0), false},
		{"nonaktif", Store{IsOpen: true, IsActive: false}, at(12, 0), false},
	}
	for _, c := range cases {
		if got := c.s.IsOpenAt(c.t, wib); got != c.want {
			t.Errorf("%s: dapat %v, harap %v", c.name, got, c.want)
		}
	}
	if day.HoursLabel() != "08:00–22:00" || allDay.HoursLabel() != "24 jam" {
		t.Fatal("label jam buka salah")
	}
}

func TestPickStore(t *testing.T) {
	user := &Coord{Lat: -6.2420, Lng: 106.7830}
	near := Store{ID: "A", IsOpen: true, IsActive: true, Lat: -6.2443, Lng: 106.7834, RadiusKM: 5}
	mid := Store{ID: "B", IsOpen: true, IsActive: true, Lat: -6.2608, Lng: 106.8133, RadiusKM: 5, Sort: 1}
	far := Store{ID: "C", IsOpen: true, IsActive: true, Lat: -6.1588, Lng: 106.9057, RadiusKM: 6}

	if s, _ := PickStore([]Store{far, mid, near}, user, at(12, 0), wib); s.ID != "A" {
		t.Fatalf("harus yang terdekat, dapat %s", s.ID)
	}
	near.IsOpen = false
	if s, _ := PickStore([]Store{far, mid, near}, user, at(12, 0), wib); s.ID != "B" {
		t.Fatalf("yang terdekat tutup → outlet buka berikutnya, dapat %s", s.ID)
	}
	mid.IsOpen = false
	// Semua yang menjangkau tutup: tetap pilih yang buka walau di luar jangkauan (checkout akan menolak dengan pesan jelas).
	if s, _ := PickStore([]Store{far, mid, near}, user, at(12, 0), wib); s.ID != "C" {
		t.Fatalf("dapat %s", s.ID)
	}
	if _, ok := PickStore(nil, user, at(12, 0), wib); ok {
		t.Fatal("tanpa outlet harus ok=false")
	}
	if m, ok := ParseClock("08:30"); !ok || m != 510 {
		t.Fatal("ParseClock salah")
	}
	if _, ok := ParseClock("24:00"); ok {
		t.Fatal("24:00 harus ditolak")
	}
}
