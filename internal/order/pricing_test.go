package order

import (
	"errors"
	"testing"
	"time"

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
	if DeliveryFee(100000, 10000, 0) != 10000 {
		t.Fatal("tanpa promo ongkir harus flat")
	}
	if DeliveryFee(50000, 10000, 50000) != 0 {
		t.Fatal("subtotal ≥ batas harus gratis")
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
