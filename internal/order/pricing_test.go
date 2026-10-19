package order

import (
	"errors"
	"testing"
	"time"

	"trytuist-api/internal/appconfig"
	"trytuist-api/internal/catalog"
	"trytuist-api/internal/httpx"
)

func fixtureProducts() map[string]catalog.Product {
	return map[string]catalog.Product{
		"P01": {ID: "P01", Name: "Es Kopi Susu Gula Aren", Price: 22000, IsAvailable: true,
			Options: []catalog.Option{{ID: "regular", Name: "Regular"}, {ID: "large", Name: "Large", ExtraPrice: 5000}}},
		"P06": {ID: "P06", Name: "Hot Matcha", Price: 25000, IsAvailable: false},
		"P11": {ID: "P11", Name: "Classic Beef Burger", Price: 42000, IsAvailable: true},
	}
}

func errType(err error) string {
	var a *httpx.AppError
	if errors.As(err, &a) {
		return a.Type
	}
	return ""
}

func TestBuildLines_PlanExample(t *testing.T) {
	lines, subtotal, err := BuildLines([]ItemInput{
		{ProductID: "P01", OptionID: "large", Quantity: 2, Note: "less sugar"},
		{ProductID: "P11", Quantity: 1},
	}, fixtureProducts(), DefaultRules)
	if err != nil {
		t.Fatal(err)
	}
	if subtotal != 96000 {
		t.Fatalf("subtotal = %d, want 96000", subtotal)
	}
	if lines[0].UnitPrice != 27000 || lines[0].Subtotal != 54000 || *lines[0].OptionName != "Large" {
		t.Fatalf("line 0 = %+v", lines[0])
	}
	if lines[1].OptionID != nil {
		t.Fatalf("produk tanpa opsi harus option_id null")
	}
}

func TestBuildLines_DefaultOption(t *testing.T) {
	lines, _, err := BuildLines([]ItemInput{{ProductID: "P01", Quantity: 1}}, fixtureProducts(), DefaultRules)
	if err != nil {
		t.Fatal(err)
	}
	if *lines[0].OptionID != "regular" || lines[0].UnitPrice != 22000 {
		t.Fatalf("default option salah: %+v", lines[0])
	}
}

func TestBuildLines_Errors(t *testing.T) {
	long := make([]rune, 101)
	for i := range long {
		long[i] = 'a'
	}
	cases := []struct {
		name  string
		items []ItemInput
		want  string
	}{
		{"empty", nil, "EMPTY_CART"},
		{"qty 0", []ItemInput{{ProductID: "P01", Quantity: 0}}, "INVALID_QUANTITY"},
		{"qty 21", []ItemInput{{ProductID: "P01", Quantity: 21}}, "INVALID_QUANTITY"},
		{"habis", []ItemInput{{ProductID: "P06", Quantity: 1}}, "PRODUCT_UNAVAILABLE"},
		{"tidak ada", []ItemInput{{ProductID: "P99", Quantity: 1}}, "PRODUCT_UNAVAILABLE"},
		{"opsi salah", []ItemInput{{ProductID: "P01", OptionID: "jumbo", Quantity: 1}}, "VALIDATION_ERROR"},
		{"catatan panjang", []ItemInput{{ProductID: "P11", Quantity: 1, Note: string(long)}}, "VALIDATION_ERROR"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := BuildLines(tc.items, fixtureProducts(), DefaultRules)
			if got := errType(err); got != tc.want {
				t.Fatalf("error type = %q, want %q (err=%v)", got, tc.want, err)
			}
		})
	}
}

func TestDiscount(t *testing.T) {
	max := int64(20000)
	pct := Voucher{DiscountType: "PERCENT", DiscountValue: 10, MaxDiscount: &max}
	if d := Discount(pct, 96000); d != 9600 {
		t.Fatalf("10%% dari 96000 = %d", d)
	}
	if d := Discount(pct, 500000); d != 20000 {
		t.Fatalf("harus dibatasi max_discount, dapat %d", d)
	}
	fixed := Voucher{DiscountType: "FIXED", DiscountValue: 15000}
	if d := Discount(fixed, 10000); d != 10000 {
		t.Fatalf("diskon tidak boleh melebihi subtotal, dapat %d", d)
	}
}

