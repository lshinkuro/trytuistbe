package catalog

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"trytuist-api/internal/database"
)

type Repository struct {
	db database.DBTX
}

func NewRepository(db database.DBTX) *Repository { return &Repository{db: db} }

// ActiveBanners: banner aktif dalam periodenya. Banner yang menjanjikan sesuatu yang sedang tidak berlaku
// disembunyikan: kupon gratis ongkir dimatikan, produk dihapus/nonaktif, kategori nonaktif.
func (r *Repository) ActiveBanners(ctx context.Context) ([]Banner, error) {
	rows, err := r.db.Query(ctx, `
		SELECT b.id, b.title, b.subtitle, b.image_url, b.action_type, b.action_value
		FROM banners b
		WHERE b.is_active
		  AND (b.starts_at IS NULL OR b.starts_at <= now())
		  AND (b.ends_at IS NULL OR b.ends_at >= now())
		  AND CASE b.action_type
		        WHEN 'FREE_DELIVERY' THEN COALESCE((SELECT (value->>'free_delivery_coupon_enabled')::boolean
		                                            FROM app_config WHERE key = 'order_rules'), FALSE)
		        WHEN 'PRODUCT' THEN EXISTS (SELECT 1 FROM products p JOIN categories c ON c.id = p.category_id
		                                    WHERE p.id = b.action_value AND c.is_active)
		        WHEN 'CATEGORY' THEN EXISTS (SELECT 1 FROM categories c WHERE c.id = b.action_value AND c.is_active)
		        ELSE TRUE END
		ORDER BY b.sort_order, b.id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Banner, error) {
		var b Banner
		err := row.Scan(&b.ID, &b.Title, &b.Subtitle, &b.ImageURL, &b.ActionType, &b.ActionValue)
		return b, err
	})
}

func (r *Repository) ActiveCategories(ctx context.Context) ([]Category, error) {
	rows, err := r.db.Query(ctx, `SELECT id, name, icon FROM categories WHERE is_active ORDER BY sort_order, id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Category, error) {
		var c Category
		err := row.Scan(&c.ID, &c.Name, &c.Icon)
		return c, err
	})
}

// productSQL: potongan SQL produk untuk satu outlet (§5.29). store = placeholder bertipe text;
// ” = tanpa outlet (harga pusat, promo semua outlet, semua menu dianggap dijual).
type productSQL struct {
	cols, from string
	price      string // harga yang dibayar sekarang (untuk sort)
	available  string // dijual, tidak dimatikan, dan stok belum 0
	sold       string // menu dijual di outlet ini (untuk daftar menu)
}

func productQuery(store string) productSQL {
	base := `COALESCE(sp.price, p.price)`
	deal := `(SELECT min(d.deal_price) FROM product_deals d
		WHERE d.product_id = p.id AND d.is_active AND d.starts_at <= now() AND d.ends_at > now()
		  AND (d.store_id IS NULL OR d.store_id = ` + store + `) AND d.deal_price < ` + base + `)`
	price := `COALESCE(` + deal + `, ` + base + `)`
	available := `(p.is_available AND (` + store + ` = '' OR (sp.product_id IS NOT NULL AND sp.is_available AND COALESCE(sp.stock, 1) > 0)))`
	return productSQL{
		cols: `p.id, p.category_id, p.name, p.description, ` + price + `, ` + base + `, COALESCE(p.image_url, ''),
			p.rating::float8, p.sold_count, ` + available + `, p.is_best_seller, p.updated_at, sp.stock`,
		from:      `products p LEFT JOIN store_products sp ON sp.product_id = p.id AND sp.store_id = ` + store,
		price:     price,
		available: available,
		sold:      `(` + store + ` = '' OR sp.product_id IS NOT NULL)`,
	}
}

func scanProduct(row pgx.CollectableRow) (Product, error) {
	var p Product
	err := row.Scan(&p.ID, &p.CategoryID, &p.Name, &p.Description, &p.Price, &p.OriginalPrice, &p.ImageURL,
		&p.Rating, &p.SoldCount, &p.IsAvailable, &p.IsBestSeller, &p.UpdatedAt, &p.Stock)
	return p, err
}

