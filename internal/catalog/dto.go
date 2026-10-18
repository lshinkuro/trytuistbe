package catalog

// DTO JSON — nama field sama persis dengan DTO di iOS (lihat API_PLAN.md §5).

type BannerDTO struct {
	ID       string  `json:"id"`
	Title    string  `json:"title"`
	Subtitle string  `json:"subtitle"`
	ImageURL string  `json:"image_url"`
	Deeplink *string `json:"deeplink,omitempty"`
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
	ProductID   string      `json:"product_id"`
	ProductName string      `json:"product_name"`
	Description string      `json:"description"`
	CategoryID  string      `json:"category_id"`
	Price       int64       `json:"price"`
	ImageURL    string      `json:"image_url"`
	Rating      float64     `json:"rating"`
	SoldCount   int         `json:"sold_count"`
	IsAvailable bool        `json:"is_available"`
	Tags        []string    `json:"tags"`
	Options     []OptionDTO `json:"options"`
}

func ToBannerDTOs(in []Banner) []BannerDTO {
	out := make([]BannerDTO, len(in))
	for i, b := range in {
		out[i] = BannerDTO{ID: b.ID, Title: b.Title, Subtitle: b.Subtitle, ImageURL: b.ImageURL, Deeplink: b.Deeplink}
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
	opts := make([]OptionDTO, len(p.Options))
	for i, o := range p.Options {
		opts[i] = OptionDTO{ID: o.ID, Name: o.Name, ExtraPrice: o.ExtraPrice}
	}
	return ProductDTO{
		ProductID:   p.ID,
		ProductName: p.Name,
		Description: p.Description,
		CategoryID:  p.CategoryID,
		Price:       p.Price,
		ImageURL:    p.ImageURL,
		Rating:      p.Rating,
		SoldCount:   p.SoldCount,
		IsAvailable: p.IsAvailable,
		Tags:        tags,
		Options:     opts,
	}
}

func ToProductDTOs(in []Product) []ProductDTO {
	out := make([]ProductDTO, len(in))
	for i, p := range in {
		out[i] = ToProductDTO(p)
	}
	return out
}
