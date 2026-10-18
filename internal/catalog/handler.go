package catalog

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"

	"trytuist-api/internal/httpx"
)

// Katalog hanya membaca data, jadi handler langsung memakai repository
// (tidak ada aturan bisnis yang perlu lapisan service).
type Handler struct {
	repo *Repository
}

func NewHandler(repo *Repository) *Handler { return &Handler{repo: repo} }

// GET /v1/banners
func (h *Handler) Banners(w http.ResponseWriter, r *http.Request) {
	banners, err := h.repo.ActiveBanners(r.Context())
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, ToBannerDTOs(banners))
}

// GET /v1/categories
func (h *Handler) Categories(w http.ResponseWriter, r *http.Request) {
	cats, err := h.repo.ActiveCategories(r.Context())
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, ToCategoryDTOs(cats))
}

// GET /v1/products
func (h *Handler) ListProducts(w http.ResponseWriter, r *http.Request) {
	filter, err := parseProductFilter(r.URL.Query())
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	store, err := h.repo.StoreFromQuery(r.Context(), r.URL.Query().Get("store_id"))
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	products, total, err := h.repo.ListProducts(r.Context(), filter)
	if err == nil && store != nil {
		err = h.repo.ApplyStoreAvailability(r.Context(), store.ID, products)
	}
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, ToProductDTOs(products),
		httpx.Meta{Page: filter.Page, Limit: filter.Limit, Total: total})
}

// GET /v1/promotions/popup — data: null bila tidak ada promo popup (§5.24).
func (h *Handler) PopupDeal(w http.ResponseWriter, r *http.Request) {
	d, ok, err := h.repo.PopupDeal(r.Context())
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	if !ok {
		httpx.JSON(w, http.StatusOK, nil)
		return
	}
	httpx.JSON(w, http.StatusOK, ToPopupDealDTO(d))
}

// GET /v1/products/{id}?store_id=
func (h *Handler) GetProduct(w http.ResponseWriter, r *http.Request) {
	store, err := h.repo.StoreFromQuery(r.Context(), r.URL.Query().Get("store_id"))
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	p, ok, err := h.repo.ProductByID(r.Context(), chi.URLParam(r, "id"))
	if err == nil && ok && store != nil {
		list := []Product{p}
		err = h.repo.ApplyStoreAvailability(r.Context(), store.ID, list)
		p = list[0]
	}
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	if !ok {
		httpx.FromError(w, r, httpx.NotFound("Produk tidak ditemukan."))
		return
	}
	httpx.JSON(w, http.StatusOK, ToProductDTO(p))
}

func parseProductFilter(q url.Values) (ProductFilter, error) {
	page, err := httpx.ParsePage(q, 20)
	if err != nil {
		return ProductFilter{}, err
	}
	f := ProductFilter{
		CategoryID: strings.TrimSpace(q.Get("category_id")),
		Query:      q.Get("q"),
		Sort:       q.Get("sort"),
		Page:       page.Page,
		Limit:      page.Limit,
	}
	switch strings.ToLower(q.Get("best_seller")) {
	case "", "false", "0":
	case "true", "1":
		f.BestSeller = true
	default:
		return f, httpx.Validation("Parameter tidak valid.", httpx.FieldDetail{Field: "best_seller", Message: "harus true atau false"})
	}
	switch f.Sort {
	case "", "popular", "rating", "price_asc", "price_desc":
	default:
		return f, httpx.Validation("Parameter tidak valid.",
			httpx.FieldDetail{Field: "sort", Message: "harus salah satu dari: popular, rating, price_asc, price_desc"})
	}
	if len(f.Query) > 100 {
		return f, httpx.Validation("Parameter tidak valid.", httpx.FieldDetail{Field: "q", Message: "maksimal 100 karakter"})
	}
	return f, nil
}