// ListProducts mengembalikan produk sesuai filter beserta total sebelum paginasi.
// Dengan StoreID: hanya menu yang dijual di outlet itu, dengan harga & stok outlet.
func (r *Repository) ListProducts(ctx context.Context, f ProductFilter) ([]Product, int, error) {
	var where []string
	var args []any
	arg := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}
	q := productQuery(arg(f.StoreID) + "::text")
	// Produk dari kategori nonaktif disembunyikan.
	where = append(where, "c.is_active", q.sold)
	if f.CategoryID != "" {
		where = append(where, "p.category_id = "+arg(f.CategoryID))
	}
	if text := strings.TrimSpace(f.Query); text != "" {
		like := "%" + escapeLike(text) + "%"
		ph := arg(like)
		where = append(where, fmt.Sprintf("(p.name ILIKE %s OR p.description ILIKE %s)", ph, ph))
	}
	if f.BestSeller {
		where = append(where, "p.is_best_seller")
	}
	fromSQL := " FROM " + q.from + " JOIN categories c ON c.id = p.category_id WHERE " + strings.Join(where, " AND ")

	var total int
	if err := r.db.QueryRow(ctx, `SELECT count(*)`+fromSQL, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	order := "p.sort_order, p.id"
	switch f.Sort {
	case "popular":
		order = "p.sold_count DESC, p.sort_order"
	case "rating":
		order = "p.rating DESC, p.sold_count DESC, p.sort_order"
	case "price_asc":
		order = q.price + " ASC, p.sort_order"
	case "price_desc":
		order = q.price + " DESC, p.sort_order"
	}
	limit := arg(f.Limit)
	offset := arg((f.Page - 1) * f.Limit)
	rows, err := r.db.Query(ctx, `SELECT `+q.cols+fromSQL+` ORDER BY `+order+` LIMIT `+limit+` OFFSET `+offset, args...)
	if err != nil {
		return nil, 0, err
	}
	products, err := pgx.CollectRows(rows, scanProduct)
	if err != nil {
		return nil, 0, err
	}
	if err := r.attachExtras(ctx, products, f.StoreID); err != nil {
		return nil, 0, err
	}
	return products, total, nil
}

func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// ProductByID mengembalikan produk beserta opsinya; ok=false bila tidak ada.
func (r *Repository) ProductByID(ctx context.Context, id, storeID string) (Product, bool, error) {
	products, err := r.ProductsByIDs(ctx, []string{id}, storeID)
	if err != nil || len(products) == 0 {
		return Product{}, false, err
	}
	return products[id], true, nil
}

// ProductsByIDs mengembalikan map id → produk (dipakai checkout & favorit).
// Menu yang tidak dijual di storeID tetap dikembalikan dengan IsAvailable=false.
func (r *Repository) ProductsByIDs(ctx context.Context, ids []string, storeID string) (map[string]Product, error) {
	list, err := r.productList(ctx, ids, storeID, true)
	if err != nil {
		return nil, err
	}
	out := make(map[string]Product, len(list))
	for _, p := range list {
		out[p.ID] = p
	}
	return out, nil
}

