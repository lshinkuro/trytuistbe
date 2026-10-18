package admin

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"trytuist-api/internal/database"
	"trytuist-api/internal/httpx"
)

// MaxPrice: batas atas harga produk / promo (Rp 10 juta).
const MaxPrice = 10_000_000

type AdminOptionDTO struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	ExtraPrice int64  `json:"extra_price"`
}

type AdminProductDTO struct {
	ProductID      string           `json:"product_id"`
	ProductName    string           `json:"product_name"`
	Description    string           `json:"description"`
	CategoryID     string           `json:"category_id"`
	CategoryName   string           `json:"category_name"`
	Price          int64            `json:"price"`           // harga normal (yang diubah admin)
	DealPrice      *int64           `json:"deal_price"`      // harga promo aktif, nil bila tidak ada
	EffectivePrice int64            `json:"effective_price"` // yang dibayar pelanggan sekarang
	ImageURL       string           `json:"image_url"`
	Rating         float64          `json:"rating"`
	SoldCount      int              `json:"sold_count"`
	IsAvailable    bool             `json:"is_available"`
	IsBestSeller   bool             `json:"is_best_seller"`
	Options        []AdminOptionDTO `json:"options"`
	UpdatedAt      string           `json:"updated_at"`
	// Hanya bila ?store_id= dikirim (§5.26): tersedia di outlet itu, dan apakah ditandai habis khusus di outlet itu.
	StoreAvailable *bool `json:"store_available,omitempty"`
	StoreSoldOut   *bool `json:"store_sold_out,omitempty"`
}

const adminProductSelect = `
	SELECT p.id, p.name, p.description, p.category_id, c.name, p.price,
	       (SELECT min(d.deal_price) FROM product_deals d
	        WHERE d.product_id = p.id AND d.is_active AND d.starts_at <= now() AND d.ends_at > now()
	          AND d.deal_price < p.price),
	       COALESCE(p.image_url, ''), p.rating::float8, p.sold_count, p.is_available, p.is_best_seller, p.updated_at
	FROM products p JOIN categories c ON c.id = p.category_id `

func scanAdminProduct(row pgx.CollectableRow) (AdminProductDTO, error) {
	var d AdminProductDTO
	var updated time.Time
	err := row.Scan(&d.ProductID, &d.ProductName, &d.Description, &d.CategoryID, &d.CategoryName, &d.Price,
		&d.DealPrice, &d.ImageURL, &d.Rating, &d.SoldCount, &d.IsAvailable, &d.IsBestSeller, &updated)
	d.EffectivePrice = d.Price
	if d.DealPrice != nil {
		d.EffectivePrice = *d.DealPrice
	}
	d.UpdatedAt = httpx.ISO(updated)
	d.Options = []AdminOptionDTO{}
	return d, err
}

func (h *Handler) attachOptions(ctx context.Context, list []AdminProductDTO) error {
	if len(list) == 0 {
		return nil
	}
	ids := make([]string, len(list))
	idx := map[string]int{}
	for i, p := range list {
		ids[i], idx[p.ProductID] = p.ProductID, i
	}
	rows, err := h.pool.Query(ctx, `SELECT product_id, id, name, extra_price FROM product_options
		WHERE product_id = ANY($1) ORDER BY product_id, sort_order, id`, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var pid string
		var o AdminOptionDTO
		if err := rows.Scan(&pid, &o.ID, &o.Name, &o.ExtraPrice); err != nil {
			return err
		}
		list[idx[pid]].Options = append(list[idx[pid]].Options, o)
	}
	return rows.Err()
}

// GET /v1/admin/products?q=&category_id=&available=true|false — termasuk produk habis
// dan kategori nonaktif (tidak dipaginasi; katalog F&B kecil).
func (h *Handler) Products(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var where []string
	var args []any
	arg := func(v any) string { args = append(args, v); return fmt.Sprintf("$%d", len(args)) }
	if s := strings.TrimSpace(q.Get("q")); s != "" {
		ph := arg("%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s) + "%")
		where = append(where, fmt.Sprintf("(p.name ILIKE %s OR p.id ILIKE %s)", ph, ph))
	}
	if c := strings.TrimSpace(q.Get("category_id")); c != "" {
		where = append(where, "p.category_id = "+arg(c))
	}
	switch q.Get("available") {
	case "":
	case "true":
		where = append(where, "p.is_available")
	case "false":
		where = append(where, "NOT p.is_available")
	default:
		httpx.FromError(w, r, httpx.Validation("Filter tidak valid.", httpx.FieldDetail{Field: "available", Message: "harus true atau false"}))
		return
	}
	sql := adminProductSelect
	if len(where) > 0 {
		sql += "WHERE " + strings.Join(where, " AND ")
	}
	ctx := r.Context()
	store, err := h.catalog.StoreFromQuery(ctx, q.Get("store_id"))
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	rows, err := h.pool.Query(ctx, sql+" ORDER BY c.sort_order, p.sort_order, p.id", args...)
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	list, err := pgx.CollectRows(rows, scanAdminProduct)
	if err == nil {
		err = h.attachOptions(ctx, list)
	}
	if err == nil && store != nil {
		err = h.annotateStore(ctx, store.ID, list)
	}
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, list)
}

