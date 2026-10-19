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
	// Combo: isi paket (kosong = produk biasa) & total harga pusat bila isinya dibeli satuan.
	BundleItems   []AdminBundleItemDTO `json:"bundle_items"`
	SeparatePrice int64                `json:"separate_price"`
	UpdatedAt     string               `json:"updated_at"`
	// Hanya bila ada outlet (?store_id= dari super admin, selalu untuk admin outlet) — §5.29.
	Store *StoreProductDTO `json:"store,omitempty"`
}

// StoreProductDTO: menu di satu outlet.
type StoreProductDTO struct {
	StoreID        string `json:"store_id"`
	IsListed       bool   `json:"is_listed"`       // dijual di outlet ini
	IsSoldOut      bool   `json:"is_sold_out"`     // saklar "habis" manual
	Stock          *int   `json:"stock"`           // null = stok tidak dihitung
	Price          *int64 `json:"price"`           // harga khusus outlet; null = harga pusat
	DealPrice      *int64 `json:"deal_price"`      // promo aktif di outlet ini (pusat atau outlet)
	EffectivePrice int64  `json:"effective_price"` // yang dibayar pelanggan di outlet ini
	IsAvailable    bool   `json:"is_available"`    // bisa dipesan sekarang
}

const adminProductSelect = `
	SELECT p.id, p.name, p.description, p.category_id, c.name, p.price,
	       (SELECT min(d.deal_price) FROM product_deals d
	        WHERE d.product_id = p.id AND d.is_active AND d.starts_at <= now() AND d.ends_at > now()
	          AND d.store_id IS NULL AND d.deal_price < p.price),
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
	d.BundleItems = []AdminBundleItemDTO{}
	return d, err
}

type AdminBundleItemDTO struct {
	ProductID   string  `json:"product_id"`
	ProductName string  `json:"product_name"`
	OptionID    *string `json:"option_id"`
	OptionName  *string `json:"option_name"`
	Quantity    int     `json:"quantity"`
	UnitPrice   int64   `json:"unit_price"` // harga pusat + varian
}

// BundleItemInput: satu isi combo dari form admin.
type BundleItemInput struct {
	ProductID string  `json:"product_id" validate:"required,max=50"`
	OptionID  *string `json:"option_id" validate:"omitempty,max=40"`
	Quantity  int     `json:"quantity" validate:"min=1,max=10"`
}

func (h *Handler) attachBundles(ctx context.Context, list []AdminProductDTO) error {
	if len(list) == 0 {
		return nil
	}
	ids := make([]string, len(list))
	idx := map[string]int{}
	for i, p := range list {
		ids[i], idx[p.ProductID] = p.ProductID, i
	}
	rows, err := h.pool.Query(ctx, `
		SELECT b.bundle_id, b.product_id, p.name, b.option_id, o.name, b.quantity, p.price + COALESCE(o.extra_price, 0)
		FROM product_bundle_items b
		JOIN products p ON p.id = b.product_id
		LEFT JOIN product_options o ON o.product_id = b.product_id AND o.id = b.option_id
		WHERE b.bundle_id = ANY($1) ORDER BY b.bundle_id, b.sort_order, b.product_id`, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var bid string
		var it AdminBundleItemDTO
		if err := rows.Scan(&bid, &it.ProductID, &it.ProductName, &it.OptionID, &it.OptionName, &it.Quantity, &it.UnitPrice); err != nil {
			return err
		}
		p := &list[idx[bid]]
		p.BundleItems = append(p.BundleItems, it)
		p.SeparatePrice += it.UnitPrice * int64(it.Quantity)
	}
	return rows.Err()
}

// replaceBundle: isi combo baru (kosong = jadi produk biasa). Isi harus ada, bukan dirinya sendiri,
// bukan combo lain, dan varian (bila diisi) milik isi tersebut. Combo juga tidak boleh jadi isi combo lain.
func replaceBundle(ctx context.Context, tx pgx.Tx, productID string, items []BundleItemInput) error {
	var details []httpx.FieldDetail
	if len(items) > 0 {
		var usedAsComponent bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM product_bundle_items WHERE product_id = $1)`, productID).
			Scan(&usedAsComponent); err != nil {
			return err
		}
		if usedAsComponent {
			return httpx.Validation("Menu ini sudah jadi isi paket lain, jadi tidak bisa dijadikan paket.",
				httpx.FieldDetail{Field: "bundle_items", Message: "menu ini isi paket lain"})
		}
	}
	seen := map[string]bool{}
	for i, it := range items {
		field := fmt.Sprintf("bundle_items[%d].product_id", i)
		key := it.ProductID + "|" + deref(it.OptionID)
		var exists, isCombo, optionOK bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM products WHERE id = $1),
			       EXISTS (SELECT 1 FROM product_bundle_items WHERE bundle_id = $1),
			       $2::text IS NULL OR EXISTS (SELECT 1 FROM product_options WHERE product_id = $1 AND id = $2)`,
			it.ProductID, it.OptionID).Scan(&exists, &isCombo, &optionOK); err != nil {
			return err
		}
		switch {
		case it.ProductID == productID:
			details = append(details, httpx.FieldDetail{Field: field, Message: "paket tidak boleh berisi dirinya sendiri"})
		case !exists:
			details = append(details, httpx.FieldDetail{Field: field, Message: "menu tidak ditemukan"})
		case isCombo:
			details = append(details, httpx.FieldDetail{Field: field, Message: "paket tidak boleh berisi paket lain"})
		case !optionOK:
			details = append(details, httpx.FieldDetail{Field: fmt.Sprintf("bundle_items[%d].option_id", i), Message: "varian tidak ada di menu ini"})
		case seen[key]:
			details = append(details, httpx.FieldDetail{Field: field, Message: "isi yang sama ditulis dua kali; ubah jumlahnya saja"})
		}
		seen[key] = true
	}
	if len(details) > 0 {
		return httpx.Validation("Isi paket tidak valid.", details...)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM product_bundle_items WHERE bundle_id = $1`, productID); err != nil {
		return err
	}
	for i, it := range items {
		if _, err := tx.Exec(ctx, `INSERT INTO product_bundle_items (bundle_id, product_id, option_id, quantity, sort_order)
			VALUES ($1, $2, $3, $4, $5)`, productID, it.ProductID, it.OptionID, it.Quantity, i+1); err != nil {
			return err
		}
	}
	return nil
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
	storeID, err := h.requestStore(r)
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
	if err == nil {
		err = h.attachBundles(ctx, list)
	}
	if err == nil && storeID != "" {
		err = h.annotateStore(ctx, storeID, list)
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
	if err = h.attachOptions(ctx, list); err == nil {
		err = h.attachBundles(ctx, list)
	}
	return list[0], err
}

