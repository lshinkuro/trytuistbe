package catalog

import (
	"fmt"

	"trytuist-api/internal/httpx"
)

// DTO JSON — nama field sama persis dengan DTO di iOS (lihat API_PLAN.md §5).

type BannerDTO struct {
	ID       string          `json:"id"`
	Title    string          `json:"title"`
	Subtitle string          `json:"subtitle"`
	ImageURL string          `json:"image_url"`
	Action   BannerActionDTO `json:"action"`
}

// BannerActionDTO: type NONE | PRODUCT | CATEGORY | FREE_DELIVERY; value = id produk/kategori.
type BannerActionDTO struct {
	Type  string  `json:"type"`
	Value *string `json:"value"`
}

type CategoryDTO struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Icon string `json:"icon"`
}

type OptionDTO struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	ExtraPrice int64  `json:"extra_price"`
}

type ProductDTO struct {
	ProductID   string `json:"product_id"`
	ProductName string `json:"product_name"`
	Description string `json:"description"`
	CategoryID  string `json:"category_id"`
	Price       int64  `json:"price"` // harga berlaku (sudah termasuk promo)
	// OriginalPrice: harga normal, hanya diisi bila produk sedang promo (§5.24).
	OriginalPrice   *int64      `json:"original_price"`
	DiscountPercent int         `json:"discount_percent"`
	ImageURL        string      `json:"image_url"`
	Rating          float64     `json:"rating"`
	SoldCount       int         `json:"sold_count"`
	IsAvailable     bool        `json:"is_available"`
	Tags            []string    `json:"tags"`
	Options         []OptionDTO `json:"options"`
	// Combo: isi paket & hemat dibanding beli satuan; null untuk produk biasa.
	Combo *ComboDTO `json:"combo"`
}

type ComboDTO struct {
	Items         []ComboItemDTO `json:"items"`
	SeparatePrice int64          `json:"separate_price"` // total bila isinya dibeli satuan
	Savings       int64          `json:"savings"`
}

type ComboItemDTO struct {
	ProductID   string  `json:"product_id"`
	ProductName string  `json:"product_name"`
	OptionID    *string `json:"option_id"`
	OptionName  *string `json:"option_name"`
	Quantity    int     `json:"quantity"`
}

func ToBannerDTOs(in []Banner) []BannerDTO {
	out := make([]BannerDTO, len(in))
	for i, b := range in {
		out[i] = BannerDTO{ID: b.ID, Title: b.Title, Subtitle: b.Subtitle, ImageURL: b.ImageURL,
			Action: BannerActionDTO{Type: b.ActionType, Value: b.ActionValue}}
	}
	return out
}

func ToCategoryDTOs(in []Category) []CategoryDTO {
	out := make([]CategoryDTO, len(in))
	for i, c := range in {
		out[i] = CategoryDTO{ID: c.ID, Name: c.Name, Icon: c.Icon}
	}
	return out
}

func ToProductDTO(p Product) ProductDTO {
	tags := []string{}
	if p.IsBestSeller {
		tags = append(tags, "best_seller")
	}
	var original *int64
	var combo *ComboDTO
	if p.IsCombo() {
		tags = append(tags, "combo")
		combo = &ComboDTO{SeparatePrice: p.SeparatePrice, Savings: p.ComboSavings(), Items: make([]ComboItemDTO, len(p.Bundle))}
		for i, b := range p.Bundle {
			combo.Items[i] = ComboItemDTO{ProductID: b.ProductID, ProductName: b.Name, OptionID: b.OptionID,
				OptionName: b.OptionName, Quantity: b.Quantity}
		}
	}
	if p.OnDeal() {
		tags = append(tags, "promo")
		v := p.OriginalPrice
		original = &v
	}
	opts := make([]OptionDTO, len(p.Options))
	for i, o := range p.Options {
		opts[i] = OptionDTO{ID: o.ID, Name: o.Name, ExtraPrice: o.ExtraPrice}
	}
	return ProductDTO{
		ProductID:       p.ID,
		ProductName:     p.Name,
		Description:     p.Description,
		CategoryID:      p.CategoryID,
		Price:           p.Price,
		OriginalPrice:   original,
		DiscountPercent: p.DiscountPercent(),
		ImageURL:        p.ImageURL,
		Rating:          p.Rating,
		SoldCount:       p.SoldCount,
		IsAvailable:     p.IsAvailable,
		Tags:            tags,
		Options:         opts,
		Combo:           combo,
	}
}

func ToProductDTOs(in []Product) []ProductDTO {
	out := make([]ProductDTO, len(in))
	for i, p := range in {
		out[i] = ToProductDTO(p)
	}
	return out
}

// PopupDealDTO: GET /v1/promotions/popup (§5.24).
type PopupDealDTO struct {
	ID       string     `json:"id"`
	Title    string     `json:"title"`
	Subtitle string     `json:"subtitle"`
	Badge    string     `json:"badge"`
	ImageURL string     `json:"image_url"`
	EndsAt   string     `json:"ends_at"`
	Product  ProductDTO `json:"product"`
}

func ToPopupDealDTO(d Deal) PopupDealDTO {
	badge := d.Badge
	if badge == "" {
		badge = fmt.Sprintf("HEMAT %d%%", d.Product.DiscountPercent())
	}
	return PopupDealDTO{
		ID: d.ID, Title: d.Title, Subtitle: d.Subtitle, Badge: badge, ImageURL: d.ImageURL,
		EndsAt: httpx.ISO(d.EndsAt), Product: ToProductDTO(d.Product),
	}
}
