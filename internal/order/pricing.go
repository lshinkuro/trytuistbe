// Package order: quote, checkout, riwayat, pembatalan, dan rating pesanan.
package order

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"trytuist-api/internal/appconfig"
	"trytuist-api/internal/catalog"
	"trytuist-api/internal/httpx"
)

// Rules: batas jumlah & catatan per item. Diambil dari app_config.order_rules
// (§5.19), jadi sama dengan yang dipakai iOS.
type Rules struct {
	MinQuantity  int
	MaxQuantity  int
	NoteMaxChars int
}

// DefaultRules = AddToCartUseCase.quantityRange & ProductDetailState.noteLimit.
var DefaultRules = Rules{MinQuantity: 1, MaxQuantity: 20, NoteMaxChars: 100}

type ItemInput struct {
	ProductID string
	OptionID  string // "" = tanpa opsi / pakai opsi default
	Quantity  int
	Note      string
}

type Line struct {
	ProductID   string
	ProductName string
	OptionID    *string
	OptionName  *string
	UnitPrice   int64
	Quantity    int
	Note        string
	Subtotal    int64
	// Isi combo per 1 paket (snapshot); kosong untuk produk biasa.
	Bundle []BundleLine
}

// BundleLine: satu isi combo yang disimpan di order_items.bundle_items.
type BundleLine struct {
	ProductID  string  `json:"product_id"`
	Name       string  `json:"product_name"`
	OptionID   *string `json:"option_id"`
	OptionName *string `json:"option_name"`
	Quantity   int     `json:"quantity"`
}

type Voucher struct {
	ID            string
	Code          string
	Title         string
	Description   string
	DiscountType  string // PERCENT | FIXED
	DiscountValue int64
	MinSubtotal   int64
	MaxDiscount   *int64
	StartsAt      time.Time
	EndsAt        time.Time
	Quota         *int
}

// Quote adalah hasil perhitungan harga di server.
type Quote struct {
	StoreID     string
	StoreName   string
	AddressID   *string
	Lines       []Line
	Subtotal    int64
	DeliveryFee int64
	Discount    int64
	Total       int64
	Voucher     *Voucher
	// Jarak alamat → outlet & estimasi waktu antar; nil bila alamat tanpa koordinat.
	DistanceKM *float64
	ETA        *ETA
	// Kupon gratis ongkir dipakai: DeliveryFee = 0, DeliveryFeeWaived = ongkir yang ditanggung toko.
	FreeDeliveryCoupon bool
	DeliveryFeeWaived  int64
	// Sisa kupon user minggu ini (untuk tampilan keranjang); nil bila tidak dihitung.
	FreeDelivery *FreeDeliveryStatus
}

// FreeDeliveryStatus: kuota kupon gratis ongkir user minggu ini.
type FreeDeliveryStatus struct {
	Enabled     bool
	WeeklyQuota int
	Used        int
	ResetsAt    time.Time
}

func (f FreeDeliveryStatus) Remaining() int {
	if !f.Enabled {
		return 0
	}
	return max(f.WeeklyQuota-f.Used, 0)
}

// WeekStart: Senin 00:00 minggu ini di zona waktu toko (kuota kupon direset tiap Senin).
func WeekStart(now time.Time, loc *time.Location) time.Time {
	y, m, d := now.In(loc).Date()
	day := time.Date(y, m, d, 0, 0, 0, 0, loc)
	offset := (int(day.Weekday()) + 6) % 7 // Senin = 0
	return day.AddDate(0, 0, -offset)
}

// ETA: perkiraan waktu sampai dalam menit sejak pesanan dibuat.
type ETA struct {
	MinMinutes int
	MaxMinutes int
}