// PopupDeal: deal popup Beranda yang sedang berlaku di outlet ini (” = promo semua outlet saja);
// ok=false bila tidak ada. Menu habis / tidak dijual / kategori nonaktif dilewati supaya
// popup tidak berujung "tidak tersedia".
func (r *Repository) PopupDeal(ctx context.Context, storeID string) (Deal, bool, error) {
	rows, err := r.db.Query(ctx, `
		SELECT d.id, d.title, d.subtitle, d.badge, d.image_url, d.ends_at, d.product_id
		FROM product_deals d
		JOIN products p ON p.id = d.product_id
		JOIN categories c ON c.id = p.category_id
		WHERE d.is_active AND d.show_as_popup AND d.starts_at <= now() AND d.ends_at > now()
		  AND (d.store_id IS NULL OR d.store_id = $1) AND c.is_active
		ORDER BY d.sort_order, d.ends_at, d.id
		LIMIT 10`, storeID)
	if err != nil {
		return Deal{}, false, err
	}
	type candidate struct {
		deal      Deal
		image     *string
		productID string
	}
	list, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (candidate, error) {
		var c candidate
		err := row.Scan(&c.deal.ID, &c.deal.Title, &c.deal.Subtitle, &c.deal.Badge, &c.image, &c.deal.EndsAt, &c.productID)
		return c, err
	})
	if err != nil {
		return Deal{}, false, err
	}
	for _, c := range list {
		p, ok, err := r.ProductByID(ctx, c.productID, storeID)
		if err != nil {
			return Deal{}, false, err
		}
		if !ok || !p.IsAvailable || !p.OnDeal() {
			continue
		}
		d := c.deal
		d.Product = p
		d.ImageURL = p.ImageURL
		if c.image != nil && *c.image != "" {
			d.ImageURL = *c.image
		}
		return d, true, nil
	}
	return Deal{}, false, nil
}

// FavoriteProducts: produk favorit user, terbaru di atas. Harga & stok mengikuti storeID (” = pusat).
func (r *Repository) FavoriteProducts(ctx context.Context, userID, storeID string) ([]Product, error) {
	q := productQuery("$2::text")
	rows, err := r.db.Query(ctx, `SELECT `+q.cols+`
		FROM `+q.from+` JOIN favorites f ON f.product_id = p.id
		WHERE f.user_id = $1
		ORDER BY f.created_at DESC`, userID, storeID)
	if err != nil {
		return nil, err
	}
	list, err := pgx.CollectRows(rows, scanProduct)
	if err != nil {
		return nil, err
	}
	return list, r.attachExtras(ctx, list, storeID)
}

// productList: produk berdasarkan id; withBundles=false dipakai saat memuat isi combo (tidak bersarang).
func (r *Repository) productList(ctx context.Context, ids []string, storeID string, withBundles bool) ([]Product, error) {
	q := productQuery("$2::text")
	rows, err := r.db.Query(ctx, `SELECT `+q.cols+` FROM `+q.from+` WHERE p.id = ANY($1)`, ids, storeID)
	if err != nil {
		return nil, err
	}
	list, err := pgx.CollectRows(rows, scanProduct)
	if err != nil {
		return nil, err
	}
	if !withBundles {
		return list, r.attachOptions(ctx, list)
	}
	return list, r.attachExtras(ctx, list, storeID)
}

// attachExtras: varian + isi combo (harga, ketersediaan & stok isi mengikuti outlet).
func (r *Repository) attachExtras(ctx context.Context, products []Product, storeID string) error {
	if err := r.attachOptions(ctx, products); err != nil {
		return err
	}
	return r.attachBundles(ctx, products, storeID)
}