func (h *Handler) product(ctx context.Context, id string) (AdminProductDTO, error) {
	rows, err := h.pool.Query(ctx, adminProductSelect+"WHERE p.id = $1", id)
	if err != nil {
		return AdminProductDTO{}, err
	}
	p, err := pgx.CollectExactlyOneRow(rows, scanAdminProduct)
	if database.IsNoRows(err) {
		return p, httpx.NotFound("Produk tidak ditemukan.")
	}
	if err != nil {
		return p, err
	}
	list := []AdminProductDTO{p}
	err = h.attachOptions(ctx, list)
	return list[0], err
}

// annotateStore mengisi store_available / store_sold_out untuk outlet storeID.
func (h *Handler) annotateStore(ctx context.Context, storeID string, list []AdminProductDTO) error {
	ids := make([]string, len(list))
	for i, p := range list {
		ids[i] = p.ProductID
	}
	off, err := h.catalog.UnavailableAt(ctx, storeID, ids)
	if err != nil {
		return err
	}
	for i := range list {
		soldOut := off[list[i].ProductID]
		available := list[i].IsAvailable && !soldOut
		list[i].StoreSoldOut, list[i].StoreAvailable = &soldOut, &available
	}
	return nil
}

// productAt: produk + status di outlet (bila storeID tidak kosong).
func (h *Handler) productAt(ctx context.Context, id, storeID string) (AdminProductDTO, error) {
	p, err := h.product(ctx, id)
	if err != nil || storeID == "" {
		return p, err
	}
	list := []AdminProductDTO{p}
	err = h.annotateStore(ctx, storeID, list)
	return list[0], err
}

// GET /v1/admin/products/{id}?store_id=
func (h *Handler) Product(w http.ResponseWriter, r *http.Request) {
	store, err := h.catalog.StoreFromQuery(r.Context(), r.URL.Query().Get("store_id"))
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	storeID := ""
	if store != nil {
		storeID = store.ID
	}
	p, err := h.productAt(r.Context(), chi.URLParam(r, "id"), storeID)
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, p)
}

// ===== Tambah & ubah menu =====

// productIDLockKey: pg_advisory_xact_lock supaya dua "Tambah menu" bersamaan tidak dapat id yang sama.
const productIDLockKey = 7_250_001

// OptionInput: varian menu. ID kosong = varian baru (id dibuat dari nama, mis. "Porsi Jumbo" → "porsi-jumbo").
type OptionInput struct {
	ID         string `json:"id" validate:"max=40"`
	Name       string `json:"name" validate:"max=50"`
	ExtraPrice int64  `json:"extra_price" validate:"min=0,max=1000000"`
}

// Semua field opsional di PATCH; yang tidak dikirim tidak diubah.
// `options` dikirim = daftar varian diganti seluruhnya (urutan = urutan tampil).
type productRequest struct {
	CategoryID   *string        `json:"category_id" validate:"omitempty,max=50"`
	Name         *string        `json:"product_name" validate:"omitempty,max=100"`
	Description  *string        `json:"description" validate:"omitempty,max=500"`
	Price        *int64         `json:"price" validate:"omitempty,min=0,max=10000000"`
	ImageURL     *string        `json:"image_url" validate:"omitempty,max=1000"`
	IsAvailable  *bool          `json:"is_available"`
	IsBestSeller *bool          `json:"is_best_seller"`
	Options      *[]OptionInput `json:"options" validate:"omitempty,max=10,dive"`
}

func (req productRequest) empty() bool {
	return req.CategoryID == nil && req.Name == nil && req.Description == nil && req.Price == nil &&
		req.ImageURL == nil && req.IsAvailable == nil && req.IsBestSeller == nil && req.Options == nil
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

func slugify(s string) string {
	s = strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(s), "-"), "-")
	if len(s) > 40 {
		s = strings.TrimRight(s[:40], "-")
	}
	if s == "" {
		s = "varian"
	}
	return s
}

