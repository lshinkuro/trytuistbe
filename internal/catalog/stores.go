package catalog

import (
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"time"

	"trytuist-api/internal/httpx"
)

type StoreDTO struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Address     string   `json:"address"`
	Lat         float64  `json:"lat"`
	Lng         float64  `json:"lng"`
	Phone       *string  `json:"phone"`
	IsOpen      bool     `json:"is_open"`     // buka sekarang (jam buka + tidak tutup sementara)
	OpensAt     string   `json:"opens_at"`    // "08:00"
	ClosesAt    string   `json:"closes_at"`   // "22:00"
	HoursLabel  string   `json:"hours_label"` // "08:00–22:00" | "24 jam"
	RadiusKM    float64  `json:"delivery_radius_km"`
	DistanceKM  *float64 `json:"distance_km"`
	Deliverable *bool    `json:"is_deliverable"` // nil bila koordinat tidak dikirim
	ClosedNote  *string  `json:"closed_note"`    // alasan tidak bisa pesan, nil bila buka
}

func ToStoreDTO(r RankedStore) StoreDTO {
	d := StoreDTO{
		ID: r.ID, Name: r.Name, Address: r.Address, Lat: r.Lat, Lng: r.Lng, Phone: r.Phone,
		IsOpen: r.OpenNow, OpensAt: FormatClock(r.OpensAt), ClosesAt: FormatClock(r.ClosesAt),
		HoursLabel: r.HoursLabel(), RadiusKM: r.RadiusKM, DistanceKM: r.DistanceKM, Deliverable: r.Deliverable,
	}
	if !r.OpenNow {
		note := r.ClosedReason()
		d.ClosedNote = &note
	}
	return d
}

// GET /v1/stores?lat=&lng= — outlet aktif, yang buka & menjangkau alamat lebih dulu, lalu terdekat.
func (h *Handler) Stores(w http.ResponseWriter, r *http.Request) {
	coord, err := ParseCoord(r.URL.Query())
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	stores, err := h.repo.Stores(r.Context())
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	ranked := RankStores(stores, coord, time.Now(), httpx.Loc)
	out := make([]StoreDTO, len(ranked))
	for i, s := range ranked {
		out[i] = ToStoreDTO(s)
	}
	httpx.JSON(w, http.StatusOK, out)
}

// Coord: koordinat user dari query ?lat=&lng= (nil bila tidak dikirim).
type Coord struct{ Lat, Lng float64 }

func ParseCoord(q url.Values) (*Coord, error) {
	if q.Get("lat") == "" && q.Get("lng") == "" {
		return nil, nil
	}
	lat, err1 := strconv.ParseFloat(q.Get("lat"), 64)
	lng, err2 := strconv.ParseFloat(q.Get("lng"), 64)
	if err1 != nil || err2 != nil || lat < -90 || lat > 90 || lng < -180 || lng > 180 {
		return nil, httpx.Validation("Koordinat tidak valid.",
			httpx.FieldDetail{Field: "lat,lng", Message: "kirim keduanya sebagai angka desimal yang valid"})
	}
	return &Coord{Lat: lat, Lng: lng}, nil
}

func sortStable[T any](s []T, less func(a, b T) bool) {
	sort.SliceStable(s, func(i, j int) bool { return less(s[i], s[j]) })
}

func haversineKM(lat1, lng1, lat2, lng2 float64) float64 {
	const earthRadiusKM = 6371.0
	toRad := func(d float64) float64 { return d * math.Pi / 180 }
	dLat, dLng := toRad(lat2-lat1), toRad(lng2-lng1)
	a := math.Sin(dLat/2)*math.Sin(dLat/2) + math.Cos(toRad(lat1))*math.Cos(toRad(lat2))*math.Sin(dLng/2)*math.Sin(dLng/2)
	return 2 * earthRadiusKM * math.Asin(math.Sqrt(a))
}
