package catalog

// Banyak outlet (§5.26): harga sama di semua outlet; yang berbeda per outlet adalah
// ketersediaan menu, jam buka, dan jangkauan antar.

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"trytuist-api/internal/database"
	"trytuist-api/internal/httpx"
)

type Store struct {
	ID       string
	Name     string
	Address  string
	Lat      float64
	Lng      float64
	Phone    *string
	IsOpen   bool // tombol "tutup sementara" dari admin
	OpensAt  int  // menit sejak 00:00
	ClosesAt int
	RadiusKM float64
	IsActive bool
	Sort     int
}

// HoursLabel: "08:00–22:00" atau "24 jam".
func (s Store) HoursLabel() string {
	if s.OpensAt == s.ClosesAt {
		return "24 jam"
	}
	return FormatClock(s.OpensAt) + "–" + FormatClock(s.ClosesAt)
}

// IsOpenAt: buka manual + aktif + di dalam jam buka (zona waktu toko).
// ClosesAt < OpensAt = buka lewat tengah malam, mis. 18:00–02:00.
func (s Store) IsOpenAt(t time.Time, loc *time.Location) bool {
	if !s.IsOpen || !s.IsActive {
		return false
	}
	if s.OpensAt == s.ClosesAt {
		return true
	}
	lt := t.In(loc)
	m := lt.Hour()*60 + lt.Minute()
	if s.OpensAt < s.ClosesAt {
		return m >= s.OpensAt && m < s.ClosesAt
	}
	return m >= s.OpensAt || m < s.ClosesAt
}

// ClosedReason: kalimat untuk pelanggan saat outlet tidak bisa menerima pesanan.
func (s Store) ClosedReason() string {
	switch {
	case !s.IsActive:
		return "Outlet " + s.Name + " sudah tidak beroperasi."
	case !s.IsOpen:
		return "Outlet " + s.Name + " sedang tutup sementara."
	default:
		return fmt.Sprintf("Outlet %s buka %s.", s.Name, s.HoursLabel())
	}
}

// DistanceKM ke koordinat c, dibulatkan 1 desimal.
func (s Store) DistanceKM(c Coord) float64 {
	return math.Round(haversineKM(c.Lat, c.Lng, s.Lat, s.Lng)*10) / 10
}

func (s Store) Delivers(c Coord) bool { return s.DistanceKM(c) <= s.RadiusKM }

func FormatClock(m int) string { return fmt.Sprintf("%02d:%02d", m/60, m%60) }

// ParseClock: "08:30" → 510.
func ParseClock(v string) (int, bool) {
	h, mm, ok := strings.Cut(strings.TrimSpace(v), ":")
	if !ok {
		return 0, false
	}
	hi, err1 := strconv.Atoi(h)
	mi, err2 := strconv.Atoi(mm)
	if err1 != nil || err2 != nil || hi < 0 || hi > 23 || mi < 0 || mi > 59 {
		return 0, false
	}
	return hi*60 + mi, true
}

const storeColumns = `id, name, address, lat, lng, phone, is_open,
	(extract(hour FROM opens_at) * 60 + extract(minute FROM opens_at))::int,
	(extract(hour FROM closes_at) * 60 + extract(minute FROM closes_at))::int,
	delivery_radius_km::float8, is_active, sort_order`

func scanStore(row pgx.CollectableRow) (Store, error) {
	var s Store
	err := row.Scan(&s.ID, &s.Name, &s.Address, &s.Lat, &s.Lng, &s.Phone, &s.IsOpen,
		&s.OpensAt, &s.ClosesAt, &s.RadiusKM, &s.IsActive, &s.Sort)
	return s, err
}

// Stores: outlet aktif untuk pelanggan.
func (r *Repository) Stores(ctx context.Context) ([]Store, error) {
	return r.queryStores(ctx, `SELECT `+storeColumns+` FROM stores WHERE is_active ORDER BY sort_order, id`)
}

// AllStores: termasuk outlet nonaktif (web admin).
func (r *Repository) AllStores(ctx context.Context) ([]Store, error) {
	return r.queryStores(ctx, `SELECT `+storeColumns+` FROM stores ORDER BY is_active DESC, sort_order, id`)
}

func (r *Repository) queryStores(ctx context.Context, sql string, args ...any) ([]Store, error) {
	rows, err := r.db.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanStore)
}

// StoreByID: ok=false bila tidak ada (termasuk nonaktif bila activeOnly).
func (r *Repository) StoreByID(ctx context.Context, id string, activeOnly bool) (Store, bool, error) {
	sql := `SELECT ` + storeColumns + ` FROM stores WHERE id = $1`
	if activeOnly {
		sql += ` AND is_active`
	}
	rows, err := r.db.Query(ctx, sql, id)
	if err != nil {
		return Store{}, false, err
	}
	s, err := pgx.CollectExactlyOneRow(rows, scanStore)
	if database.IsNoRows(err) {
		return Store{}, false, nil
	}
	return s, err == nil, err
}

// StoreFromQuery membaca ?store_id= (opsional). Outlet tidak dikenal → 400.
func (r *Repository) StoreFromQuery(ctx context.Context, storeID string) (*Store, error) {
	storeID = strings.TrimSpace(storeID)
	if storeID == "" {
		return nil, nil
	}
	s, ok, err := r.StoreByID(ctx, storeID, true)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, httpx.Validation("Outlet tidak ditemukan.", httpx.FieldDetail{Field: "store_id", Message: "tidak ditemukan"})
	}
	return &s, nil
}

// PickStore memilih outlet untuk pelanggan: yang buka sekarang & menjangkau alamat dulu,
// lalu yang terdekat. Tanpa koordinat: outlet buka pertama menurut urutan admin.
// ok=false bila tidak ada outlet aktif sama sekali.
func PickStore(stores []Store, c *Coord, now time.Time, loc *time.Location) (Store, bool) {
	ranked := RankStores(stores, c, now, loc)
	if len(ranked) == 0 {
		return Store{}, false
	}
	return ranked[0].Store, true
}

// RankedStore: outlet + status untuk pelanggan.
type RankedStore struct {
	Store
	OpenNow     bool
	DistanceKM  *float64
	Deliverable *bool
}

// RankStores: buka sekarang → menjangkau alamat → terdekat → urutan admin.
func RankStores(stores []Store, c *Coord, now time.Time, loc *time.Location) []RankedStore {
	out := make([]RankedStore, len(stores))
	for i, s := range stores {
		out[i] = RankedStore{Store: s, OpenNow: s.IsOpenAt(now, loc)}
		if c != nil {
			d := s.DistanceKM(*c)
			ok := d <= s.RadiusKM
			out[i].DistanceKM, out[i].Deliverable = &d, &ok
		}
	}
	score := func(r RankedStore) int {
		n := 0
		if r.OpenNow {
			n += 2
		}
		if r.Deliverable == nil || *r.Deliverable {
			n++
		}
		return n
	}
	sortStable(out, func(a, b RankedStore) bool {
		if sa, sb := score(a), score(b); sa != sb {
			return sa > sb
		}
		if a.DistanceKM != nil && b.DistanceKM != nil && *a.DistanceKM != *b.DistanceKM {
			return *a.DistanceKM < *b.DistanceKM
		}
		if a.Sort != b.Sort {
			return a.Sort < b.Sort
		}
		return a.ID < b.ID
	})
	return out
}