// attachBundles: combo hanya bisa dipesan bila semua isinya tersedia; stok combo = stok isi yang paling sedikit.
func (r *Repository) attachBundles(ctx context.Context, products []Product, storeID string) error {
	if len(products) == 0 {
		return nil
	}
	ids := make([]string, len(products))
	idx := make(map[string]int, len(products))
	for i, p := range products {
		ids[i], idx[p.ID] = p.ID, i
	}
	type row struct {
		bundleID, productID string
		optionID            *string
		quantity            int
	}
	rows, err := r.db.Query(ctx, `
		SELECT bundle_id, product_id, option_id, quantity FROM product_bundle_items
		WHERE bundle_id = ANY($1) ORDER BY bundle_id, sort_order, product_id`, ids)
	if err != nil {
		return err
	}
	items, err := pgx.CollectRows(rows, func(rw pgx.CollectableRow) (row, error) {
		var it row
		err := rw.Scan(&it.bundleID, &it.productID, &it.optionID, &it.quantity)
		return it, err
	})
	if err != nil || len(items) == 0 {
		return err
	}
	compIDs := make([]string, 0, len(items))
	for _, it := range items {
		compIDs = append(compIDs, it.productID)
	}
	comps, err := r.productList(ctx, compIDs, storeID, false)
	if err != nil {
		return err
	}
	byID := make(map[string]Product, len(comps))
	for _, c := range comps {
		byID[c.ID] = c
	}
	for _, it := range items {
		p := &products[idx[it.bundleID]]
		c, ok := byID[it.productID]
		bi := BundleItem{ProductID: it.productID, Name: c.Name, OptionID: it.optionID, Quantity: it.quantity,
			UnitPrice: c.Price, IsAvailable: ok && c.IsAvailable, Stock: c.Stock}
		if it.optionID != nil {
			if o, found := c.FindOption(*it.optionID); found {
				name := o.Name
				bi.OptionName, bi.UnitPrice = &name, c.Price+o.ExtraPrice
			} else {
				bi.IsAvailable = false // varian isi sudah dihapus
			}
		}
		p.Bundle = append(p.Bundle, bi)
		p.SeparatePrice += bi.UnitPrice * int64(bi.Quantity)
		if !bi.IsAvailable {
			p.IsAvailable = false
		}
		if bi.Stock != nil {
			left := *bi.Stock / bi.Quantity
			if p.Stock == nil || left < *p.Stock {
				p.Stock = &left
			}
			if left == 0 {
				p.IsAvailable = false
			}
		}
	}
	return nil
}

func (r *Repository) attachOptions(ctx context.Context, products []Product) error {
	if len(products) == 0 {
		return nil
	}
	ids := make([]string, len(products))
	idx := make(map[string]int, len(products))
	for i, p := range products {
		ids[i] = p.ID
		idx[p.ID] = i
		products[i].Options = []Option{}
	}
	rows, err := r.db.Query(ctx, `
		SELECT product_id, id, name, extra_price
		FROM product_options WHERE product_id = ANY($1)
		ORDER BY product_id, sort_order, id`, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var pid string
		var o Option
		if err := rows.Scan(&pid, &o.ID, &o.Name, &o.ExtraPrice); err != nil {
			return err
		}
		i := idx[pid]
		products[i].Options = append(products[i].Options, o)
	}
	return rows.Err()
}

// Aturan isi section produk Beranda (home_sections.rule).
const (
	RuleBestSeller = "BEST_SELLER"
	RuleTopRated   = "TOP_RATED"
	RuleManual     = "MANUAL"
)

// SectionProducts: produk untuk section Beranda di outlet storeID. Produk habis / tidak dijual tidak dimasukkan.
func (r *Repository) SectionProducts(ctx context.Context, rule string, manualIDs []string, limit int, storeID string) ([]Product, error) {
	var (
		rows pgx.Rows
		err  error
	)
	q := productQuery("$1::text")
	base := `SELECT ` + q.cols + ` FROM ` + q.from + ` JOIN categories c ON c.id = p.category_id
		WHERE ` + q.available + ` AND c.is_active `
	switch rule {
	case RuleBestSeller:
		rows, err = r.db.Query(ctx, base+`ORDER BY p.sold_count DESC, p.sort_order LIMIT $2`, storeID, limit)
	case RuleTopRated:
		rows, err = r.db.Query(ctx, base+`ORDER BY p.rating DESC, p.sold_count DESC, p.sort_order LIMIT $2`, storeID, limit)
	case RuleManual:
		if len(manualIDs) == 0 {
			return []Product{}, nil
		}
		rows, err = r.db.Query(ctx, base+`AND p.id = ANY($3) ORDER BY array_position($3::text[], p.id) LIMIT $2`, storeID, limit, manualIDs)
	default:
		return []Product{}, nil
	}
	if err != nil {
		return nil, err
	}
	list, err := pgx.CollectRows(rows, scanProduct)
	if err != nil {
		return nil, err
	}
	if err := r.attachExtras(ctx, list, storeID); err != nil {
		return nil, err
	}
	// Combo yang isinya habis tidak tampil di Beranda (SQL di atas belum tahu isi combo).
	out := list[:0]
	for _, p := range list {
		if p.IsAvailable {
			out = append(out, p)
		}
	}
	return out, nil
}