func TestCheckVoucher(t *testing.T) {
	now := time.Now()
	v := Voucher{MinSubtotal: 30000, StartsAt: now.Add(-time.Hour), EndsAt: now.Add(time.Hour)}
	if err := CheckVoucher(v, 30000, now); err != nil {
		t.Fatal(err)
	}
	if errType(CheckVoucher(v, 29999, now)) != "INVALID_VOUCHER" {
		t.Fatal("minimal belanja harus ditolak")
	}
	if errType(CheckVoucher(v, 50000, now.Add(2*time.Hour))) != "INVALID_VOUCHER" {
		t.Fatal("voucher kedaluwarsa harus ditolak")
	}
}

func TestDeliveryFee(t *testing.T) {
	flat := appconfig.OrderRules{DeliveryFee: 10000}
	if DeliveryFee(100000, nil, flat) != 10000 {
		t.Fatal("tanpa promo ongkir harus flat")
	}
	if DeliveryFee(50000, nil, appconfig.OrderRules{DeliveryFee: 10000, FreeDeliveryMinSubtotal: 50000}) != 0 {
		t.Fatal("subtotal ≥ batas harus gratis")
	}
	km := func(f float64) *float64 { return &f }
	r := appconfig.OrderRules{DeliveryFee: 10000, DeliveryBaseKM: 3, DeliveryFeePerKM: 2000, DeliveryFeeMax: 20000}
	for _, c := range []struct {
		km   *float64
		want int64
	}{
		{nil, 10000},      // alamat tanpa koordinat → tarif dasar
		{km(2.5), 10000},  // dalam jarak dasar
		{km(3.0), 10000},  // tepat batas
		{km(3.1), 12000},  // lewat sedikit → dibulatkan ke atas 1 km
		{km(5.0), 14000},  // 2 km tambahan
		{km(12.0), 20000}, // dibatasi fee_max
	} {
		if got := DeliveryFee(30000, c.km, r); got != c.want {
			t.Fatalf("jarak %v: ongkir %d, mau %d", c.km, got, c.want)
		}
	}
}

func TestDeliveryETA(t *testing.T) {
	r := appconfig.OrderRules{PrepMinutes: 15, CourierSpeedKMH: 20}
	for _, c := range []struct {
		km       float64
		min, max int
	}{
		{0, 15, 25},   // hanya waktu siapkan
		{2, 25, 35},   // 15 + 6 = 21 → 25
		{6.5, 35, 45}, // 15 + 20 = 35
	} {
		if got := DeliveryETA(c.km, r); got.MinMinutes != c.min || got.MaxMinutes != c.max {
			t.Fatalf("jarak %.1f: ETA %+v, mau %d–%d", c.km, got, c.min, c.max)
		}
	}
}

func TestTransitions(t *testing.T) {
	ok := [][2]string{{"PENDING", "PAID"}, {"PENDING", "PROCESSING"}, {"PAID", "PROCESSING"},
		{"PROCESSING", "DELIVERING"}, {"DELIVERING", "COMPLETED"}, {"PENDING", "CANCELLED"}, {"PENDING", "FAILED"}}
	for _, tr := range ok {
		if !CanTransition(tr[0], tr[1]) {
			t.Errorf("%s → %s harus boleh", tr[0], tr[1])
		}
	}
	bad := [][2]string{{"COMPLETED", "PENDING"}, {"PROCESSING", "CANCELLED"}, {"PAID", "COMPLETED"}}
	for _, tr := range bad {
		if CanTransition(tr[0], tr[1]) {
			t.Errorf("%s → %s tidak boleh", tr[0], tr[1])
		}
	}
}

