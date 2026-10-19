package admin

import (
	"context"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5"

	"trytuist-api/internal/appconfig"
	"trytuist-api/internal/database"
	"trytuist-api/internal/httpx"
	"trytuist-api/internal/order"
)

// Web admin → Ongkir (super admin): aturan ongkir per jarak & estimasi waktu antar di
// app_config.order_rules. Contoh perhitungan memakai fungsi yang sama dengan quote/checkout.

type DeliverySettingsDTO struct {
	DeliveryFee             int64   `json:"delivery_fee"`
	DeliveryBaseKM          float64 `json:"delivery_base_km"`
	DeliveryFeePerKM        int64   `json:"delivery_fee_per_km"`
	DeliveryFeeMax          int64   `json:"delivery_fee_max"`
	FreeDeliveryMinSubtotal int64   `json:"free_delivery_min_subtotal"`
	PrepMinutes             int     `json:"prep_minutes"`
	CourierSpeedKMH         float64 `json:"courier_speed_kmh"`
	// Kupon gratis ongkir mingguan per user (diaktifkan user saat checkout).
	FreeDeliveryCouponEnabled bool `json:"free_delivery_coupon_enabled"`
	FreeDeliveryCouponWeekly  int  `json:"free_delivery_coupon_weekly"`
}

type DeliveryExampleDTO struct {
	DistanceKM    float64 `json:"distance_km"`
	DeliveryFee   int64   `json:"delivery_fee"`
	ETAMinMinutes int     `json:"eta_min_minutes"`
	ETAMaxMinutes int     `json:"eta_max_minutes"`
}

type DeliverySettingsResponse struct {
	Settings  DeliverySettingsDTO  `json:"settings"`
	Examples  []DeliveryExampleDTO `json:"examples"`
	UpdatedAt *string              `json:"updated_at"`
}

// Jarak contoh di tabel perhitungan web admin.
var exampleDistances = []float64{0.5, 1, 2, 3, 4, 5, 7, 10, 15}

func toDeliverySettings(r appconfig.OrderRules) DeliverySettingsDTO {
	return DeliverySettingsDTO{
		DeliveryFee: r.DeliveryFee, DeliveryBaseKM: r.DeliveryBaseKM, DeliveryFeePerKM: r.DeliveryFeePerKM,
		DeliveryFeeMax: r.DeliveryFeeMax, FreeDeliveryMinSubtotal: r.FreeDeliveryMinSubtotal,
		PrepMinutes: r.PrepMinutes, CourierSpeedKMH: r.CourierSpeedKMH,
		FreeDeliveryCouponEnabled: r.FreeDeliveryCouponEnabled, FreeDeliveryCouponWeekly: r.FreeDeliveryCouponWeekly,
	}
}

// deliveryExamples: subtotal 0 = tanpa gratis ongkir (yang terlihat murni tarif jarak).
func deliveryExamples(r appconfig.OrderRules, distances []float64, subtotal int64) []DeliveryExampleDTO {
	out := make([]DeliveryExampleDTO, len(distances))
	for i, d := range distances {
		eta := order.DeliveryETA(d, r)
		out[i] = DeliveryExampleDTO{DistanceKM: d, DeliveryFee: order.DeliveryFee(subtotal, &d, r),
			ETAMinMinutes: eta.MinMinutes, ETAMaxMinutes: eta.MaxMinutes}
	}
	return out
}

func (h *Handler) deliveryResponse(ctx context.Context, r appconfig.OrderRules) (DeliverySettingsResponse, error) {
	at, err := h.appConfig.OrderRulesUpdatedAt(ctx)
	if err != nil {
		return DeliverySettingsResponse{}, err
	}
	return DeliverySettingsResponse{Settings: toDeliverySettings(r), Examples: deliveryExamples(r, exampleDistances, 0),
		UpdatedAt: httpx.ISOPtr(at)}, nil
}

// GET /v1/admin/delivery-settings
func (h *Handler) DeliverySettings(w http.ResponseWriter, r *http.Request) {
	rules, err := h.appConfig.OrderRules(r.Context(), nil)
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	res, err := h.deliveryResponse(r.Context(), rules)
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, res)
}

// deliveryRequest: field yang tidak dikirim tidak berubah.
type deliveryRequest struct {
	DeliveryFee               *int64   `json:"delivery_fee" validate:"omitempty,min=0,max=1000000"`
	DeliveryBaseKM            *float64 `json:"delivery_base_km" validate:"omitempty,min=0,max=50"`
	DeliveryFeePerKM          *int64   `json:"delivery_fee_per_km" validate:"omitempty,min=0,max=100000"`
	DeliveryFeeMax            *int64   `json:"delivery_fee_max" validate:"omitempty,min=0,max=1000000"`
	FreeDeliveryMinSubtotal   *int64   `json:"free_delivery_min_subtotal" validate:"omitempty,min=0,max=100000000"`
	PrepMinutes               *int     `json:"prep_minutes" validate:"omitempty,min=0,max=240"`
	CourierSpeedKMH           *float64 `json:"courier_speed_kmh" validate:"omitempty,gt=0,max=120"`
	FreeDeliveryCouponEnabled *bool    `json:"free_delivery_coupon_enabled"`
	FreeDeliveryCouponWeekly  *int     `json:"free_delivery_coupon_weekly" validate:"omitempty,min=1,max=14"`
	// Hanya untuk pratinjau: subtotal keranjang contoh (cek gratis ongkir) & jarak tambahan.
	PreviewSubtotal  int64     `json:"preview_subtotal" validate:"min=0"`
	PreviewDistances []float64 `json:"preview_distances" validate:"max=20,dive,min=0,max=100"`
}