// normalize merapikan & memvalidasi isi request (aturan yang tidak bisa lewat tag `validate`).
func (req *productRequest) normalize() error {
	var details []httpx.FieldDetail
	trim := func(p *string) {
		if p != nil {
			*p = strings.TrimSpace(*p)
		}
	}
	trim(req.CategoryID)
	trim(req.Name)
	trim(req.Description)
	trim(req.ImageURL)
	if req.Name != nil && *req.Name == "" {
		details = append(details, httpx.FieldDetail{Field: "product_name", Message: "wajib diisi"})
	}
	if req.CategoryID != nil && *req.CategoryID == "" {
		details = append(details, httpx.FieldDetail{Field: "category_id", Message: "wajib diisi"})
	}
	if req.ImageURL != nil && *req.ImageURL != "" && !strings.HasPrefix(*req.ImageURL, "https://") {
		details = append(details, httpx.FieldDetail{Field: "image_url", Message: "harus diawali https://"})
	}
	if req.Options != nil {
		seenID, seenName := map[string]bool{}, map[string]bool{}
		opts := *req.Options
		for i := range opts {
			o := &opts[i]
			o.Name = strings.TrimSpace(o.Name)
			o.ID = strings.TrimSpace(o.ID)
			field := fmt.Sprintf("options[%d].name", i)
			switch {
			case o.Name == "":
				details = append(details, httpx.FieldDetail{Field: field, Message: "wajib diisi"})
				continue
			case seenName[strings.ToLower(o.Name)]:
				details = append(details, httpx.FieldDetail{Field: field, Message: "nama varian tidak boleh sama"})
				continue
			}
			seenName[strings.ToLower(o.Name)] = true
			if o.ID == "" {
				o.ID = slugify(o.Name)
			}
			for base, n := o.ID, 2; seenID[o.ID]; n++ {
				o.ID = fmt.Sprintf("%s-%d", base, n)
			}
			seenID[o.ID] = true
		}
	}
	if len(details) > 0 {
		return httpx.Validation("Data menu belum lengkap.", details...)
	}
	return nil
}

