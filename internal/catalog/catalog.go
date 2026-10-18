// Package catalog: banner, kategori, dan produk (endpoint publik).
package catalog

import "time"

type Banner struct {
	ID       string
	Title    string
	Subtitle string
	ImageURL string
	Deeplink *string
}

type Category struct {
	ID   string
	Name string
	Icon string
}

type Option struct {
	ID         string
	Name       string
	ExtraPrice int64
}

type Product struct {
	ID          string
	CategoryID  string
	Name        string
	Description string
	// Price: harga yang berlaku sekarang (harga deal bila ada promo aktif, §5.24).
	Price int64
	// OriginalPrice: harga normal tanpa promo.
	OriginalPrice int64
	ImageURL      string
	Rating        float64
	SoldCount     int
	IsAvailable   bool
	IsBestSeller  bool
	Options       []Option
	UpdatedAt     time.Time
}

// OnDeal: produk sedang dijual dengan harga promo.
func (p Product) OnDeal() bool { return p.OriginalPrice > p.Price }

// DiscountPercent: potongan dibulatkan ke bawah, mis. 32.000 → 19.900 = 37%.
func (p Product) DiscountPercent() int {
	if !p.OnDeal() || p.OriginalPrice <= 0 {
		return 0
	}
	return int((p.OriginalPrice - p.Price) * 100 / p.OriginalPrice)
}

// Deal: promo produk yang tampil sebagai popup di Beranda (§5.24).
type Deal struct {
	ID       string
	Title    string
	Subtitle string
	Badge    string
	ImageURL string // sudah jatuh ke foto produk bila deal tidak punya gambar
	EndsAt   time.Time
	Product  Product
}

// FindOption mencari opsi berdasarkan id.
func (p Product) FindOption(id string) (Option, bool) {
	for _, o := range p.Options {
		if o.ID == id {
			return o, true
		}
	}
	return Option{}, false
}

// ProductFilter untuk GET /v1/products.
type ProductFilter struct {
	CategoryID string
	Query      string
	BestSeller bool
	Sort       string // "" | popular | rating | price_asc | price_desc
	Page       int
	Limit      int
	// StoreID: ketersediaan mengikuti outlet ini (§5.26). Harga tetap sama di semua outlet.
	StoreID string
}