// requestStore: outlet untuk ?store_id= (super admin, opsional) atau outlet admin outlet.
func (h *Handler) requestStore(r *http.Request) (string, error) {
	storeID, err := scopeStore(r.Context(), r.URL.Query().Get("store_id"))
	if err != nil || storeID == "" {
		return storeID, err
	}
	store, err := h.catalog.StoreFromQuery(r.Context(), storeID)
	if err != nil {
		return "", err
	}
	return store.ID, nil
}

// annotateStore mengisi `store` (dijual, stok, harga, promo) untuk outlet storeID.
func (h *Handler) annotateStore(ctx context.Context, storeID string, list []AdminProductDTO) error {
	ids := make([]string, len(list))
	idx := map[string]int{}
	for i, p := range list {
		ids[i], idx[p.ProductID] = p.ProductID, i
	}
	rows, err := h.pool.Query(ctx, `
		SELECT p.id, sp.product_id IS NOT NULL, NOT COALESCE(sp.is_available, TRUE), sp.stock, sp.price,
		       (SELECT min(d.deal_price) FROM product_deals d
		        WHERE d.product_id = p.id AND d.is_active AND d.starts_at <= now() AND d.ends_at > now()
		          AND (d.store_id IS NULL OR d.store_id = $1) AND d.deal_price < COALESCE(sp.price, p.price))
		FROM products p LEFT JOIN store_products sp ON sp.product_id = p.id AND sp.store_id = $1
		WHERE p.id = ANY($2)`, storeID, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		s := StoreProductDTO{StoreID: storeID}
		if err := rows.Scan(&id, &s.IsListed, &s.IsSoldOut, &s.Stock, &s.Price, &s.DealPrice); err != nil {
			return err
		}
		p := &list[idx[id]]
		s.EffectivePrice = p.Price
		if s.Price != nil {
			s.EffectivePrice = *s.Price
		}
		if s.DealPrice != nil {
			s.EffectivePrice = *s.DealPrice
		}
		s.IsAvailable = p.IsAvailable && s.IsListed && !s.IsSoldOut && (s.Stock == nil || *s.Stock > 0)
		p.Store = &s
	}
	return rows.Err()
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
	storeID, err := h.requestStore(r)
	if err != nil {
		httpx.FromError(w, r, err)
		return
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
	// Isi combo: nil = tidak diubah, [] = jadikan produk biasa.
	BundleItems *[]BundleItemInput `json:"bundle_items" validate:"omitempty,max=10,dive"`
}

func (req productRequest) empty() bool {
	return req.CategoryID == nil && req.Name == nil && req.Description == nil && req.Price == nil &&
		req.ImageURL == nil && req.IsAvailable == nil && req.IsBestSeller == nil && req.Options == nil && req.BundleItems == nil
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
		// Menu dari pusat dijual di semua outlet; menu dari admin outlet hanya di outletnya.
		a := adminFrom(ctx)
		if _, err := tx.Exec(ctx, `INSERT INTO store_products (store_id, product_id)
			SELECT id, $1 FROM stores WHERE $2 = '' OR id = $2`, id, a.StoreID); err != nil {
			return err
		}
		if req.Options != nil {
			if err := replaceOptions(ctx, tx, id, *req.Options); err != nil {
				return err
			}
		}
		if req.BundleItems != nil {
			return replaceBundle(ctx, tx, id, *req.BundleItems)
		}
		return nil
	})
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	p, err := h.productAt(ctx, id, adminFrom(ctx).StoreID)
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
	if err == nil {
		err = h.canEditProduct(ctx, id)
	}
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
			if err := replaceOptions(ctx, tx, id, *req.Options); err != nil {
				return err
			}
		}
		if req.BundleItems != nil {
			return replaceBundle(ctx, tx, id, *req.BundleItems)
		}
		return nil
	})
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	after, err := h.productAt(ctx, id, adminFrom(ctx).StoreID)
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
	if req.BundleItems != nil && fmt.Sprint(before.BundleItems) != fmt.Sprint(after.BundleItems) {
		changes["bundle_items"] = after.BundleItems
		parts = append(parts, fmt.Sprintf("isi paket (%d menu)", len(after.BundleItems)))
	}
	if len(parts) > 0 {
		h.audit(r, "PRODUCT_UPDATE", "product", id, after.ProductName+": "+strings.Join(parts, ", "), changes)
	}
	httpx.JSONMessage(w, http.StatusOK, "Produk diperbarui", after)
}

// canEditProduct: data pusat (nama, harga pusat, foto, varian) hanya diubah super admin,
// kecuali menu yang dijual khusus di outlet admin itu (biasanya menu buatannya sendiri).
func (h *Handler) canEditProduct(ctx context.Context, productID string) error {
	a := adminFrom(ctx)
	if a.IsSuper() {
		return nil
	}
	var ownOnly bool
	err := h.pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM store_products WHERE product_id = $1 AND store_id = $2)
		   AND NOT EXISTS (SELECT 1 FROM store_products WHERE product_id = $1 AND store_id <> $2)`,
		productID, a.StoreID).Scan(&ownOnly)
	if err != nil {
		return err
	}
	if !ownOnly {
		return httpx.NewError(http.StatusForbidden, "FORBIDDEN",
			"Menu ini juga dijual di outlet lain. Ubah harga & stok outletmu lewat pengaturan outlet, atau minta super admin.")
	}
	return nil
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