func TestRupiah(t *testing.T) {
	for in, want := range map[int64]string{0: "Rp 0", 500: "Rp 500", 15000: "Rp 15.000", 1250000: "Rp 1.250.000"} {
		if got := Rupiah(in); got != want {
			t.Errorf("Rupiah(%d) = %q, want %q", in, got, want)
		}
	}
}

// Produk promo: harga berlaku (Price) yang dipakai; extra opsi tetap ditambahkan.
func TestBuildLines_DealPrice(t *testing.T) {
	products := map[string]catalog.Product{
		"P07": {ID: "P07", Name: "Nasi Goreng Spesial", Price: 19900, OriginalPrice: 32000, IsAvailable: true,
			Options: []catalog.Option{{ID: "normal", Name: "Normal"}, {ID: "jumbo", Name: "Porsi Jumbo", ExtraPrice: 8000}}},
	}
	lines, subtotal, err := BuildLines([]ItemInput{{ProductID: "P07", OptionID: "jumbo", Quantity: 2}}, products, DefaultRules)
	if err != nil {
		t.Fatal(err)
	}
	if lines[0].UnitPrice != 27900 || subtotal != 55800 {
		t.Fatalf("unit = %d, subtotal = %d; harap 27900 & 55800", lines[0].UnitPrice, subtotal)
	}
	if !products["P07"].OnDeal() || products["P07"].DiscountPercent() != 37 {
		t.Fatal("P07 harus terdeteksi promo 37%")
	}
}

func TestBuildLines_Stock(t *testing.T) {
	products := fixtureProducts()
	p := products["P01"]
	three := 3
	p.Stock = &three
	products["P01"] = p
	// Varian berbeda tetap memakai stok produk yang sama: 2 + 1 = 3 masih boleh.
	if _, _, err := BuildLines([]ItemInput{
		{ProductID: "P01", OptionID: "large", Quantity: 2},
		{ProductID: "P01", OptionID: "regular", Quantity: 1},
	}, products, DefaultRules); err != nil {
		t.Fatalf("3 dari stok 3 harus boleh: %v", err)
	}
	_, _, err := BuildLines([]ItemInput{
		{ProductID: "P01", OptionID: "large", Quantity: 2},
		{ProductID: "P01", OptionID: "regular", Quantity: 2},
	}, products, DefaultRules)
	if errType(err) != "OUT_OF_STOCK" {
		t.Fatalf("4 dari stok 3 harus OUT_OF_STOCK, dapat %v", err)
	}
}

func TestWeekStart(t *testing.T) {
	jkt, _ := time.LoadLocation("Asia/Jakarta")
	for _, c := range []struct{ now, want string }{
		{"2026-10-19T00:00:00+07:00", "2026-10-19"}, // Senin 00:00 → hari itu
		{"2026-10-21T15:30:00+07:00", "2026-10-19"}, // Rabu
		{"2026-10-25T23:59:00+07:00", "2026-10-19"}, // Minggu malam masih minggu yang sama
		{"2026-10-18T20:00:00Z", "2026-10-19"},      // Senin 03:00 WIB walau di UTC masih Minggu
	} {
		now, _ := time.Parse(time.RFC3339, c.now)
		if got := WeekStart(now, jkt).Format("2006-01-02"); got != c.want {
			t.Errorf("%s → %s, harap %s", c.now, got, c.want)
		}
	}
}

func TestRefundInfo(t *testing.T) {
	if refundInfo(StatusProcessing, 50000, 0) != nil {
		t.Fatal("pesanan berjalan tanpa refund → tidak ada info")
	}
	if r := refundInfo(StatusCancelled, 50000, 0); r == nil || r.Status != RefundPending {
		t.Fatalf("online batal tanpa refund → menunggu diproses: %v", r)
	}
	if r := refundInfo(StatusCompleted, 50000, 10000); r == nil || r.Status != RefundPartial {
		t.Fatalf("refund sebagian: %v", r)
	}
	if r := refundInfo(StatusCancelled, 50000, 50000); r == nil || r.Status != RefundCompleted {
		t.Fatalf("refund penuh: %v", r)
	}
}
