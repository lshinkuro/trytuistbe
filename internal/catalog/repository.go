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

func (r *Repository) ActiveBanners(ctx context.Context) ([]Banner, error) {
	rows, err := r.db.Query(ctx, `
		SELECT id, title, subtitle, image_url, deeplink
		FROM banners
		WHERE is_active
		  AND (starts_at IS NULL OR starts_at <= now())
		  AND (ends_at IS NULL OR ends_at >= now())
		ORDER BY sort_order, id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Banner, error) {
		var b Banner
		err := row.Scan(&b.ID, &b.Title, &b.Subtitle, &b.ImageURL, &b.Deeplink)
		return b, err
	})
}

// IsBannerActive dipakai aturan gratis ongkir (banner B2).
func (r *Repository) IsBannerActive(ctx context.Context, id string) (bool, error) {
	var ok bool
	err := r.db.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM banners WHERE id = $1 AND is_active
		  AND (starts_at IS NULL OR starts_at <= now()) AND (ends_at IS NULL OR ends_at >= now()))`, id).Scan(&ok)
	return ok, err
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

// activeDealPrice: harga deal termurah yang sedang berlaku untuk p (NULL bila tidak ada).
// Deal yang tidak lebih murah dari harga normal diabaikan.
const activeDealPrice = `(SELECT min(d.deal_price) FROM product_deals d
	WHERE d.product_id = p.id AND d.is_active AND d.starts_at <= now() AND d.ends_at > now()
	  AND d.deal_price < p.price)`

// effectivePrice: harga yang dibayar sekarang (dipakai juga untuk sort harga).
const effectivePrice = `COALESCE(` + activeDealPrice + `, p.price)`

const productColumns = `p.id, p.category_id, p.name, p.description, ` + effectivePrice + `, p.price, COALESCE(p.image_url, ''),
	p.rating::float8, p.sold_count, p.is_available, p.is_best_seller, p.updated_at`

func scanProduct(row pgx.CollectableRow) (Product, error) {
	var p Product
	err := row.Scan(&p.ID, &p.CategoryID, &p.Name, &p.Description, &p.Price, &p.OriginalPrice, &p.ImageURL,
		&p.Rating, &p.SoldCount, &p.IsAvailable, &p.IsBestSeller, &p.UpdatedAt)
	return p, err
}

// ListProducts mengembalikan produk sesuai filter beserta total sebelum paginasi.
func (r *Repository) ListProducts(ctx context.Context, f ProductFilter) ([]Product, int, error) {
	var where []string
	var args []any
	arg := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}
	// Produk dari kategori nonaktif disembunyikan.
	where = append(where, "c.is_active")
	if f.CategoryID != "" {
		where = append(where, "p.category_id = "+arg(f.CategoryID))
	}
	if q := strings.TrimSpace(f.Query); q != "" {
		like := "%" + escapeLike(q) + "%"
		ph := arg(like)
		where = append(where, fmt.Sprintf("(p.name ILIKE %s OR p.description ILIKE %s)", ph, ph))
	}
	if f.BestSeller {
		where = append(where, "p.is_best_seller")
	}
	whereSQL := "WHERE " + strings.Join(where, " AND ")

	var total int
	if err := r.db.QueryRow(ctx,
		`SELECT count(*) FROM products p JOIN categories c ON c.id = p.category_id `+whereSQL, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	order := "p.sort_order, p.id"
	switch f.Sort {
	case "popular":
		order = "p.sold_count DESC, p.sort_order"
	case "rating":
		order = "p.rating DESC, p.sold_count DESC, p.sort_order"
	case "price_asc":
		order = effectivePrice + " ASC, p.sort_order"
	case "price_desc":
		order = effectivePrice + " DESC, p.sort_order"
	}
	limit := arg(f.Limit)
	offset := arg((f.Page - 1) * f.Limit)
	rows, err := r.db.Query(ctx, `SELECT `+productColumns+`
		FROM products p JOIN categories c ON c.id = p.category_id `+whereSQL+`
		ORDER BY `+order+` LIMIT `+limit+` OFFSET `+offset, args...)
	if err != nil {
		return nil, 0, err
	}
	products, err := pgx.CollectRows(rows, scanProduct)
	if err != nil {
		return nil, 0, err
	}
	if err := r.attachOptions(ctx, products); err != nil {
		return nil, 0, err
	}
	return products, total, nil
}

func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// ProductByID mengembalikan produk beserta opsinya; ok=false bila tidak ada.
func (r *Repository) ProductByID(ctx context.Context, id string) (Product, bool, error) {
	products, err := r.ProductsByIDs(ctx, []string{id})
	if err != nil || len(products) == 0 {
		return Product{}, false, err
	}
	return products[id], true, nil
}

// ProductsByIDs mengembalikan map id → produk (dipakai checkout & favorit).
func (r *Repository) ProductsByIDs(ctx context.Context, ids []string) (map[string]Product, error) {
	rows, err := r.db.Query(ctx, `SELECT `+productColumns+` FROM products p WHERE p.id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	list, err := pgx.CollectRows(rows, scanProduct)
	if err != nil {
		return nil, err
	}
	if err := r.attachOptions(ctx, list); err != nil {
		return nil, err
	}
	out := make(map[string]Product, len(list))
	for _, p := range list {
		out[p.ID] = p
	}
	return out, nil
}

// PopupDeal: deal popup Beranda yang sedang berlaku; ok=false bila tidak ada.
// Produk habis / kategori nonaktif dilewati supaya popup tidak berujung "tidak tersedia".
func (r *Repository) PopupDeal(ctx context.Context) (Deal, bool, error) {
	var d Deal
	var productID string
	var image *string
	err := r.db.QueryRow(ctx, `
		SELECT d.id, d.title, d.subtitle, d.badge, d.image_url, d.ends_at, d.product_id
		FROM product_deals d
		JOIN products p ON p.id = d.product_id
		JOIN categories c ON c.id = p.category_id
		WHERE d.is_active AND d.show_as_popup AND d.starts_at <= now() AND d.ends_at > now()
		  AND d.deal_price < p.price AND p.is_available AND c.is_active
		ORDER BY d.sort_order, d.ends_at, d.id
		LIMIT 1`).Scan(&d.ID, &d.Title, &d.Subtitle, &d.Badge, &image, &d.EndsAt, &productID)
	if database.IsNoRows(err) {
		return Deal{}, false, nil
	}
	if err != nil {
		return Deal{}, false, err
	}
	p, ok, err := r.ProductByID(ctx, productID)
	if err != nil || !ok {
		return Deal{}, false, err
	}
	d.Product = p
	d.ImageURL = p.ImageURL
	if image != nil && *image != "" {
		d.ImageURL = *image
	}
	return d, true, nil
}

// FavoriteProducts: produk favorit user, terbaru di atas.
func (r *Repository) FavoriteProducts(ctx context.Context, userID string) ([]Product, error) {
	rows, err := r.db.Query(ctx, `SELECT `+productColumns+`
		FROM favorites f JOIN products p ON p.id = f.product_id
		WHERE f.user_id = $1
		ORDER BY f.created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	list, err := pgx.CollectRows(rows, scanProduct)
	if err != nil {
		return nil, err
	}
	return list, r.attachOptions(ctx, list)
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

// SectionProducts: produk untuk section Beranda. Produk habis tidak dimasukkan.
func (r *Repository) SectionProducts(ctx context.Context, rule string, manualIDs []string, limit int) ([]Product, error) {
	var (
		rows pgx.Rows
		err  error
	)
	base := `SELECT ` + productColumns + ` FROM products p JOIN categories c ON c.id = p.category_id
		WHERE p.is_available AND c.is_active `
	switch rule {
	case RuleBestSeller:
		rows, err = r.db.Query(ctx, base+`ORDER BY p.sold_count DESC, p.sort_order LIMIT $1`, limit)
	case RuleTopRated:
		rows, err = r.db.Query(ctx, base+`ORDER BY p.rating DESC, p.sold_count DESC, p.sort_order LIMIT $1`, limit)
	case RuleManual:
		if len(manualIDs) == 0 {
			return []Product{}, nil
		}
		rows, err = r.db.Query(ctx, base+`AND p.id = ANY($1) ORDER BY array_position($1::text[], p.id) LIMIT $2`, manualIDs, limit)
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
	return list, r.attachOptions(ctx, list)
}
