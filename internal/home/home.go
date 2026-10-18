// Package home: GET /v1/home — semua isi Beranda dalam satu request (§5.18).
// Isi & urutan section diatur tabel home_sections, jadi bisa diubah tanpa update app.
package home

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"trytuist-api/internal/catalog"
	"trytuist-api/internal/database"
	"trytuist-api/internal/httpx"
	"trytuist-api/internal/user"
)

// Tipe section. Tipe yang tidak dikenal iOS dilewati, jadi aman menambah tipe baru.
const (
	TypeBannerCarousel  = "BANNER_CAROUSEL"
	TypeCategoryGrid    = "CATEGORY_GRID"
	TypeProductCarousel = "PRODUCT_CAROUSEL"
	TypeProductList     = "PRODUCT_LIST"
)

type sectionRow struct {
	ID         string
	Type       string
	Title      *string
	Rule       *string
	ProductIDs []string
	Limit      int
	SeeAll     []byte
}

// ===== DTO =====

type UserDTO struct {
	Name         string `json:"name"`
	Points       int    `json:"points"`
	VoucherCount int    `json:"voucher_count"`
}

type AddressDTO struct {
	ID      string `json:"id"`
	Label   string `json:"label"`
	Address string `json:"address"`
}

type StoreDTO struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	IsOpen bool   `json:"is_open"`
}

type HomeDTO struct {
	Greeting        string           `json:"greeting"`
	User            *UserDTO         `json:"user"`
	DeliveryAddress *AddressDTO      `json:"delivery_address"`
	Store           *StoreDTO        `json:"store"`
	Sections        []map[string]any `json:"sections"`
}

// Greeting dihitung dari jam lokal (default Asia/Jakarta).
func Greeting(t time.Time) string {
	switch h := t.Hour(); {
	case h >= 4 && h < 11:
		return "Selamat pagi ☀️"
	case h >= 11 && h < 15:
		return "Selamat siang 🌤️"
	case h >= 15 && h < 18:
		return "Selamat sore 🌇"
	default:
		return "Selamat malam 🌙"
	}
}

// ===== Handler =====

type Handler struct {
	pool    *pgxpool.Pool
	catalog *catalog.Repository
	users   user.Repository
	loc     *time.Location
	now     func() time.Time
}

func NewHandler(pool *pgxpool.Pool, loc *time.Location) *Handler {
	if loc == nil {
		loc = time.UTC
	}
	return &Handler{pool: pool, catalog: catalog.NewRepository(pool), loc: loc, now: time.Now}
}

// GET /v1/home 🔒* (token opsional) — query opsional lat, lng.
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	coord, err := catalog.ParseCoord(r.URL.Query())
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	out := HomeDTO{Greeting: Greeting(h.now().In(h.loc)), Sections: []map[string]any{}}

	if uid := httpx.UserID(ctx); uid != "" {
		if err := h.fillUser(ctx, uid, &out); err != nil {
			httpx.FromError(w, r, err)
			return
		}
	}

	stores, err := h.catalog.Stores(ctx)
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	for _, s := range catalog.SortByDistance(stores, coord) {
		if s.IsOpen {
			out.Store = &StoreDTO{ID: s.ID, Name: s.Name, IsOpen: true}
			break
		}
	}
	if out.Store == nil && len(stores) > 0 { // semua tutup: tetap tampilkan, iOS bisa memberi label "Tutup"
		out.Store = &StoreDTO{ID: stores[0].ID, Name: stores[0].Name, IsOpen: false}
	}

	if out.Sections, err = h.sections(ctx); err != nil {
		httpx.FromError(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, out)
}

func (h *Handler) fillUser(ctx context.Context, uid string, out *HomeDTO) error {
	p, err := h.users.Profile(ctx, h.pool, uid)
	if err != nil {
		if database.IsNoRows(err) {
			return httpx.Unauthorized("Akun tidak ditemukan. Silakan masuk lagi.")
		}
		return err
	}
	out.User = &UserDTO{Name: p.Name, Points: p.Points, VoucherCount: p.VoucherCount}

	addrs, err := h.users.Addresses(ctx, h.pool, uid) // default selalu paling atas
	if err != nil {
		return err
	}
	if len(addrs) > 0 && addrs[0].IsDefault {
		a := addrs[0]
		out.DeliveryAddress = &AddressDTO{ID: a.ID, Label: a.Label, Address: a.Address}
	}
	return nil
}

func (h *Handler) sections(ctx context.Context) ([]map[string]any, error) {
	rows, err := h.pool.Query(ctx, `
		SELECT id, type, title, rule, COALESCE(product_ids, '{}'), item_limit, see_all
		FROM home_sections WHERE is_active ORDER BY sort_order, id`)
	if err != nil {
		return nil, err
	}
	defs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (sectionRow, error) {
		var s sectionRow
		err := row.Scan(&s.ID, &s.Type, &s.Title, &s.Rule, &s.ProductIDs, &s.Limit, &s.SeeAll)
		return s, err
	})
	if err != nil {
		return nil, err
	}

	// Banner & kategori diambil sekali walau dipakai beberapa section.
	var banners []catalog.Banner
	var categories []catalog.Category

	out := make([]map[string]any, 0, len(defs))
	for _, d := range defs {
		sec := map[string]any{"id": d.ID, "type": d.Type, "title": d.Title}
		switch d.Type {
		case TypeBannerCarousel:
			if banners == nil {
				if banners, err = h.catalog.ActiveBanners(ctx); err != nil {
					return nil, err
				}
			}
			sec["banners"] = catalog.ToBannerDTOs(head(banners, d.Limit))
		case TypeCategoryGrid:
			if categories == nil {
				if categories, err = h.catalog.ActiveCategories(ctx); err != nil {
					return nil, err
				}
			}
			sec["categories"] = catalog.ToCategoryDTOs(head(categories, d.Limit))
		case TypeProductCarousel, TypeProductList:
			rule := ""
			if d.Rule != nil {
				rule = *d.Rule
			}
			products, err := h.catalog.SectionProducts(ctx, rule, d.ProductIDs, d.Limit)
			if err != nil {
				return nil, err
			}
			if len(products) == 0 {
				continue // section produk kosong tidak dikirim
			}
			sec["products"] = catalog.ToProductDTOs(products)
			sec["see_all"] = json.RawMessage(nullIfEmpty(d.SeeAll))
		default:
			continue
		}
		out = append(out, sec)
	}
	return out, nil
}

func head[T any](s []T, n int) []T {
	if n > 0 && len(s) > n {
		return s[:n]
	}
	return s
}

func nullIfEmpty(b []byte) []byte {
	if len(b) == 0 {
		return []byte("null")
	}
	return b
}
