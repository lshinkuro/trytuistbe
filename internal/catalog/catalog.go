// Package catalog: banner, kategori, dan produk (endpoint publik).
package catalog

import "time"

// Aksi saat banner di-tap.
const (
	BannerNone         = "NONE"
	BannerProduct      = "PRODUCT"       // buka detail produk (ActionValue = id produk)
	BannerCategory     = "CATEGORY"      // buka menu kategori (ActionValue = id kategori)
	BannerFreeDelivery = "FREE_DELIVERY" // info kupon gratis ongkir mingguan
)

type Banner struct {
	ID          string
	Title       string
	Subtitle    string
	ImageURL    string
	ActionType  string
	ActionValue *string
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
	// Stock: sisa stok di outlet yang diminta; nil = tidak dihitung (atau tanpa outlet).
	Stock     *int
	Options   []Option
	UpdatedAt time.Time
	// Combo: isi paket (kosong = produk biasa) & total harga bila isinya dibeli satuan.
	Bundle        []BundleItem
	SeparatePrice int64
}

// BundleItem: satu isi combo, dengan harga & ketersediaan isi di outlet yang diminta.
type BundleItem struct {
	ProductID   string
	Name        string
	OptionID    *string
	OptionName  *string
	Quantity    int
	UnitPrice   int64 // harga satuan isi sekarang (termasuk varian & promo)
	IsAvailable bool
	Stock       *int
}

func (p Product) IsCombo() bool { return len(p.Bundle) > 0 }

// ComboSavings: hemat dibanding membeli isinya satuan (0 bila tidak lebih murah).
func (p Product) ComboSavings() int64 { return max(p.SeparatePrice-p.Price, 0) }

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
	// StoreID: menu, harga, promo & stok mengikuti outlet ini (§5.29). '' = katalog pusat.
	StoreID string
}
