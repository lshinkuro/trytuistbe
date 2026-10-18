package catalog

import (
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"

	"trytuist-api/internal/httpx"
)

type StoreDTO struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Address    string   `json:"address"`
	Lat        float64  `json:"lat"`
	Lng        float64  `json:"lng"`
	IsOpen     bool     `json:"is_open"`
	DistanceKM *float64 `json:"distance_km"`
}

// StoreWithDistance: outlet + jarak (nil bila koordinat user tidak dikirim).
type StoreWithDistance struct {
	Store
	DistanceKM *float64
}

// GET /v1/stores?lat=&lng= — bila koordinat dikirim, urut dari yang terdekat (outlet buka dulu).
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
	sorted := SortByDistance(stores, coord)
	out := make([]StoreDTO, len(sorted))
	for i, s := range sorted {
		out[i] = StoreDTO{ID: s.ID, Name: s.Name, Address: s.Address, Lat: s.Lat, Lng: s.Lng, IsOpen: s.IsOpen, DistanceKM: s.DistanceKM}
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

// SortByDistance: tanpa koordinat urutan tetap (per id); dengan koordinat,
// outlet buka lebih dulu lalu yang terdekat.
func SortByDistance(stores []Store, c *Coord) []StoreWithDistance {
	out := make([]StoreWithDistance, len(stores))
	for i, s := range stores {
		out[i] = StoreWithDistance{Store: s}
		if c != nil {
			d := math.Round(haversineKM(c.Lat, c.Lng, s.Lat, s.Lng)*10) / 10
			out[i].DistanceKM = &d
		}
	}
	if c != nil {
		sort.SliceStable(out, func(i, j int) bool {
			if out[i].IsOpen != out[j].IsOpen {
				return out[i].IsOpen
			}
			return *out[i].DistanceKM < *out[j].DistanceKM
		})
	}
	return out
}

func haversineKM(lat1, lng1, lat2, lng2 float64) float64 {
	const earthRadiusKM = 6371.0
	toRad := func(d float64) float64 { return d * math.Pi / 180 }
	dLat, dLng := toRad(lat2-lat1), toRad(lng2-lng1)
	a := math.Sin(dLat/2)*math.Sin(dLat/2) + math.Cos(toRad(lat1))*math.Cos(toRad(lat2))*math.Sin(dLng/2)*math.Sin(dLng/2)
	return 2 * earthRadiusKM * math.Asin(math.Sqrt(a))
}