func (req deliveryRequest) patch() map[string]any {
	p := map[string]any{}
	if req.DeliveryFee != nil {
		p["delivery_fee"] = *req.DeliveryFee
	}
	if req.DeliveryBaseKM != nil {
		p["delivery_base_km"] = *req.DeliveryBaseKM
	}
	if req.DeliveryFeePerKM != nil {
		p["delivery_fee_per_km"] = *req.DeliveryFeePerKM
	}
	if req.DeliveryFeeMax != nil {
		p["delivery_fee_max"] = *req.DeliveryFeeMax
	}
	if req.FreeDeliveryMinSubtotal != nil {
		p["free_delivery_min_subtotal"] = *req.FreeDeliveryMinSubtotal
	}
	if req.PrepMinutes != nil {
		p["prep_minutes"] = *req.PrepMinutes
	}
	if req.CourierSpeedKMH != nil {
		p["courier_speed_kmh"] = *req.CourierSpeedKMH
	}
	if req.FreeDeliveryCouponEnabled != nil {
		p["free_delivery_coupon_enabled"] = *req.FreeDeliveryCouponEnabled
	}
	if req.FreeDeliveryCouponWeekly != nil {
		p["free_delivery_coupon_weekly"] = *req.FreeDeliveryCouponWeekly
	}
	return p
}

// apply: aturan sekarang + perubahan dari request (belum disimpan).
func (req deliveryRequest) apply(r appconfig.OrderRules) (appconfig.OrderRules, error) {
	if req.DeliveryFee != nil {
		r.DeliveryFee = *req.DeliveryFee
	}
	if req.DeliveryBaseKM != nil {
		r.DeliveryBaseKM = *req.DeliveryBaseKM
	}
	if req.DeliveryFeePerKM != nil {
		r.DeliveryFeePerKM = *req.DeliveryFeePerKM
	}
	if req.DeliveryFeeMax != nil {
		r.DeliveryFeeMax = *req.DeliveryFeeMax
	}
	if req.FreeDeliveryMinSubtotal != nil {
		r.FreeDeliveryMinSubtotal = *req.FreeDeliveryMinSubtotal
	}
	if req.PrepMinutes != nil {
		r.PrepMinutes = *req.PrepMinutes
	}
	if req.CourierSpeedKMH != nil {
		r.CourierSpeedKMH = *req.CourierSpeedKMH
	}
	if req.FreeDeliveryCouponEnabled != nil {
		r.FreeDeliveryCouponEnabled = *req.FreeDeliveryCouponEnabled
	}
	if req.FreeDeliveryCouponWeekly != nil {
		r.FreeDeliveryCouponWeekly = *req.FreeDeliveryCouponWeekly
	}
	if r.DeliveryFeeMax > 0 && r.DeliveryFeeMax < r.DeliveryFee {
		return r, httpx.Validation("Ongkir maksimal tidak boleh lebih kecil dari tarif dasar.",
			httpx.FieldDetail{Field: "delivery_fee_max", Message: "harus ≥ tarif dasar, atau 0 untuk tanpa batas"})
	}
	return r, nil
}

// POST /v1/admin/delivery-settings/preview — hitung contoh dari aturan draf tanpa menyimpan.
func (h *Handler) PreviewDeliverySettings(w http.ResponseWriter, r *http.Request) {
	var req deliveryRequest
	if err := httpx.Decode(r, &req); err != nil {
		httpx.FromError(w, r, err)
		return
	}
	current, err := h.appConfig.OrderRules(r.Context(), nil)
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	draft, err := req.apply(current)
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	distances := exampleDistances
	if len(req.PreviewDistances) > 0 {
		distances = req.PreviewDistances
	}
	httpx.JSON(w, http.StatusOK, DeliverySettingsResponse{Settings: toDeliverySettings(draft),
		Examples: deliveryExamples(draft, distances, req.PreviewSubtotal)})
}

// PATCH /v1/admin/delivery-settings — langsung berlaku untuk quote & checkout berikutnya.
func (h *Handler) UpdateDeliverySettings(w http.ResponseWriter, r *http.Request) {
	var req deliveryRequest
	if err := httpx.Decode(r, &req); err != nil {
		httpx.FromError(w, r, err)
		return
	}
	ctx := r.Context()
	var before, after appconfig.OrderRules
	err := database.InTx(ctx, h.pool, func(tx pgx.Tx) error {
		// Kunci baris supaya dua admin yang menyimpan bersamaan tidak saling menimpa diam-diam.
		if _, err := tx.Exec(ctx, `SELECT 1 FROM app_config WHERE key = 'order_rules' FOR UPDATE`); err != nil {
			return err
		}
		var err error
		if before, err = h.appConfig.OrderRules(ctx, tx); err != nil {
			return err
		}
		if _, err = req.apply(before); err != nil {
			return err
		}
		after, err = h.appConfig.MergeOrderRules(ctx, tx, req.patch())
		return err
	})
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	h.audit(r, "DELIVERY_SETTINGS", "app_config", "order_rules",
		"Ubah aturan ongkir: "+order.Rupiah(after.DeliveryFee)+" s.d. "+trimKM(after.DeliveryBaseKM)+
			" km, +"+order.Rupiah(after.DeliveryFeePerKM)+"/km",
		map[string]any{"before": toDeliverySettings(before), "after": toDeliverySettings(after)})
	res, err := h.deliveryResponse(ctx, after)
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	httpx.JSONMessage(w, http.StatusOK, "Aturan ongkir disimpan", res)
}

func trimKM(km float64) string { return strconv.FormatFloat(km, 'f', -1, 64) }