func categoryExists(ctx context.Context, db database.DBTX, id string) error {
	var ok bool
	if err := db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM categories WHERE id = $1)`, id).Scan(&ok); err != nil {
		return err
	}
	if !ok {
		return httpx.Validation("Kategori tidak ditemukan.", httpx.FieldDetail{Field: "category_id", Message: "kategori tidak ada"})
	}
	return nil
}

func replaceOptions(ctx context.Context, tx pgx.Tx, productID string, opts []OptionInput) error {
	if _, err := tx.Exec(ctx, `DELETE FROM product_options WHERE product_id = $1`, productID); err != nil {
		return err
	}
	for i, o := range opts {
		if _, err := tx.Exec(ctx, `INSERT INTO product_options (product_id, id, name, extra_price, sort_order)
			VALUES ($1, $2, $3, $4, $5)`, productID, o.ID, o.Name, o.ExtraPrice, i+1); err != nil {
			return err
		}
	}
	return nil
}

// POST /v1/admin/products → 201. Wajib: category_id, product_name, price.
// Id dibuat berurutan seperti seed (P15, P16, …); menu baru tampil di akhir kategorinya.
func (h *Handler) CreateProduct(w http.ResponseWriter, r *http.Request) {
	var req productRequest
	if err := httpx.Decode(r, &req); err != nil {
		httpx.FromError(w, r, err)
		return
	}
	if err := req.normalize(); err != nil {
		httpx.FromError(w, r, err)
		return
	}
	var missing []httpx.FieldDetail
	if req.CategoryID == nil {
		missing = append(missing, httpx.FieldDetail{Field: "category_id", Message: "wajib diisi"})
	}
	if req.Name == nil {
		missing = append(missing, httpx.FieldDetail{Field: "product_name", Message: "wajib diisi"})
	}
	if req.Price == nil {
		missing = append(missing, httpx.FieldDetail{Field: "price", Message: "wajib diisi"})
	}
	if len(missing) > 0 {
		httpx.FromError(w, r, httpx.Validation("Data menu belum lengkap.", missing...))
		return
	}
	str := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	boolOr := func(p *bool, def bool) bool {
		if p == nil {
			return def
		}
		return *p
	}
	ctx := r.Context()
	var id string
	err := database.InTx(ctx, h.pool, func(tx pgx.Tx) error {
		if err := categoryExists(ctx, tx, *req.CategoryID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, productIDLockKey); err != nil {
			return err
		}
		err := tx.QueryRow(ctx, `
			INSERT INTO products (id, category_id, name, description, price, image_url, is_available, is_best_seller, sort_order)
			SELECT 'P' || CASE WHEN n.next < 10 THEN '0' ELSE '' END || n.next::text,
			       $1, $2, $3, $4, NULLIF($5, ''), $6, $7,
			       (SELECT COALESCE(max(sort_order), 0) + 1 FROM products)
			FROM (SELECT COALESCE(max(substring(id FROM '^P([0-9]+)$')::int), 0) + 1 AS next FROM products) n
			RETURNING id`,
			*req.CategoryID, *req.Name, str(req.Description), *req.Price, str(req.ImageURL),
			boolOr(req.IsAvailable, true), boolOr(req.IsBestSeller, false)).Scan(&id)
		if err != nil {
			return err
		}
		if req.Options != nil {
			return replaceOptions(ctx, tx, id, *req.Options)
		}
		return nil
	})
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	p, err := h.product(ctx, id)
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	h.audit(r, "PRODUCT_CREATE", "product", id,
		fmt.Sprintf("Menu baru %s (%s) %s", p.ProductName, p.CategoryName, rupiah(p.Price)), p)
	httpx.JSONMessage(w, http.StatusCreated, "Menu ditambahkan", p)
}

// PATCH /v1/admin/products/{id}
func (h *Handler) UpdateProduct(w http.ResponseWriter, r *http.Request) {
	var req productRequest
	if err := httpx.Decode(r, &req); err != nil {
		httpx.FromError(w, r, err)
		return
	}
	if err := req.normalize(); err != nil {
		httpx.FromError(w, r, err)
		return
	}
	if req.empty() {
		httpx.FromError(w, r, httpx.Validation("Tidak ada perubahan yang dikirim."))
		return
	}
	ctx := r.Context()
	id := chi.URLParam(r, "id")
	before, err := h.product(ctx, id)
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	err = database.InTx(ctx, h.pool, func(tx pgx.Tx) error {
		if req.CategoryID != nil {
			if err := categoryExists(ctx, tx, *req.CategoryID); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `
			UPDATE products SET
				category_id = COALESCE($2, category_id),
				name = COALESCE($3, name),
				description = COALESCE($4, description),
				price = COALESCE($5, price),
				image_url = CASE WHEN $6::text IS NULL THEN image_url ELSE NULLIF($6, '') END,
				is_available = COALESCE($7, is_available),
				is_best_seller = COALESCE($8, is_best_seller),
				updated_at = now()
			WHERE id = $1`, id, req.CategoryID, req.Name, req.Description, req.Price, req.ImageURL,
			req.IsAvailable, req.IsBestSeller); err != nil {
			return err
		}
		if req.Options != nil {
			return replaceOptions(ctx, tx, id, *req.Options)
		}
		return nil
	})
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	after, err := h.product(ctx, id)
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	changes := map[string]any{}
	var parts []string
	if before.Price != after.Price {
		changes["price"] = map[string]int64{"from": before.Price, "to": after.Price}
		parts = append(parts, fmt.Sprintf("harga %s → %s", rupiah(before.Price), rupiah(after.Price)))
	}
	if before.IsAvailable != after.IsAvailable {
		changes["is_available"] = after.IsAvailable
		parts = append(parts, map[bool]string{true: "tersedia", false: "habis"}[after.IsAvailable])
	}
	if before.ProductName != after.ProductName {
		changes["product_name"] = map[string]string{"from": before.ProductName, "to": after.ProductName}
		parts = append(parts, "nama")
	}
	if before.CategoryID != after.CategoryID {
		changes["category_id"] = map[string]string{"from": before.CategoryID, "to": after.CategoryID}
		parts = append(parts, "kategori "+after.CategoryName)
	}
	if before.Description != after.Description {
		changes["description"] = true
		parts = append(parts, "deskripsi")
	}
	if before.ImageURL != after.ImageURL {
		changes["image_url"] = after.ImageURL
		parts = append(parts, "foto")
	}
	if before.IsBestSeller != after.IsBestSeller {
		changes["is_best_seller"] = after.IsBestSeller
		parts = append(parts, "label terlaris")
	}
	if req.Options != nil && fmt.Sprint(before.Options) != fmt.Sprint(after.Options) {
		changes["options"] = after.Options
		parts = append(parts, fmt.Sprintf("%d varian", len(after.Options)))
	}
	if len(parts) > 0 {
		h.audit(r, "PRODUCT_UPDATE", "product", id, after.ProductName+": "+strings.Join(parts, ", "), changes)
	}
	httpx.JSONMessage(w, http.StatusOK, "Produk diperbarui", after)
}

// rupiah: 15000 → "Rp 15.000" (sama dengan order.Rupiah, dipakai di ringkasan audit).
func rupiah(n int64) string {
	s := fmt.Sprint(n)
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte('.')
		}
		b.WriteRune(c)
	}
	return "Rp " + b.String()
}