// BuildLines menghitung harga per item dari data produk di DB. Harga dari
// client tidak pernah dipakai.
func BuildLines(items []ItemInput, products map[string]catalog.Product, rules Rules) ([]Line, int64, error) {
	ordered := map[string]int{}
	if len(items) == 0 {
		return nil, 0, httpx.Unprocessable("EMPTY_CART", "Keranjang masih kosong.")
	}
	lines := make([]Line, 0, len(items))
	var subtotal int64
	for i, it := range items {
		if it.Quantity < rules.MinQuantity || it.Quantity > rules.MaxQuantity {
			return nil, 0, &httpx.AppError{
				Status: 422, Type: "INVALID_QUANTITY",
				Message: fmt.Sprintf("Jumlah per item harus %d–%d.", rules.MinQuantity, rules.MaxQuantity),
				Details: []httpx.FieldDetail{{Field: fmt.Sprintf("items[%d].quantity", i), Message: "di luar batas"}},
			}
		}
		if n := len([]rune(it.Note)); n > rules.NoteMaxChars {
			return nil, 0, httpx.Validation("Catatan item terlalu panjang.",
				httpx.FieldDetail{Field: fmt.Sprintf("items[%d].note", i), Message: fmt.Sprintf("maksimal %d karakter", rules.NoteMaxChars)})
		}
		p, ok := products[it.ProductID]
		if !ok {
			return nil, 0, &httpx.AppError{
				Status: 422, Type: "PRODUCT_UNAVAILABLE",
				Message: "Ada produk yang sudah tidak tersedia.",
				Details: []httpx.FieldDetail{{Field: fmt.Sprintf("items[%d].product_id", i), Message: "produk tidak ditemukan"}},
			}
		}
		if !p.IsAvailable {
			return nil, 0, &httpx.AppError{
				Status: 422, Type: "PRODUCT_UNAVAILABLE",
				Message: p.Name + " sedang habis.",
				Details: []httpx.FieldDetail{{Field: fmt.Sprintf("items[%d].product_id", i), Message: "habis"}},
			}
		}
		// Stok outlet dihitung per produk (semua varian & catatan digabung).
		ordered[p.ID] += it.Quantity
		if p.Stock != nil && ordered[p.ID] > *p.Stock {
			return nil, 0, OutOfStock(p.Name, *p.Stock, i)
		}
		// Combo memakai stok isinya; isi yang sama di baris lain (satuan / combo lain) ikut dihitung.
		var bundle []BundleLine
		for _, b := range p.Bundle {
			ordered[b.ProductID] += it.Quantity * b.Quantity
			if b.Stock != nil && ordered[b.ProductID] > *b.Stock {
				return nil, 0, OutOfStock(b.Name, *b.Stock, i)
			}
			bundle = append(bundle, BundleLine{ProductID: b.ProductID, Name: b.Name, OptionID: b.OptionID,
				OptionName: b.OptionName, Quantity: b.Quantity})
		}
		unit := p.Price
		line := Line{ProductID: p.ID, ProductName: p.Name, Quantity: it.Quantity, Note: strings.TrimSpace(it.Note), Bundle: bundle}
		if len(p.Options) > 0 {
			opt := p.Options[0] // default: opsi pertama (mis. Regular / Normal)
			if it.OptionID != "" {
				var found bool
				if opt, found = p.FindOption(it.OptionID); !found {
					return nil, 0, httpx.Validation("Varian produk tidak valid.",
						httpx.FieldDetail{Field: fmt.Sprintf("items[%d].option_id", i), Message: "tidak tersedia untuk " + p.Name})
				}
			}
			unit += opt.ExtraPrice
			id, name := opt.ID, opt.Name
			line.OptionID, line.OptionName = &id, &name
		} else if it.OptionID != "" {
			return nil, 0, httpx.Validation("Varian produk tidak valid.",
				httpx.FieldDetail{Field: fmt.Sprintf("items[%d].option_id", i), Message: p.Name + " tidak punya varian"})
		}
		line.UnitPrice = unit
		line.Subtotal = unit * int64(it.Quantity)
		subtotal += line.Subtotal
		lines = append(lines, line)
	}
	return lines, subtotal, nil
}

// OutOfStock: jumlah melebihi sisa stok outlet (§5.29).
func OutOfStock(name string, left, item int) *httpx.AppError {
	return &httpx.AppError{
		Status: 422, Type: "OUT_OF_STOCK",
		Message: fmt.Sprintf("Stok %s tinggal %d. Kurangi jumlahnya ya.", name, left),
		Details: []httpx.FieldDetail{{Field: fmt.Sprintf("items[%d].quantity", item), Message: fmt.Sprintf("maksimal %d", left)}},
	}
}

// DeliveryFee: r.DeliveryFee sampai r.DeliveryBaseKM, lalu + r.DeliveryFeePerKM tiap km berikutnya
// (dibulatkan ke atas), maks. r.DeliveryFeeMax. distanceKM nil = tarif dasar.
// Gratis bila r.FreeDeliveryMinSubtotal > 0 dan subtotal ≥ batas itu.
func DeliveryFee(subtotal int64, distanceKM *float64, r appconfig.OrderRules) int64 {
	if r.FreeDeliveryMinSubtotal > 0 && subtotal >= r.FreeDeliveryMinSubtotal {
		return 0
	}
	fee := r.DeliveryFee
	if distanceKM != nil && r.DeliveryFeePerKM > 0 {
		if extra := math.Ceil(*distanceKM - r.DeliveryBaseKM); extra > 0 {
			fee += int64(extra) * r.DeliveryFeePerKM
		}
	}
	if r.DeliveryFeeMax > 0 && fee > r.DeliveryFeeMax {
		fee = r.DeliveryFeeMax
	}
	return fee
}

// DeliveryETA: waktu siapkan + perjalanan, dibulatkan ke atas ke kelipatan 5 menit; rentang 10 menit.
func DeliveryETA(distanceKM float64, r appconfig.OrderRules) ETA {
	speed := r.CourierSpeedKMH
	if speed <= 0 {
		speed = 20
	}
	total := r.PrepMinutes + int(math.Ceil(distanceKM/speed*60))
	low := (total + 4) / 5 * 5
	return ETA{MinMinutes: low, MaxMinutes: low + 10}
}

// CheckVoucher memvalidasi periode & minimal belanja.
func CheckVoucher(v Voucher, subtotal int64, now time.Time) error {
	if now.Before(v.StartsAt) || now.After(v.EndsAt) {
		return invalidVoucher("Voucher tidak berlaku atau sudah kedaluwarsa.")
	}
	if subtotal < v.MinSubtotal {
		return invalidVoucher("Minimal belanja " + Rupiah(v.MinSubtotal) + " untuk voucher ini.")
	}
	return nil
}

// Discount menghitung potongan; tidak pernah melebihi subtotal.
func Discount(v Voucher, subtotal int64) int64 {
	var d int64
	switch v.DiscountType {
	case "PERCENT":
		d = subtotal * v.DiscountValue / 100
	case "FIXED":
		d = v.DiscountValue
	}
	if v.MaxDiscount != nil && d > *v.MaxDiscount {
		d = *v.MaxDiscount
	}
	if d > subtotal {
		d = subtotal
	}
	if d < 0 {
		d = 0
	}
	return d
}

func invalidVoucher(msg string) error { return httpx.Unprocessable("INVALID_VOUCHER", msg) }

// Rupiah: 15000 → "Rp 15.000".
func Rupiah(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte('.')
		}
		b.WriteRune(c)
	}
	if neg {
		return "-Rp " + b.String()
	}
	return "Rp " + b.String()
}
