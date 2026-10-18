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
	ID           string
	CategoryID   string
	Name         string
	Description  string
	Price        int64
	ImageURL     string
	Rating       float64
	SoldCount    int
	IsAvailable  bool
	IsBestSeller bool
	Options      []Option
	UpdatedAt    time.Time
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
}
