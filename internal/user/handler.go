package user

import (
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"trytuist-api/internal/catalog"
	"trytuist-api/internal/httpx"
)

// ===== DTO =====

type ProfileDTO struct {
	ID           string  `json:"id"`
	Name         string  `json:"name"`
	Email        string  `json:"email"`
	Phone        *string `json:"phone"`
	AvatarURL    *string `json:"avatar_url"`
	MemberTier   string  `json:"member_tier"`
	Points       int     `json:"points"`
	VoucherCount int     `json:"voucher_count"`
	Balance      int64   `json:"balance"`
	// false → tampilkan "Verifikasi email" (kode di POST /v1/me/email/verify).
	EmailVerified bool `json:"email_verified"`
	// false = akun Google tanpa password; ganti password tidak butuh password lama.
	HasPassword bool `json:"has_password"`
}

type updateMeRequest struct {
	Name      *string `json:"name" validate:"omitempty,max=100"`
	Phone     *string `json:"phone" validate:"omitempty,max=20"`
	AvatarURL *string `json:"avatar_url" validate:"omitempty,max=500"`
}

type AddressDTO struct {
	ID        string   `json:"id"`
	Label     string   `json:"label"`
	Recipient string   `json:"recipient"`
	Phone     string   `json:"phone"`
	Address   string   `json:"address"`
	Notes     *string  `json:"notes"`
	Lat       *float64 `json:"lat"`
	Lng       *float64 `json:"lng"`
	IsDefault bool     `json:"is_default"`
}

type createAddressRequest struct {
	Label     string   `json:"label" validate:"required,max=50"`
	Recipient string   `json:"recipient" validate:"required,max=100"`
	Phone     string   `json:"phone" validate:"required,max=20"`
	Address   string   `json:"address" validate:"required,max=500"`
	Notes     *string  `json:"notes" validate:"omitempty,max=500"`
	Lat       *float64 `json:"lat" validate:"omitempty,min=-90,max=90"`
	Lng       *float64 `json:"lng" validate:"omitempty,min=-180,max=180"`
	IsDefault bool     `json:"is_default"`
}

type updateAddressRequest struct {
	Label     *string  `json:"label" validate:"omitempty,max=50"`
	Recipient *string  `json:"recipient" validate:"omitempty,max=100"`
	Phone     *string  `json:"phone" validate:"omitempty,max=20"`
	Address   *string  `json:"address" validate:"omitempty,max=500"`
	Notes     *string  `json:"notes" validate:"omitempty,max=500"`
	Lat       *float64 `json:"lat" validate:"omitempty,min=-90,max=90"`
	Lng       *float64 `json:"lng" validate:"omitempty,min=-180,max=180"`
	IsDefault *bool    `json:"is_default"`
}

type VoucherDTO struct {
	Code          string `json:"code"`
	Title         string `json:"title"`
	Description   string `json:"description"`
	DiscountType  string `json:"discount_type"`
	DiscountValue int64  `json:"discount_value"`
	MinSubtotal   int64  `json:"min_subtotal"`
	MaxDiscount   *int64 `json:"max_discount"`
	EndsAt        string `json:"ends_at"`
}

func toProfileDTO(p Profile) ProfileDTO {
	return ProfileDTO{ID: p.ID, Name: p.Name, Email: p.Email, Phone: p.Phone, AvatarURL: p.AvatarURL,
		MemberTier: p.MemberTier, Points: p.Points, VoucherCount: p.VoucherCount, Balance: p.Balance,
		EmailVerified: p.EmailVerified, HasPassword: p.HasPassword}
}

func toAddressDTO(a Address) AddressDTO {
	return AddressDTO{ID: a.ID, Label: a.Label, Recipient: a.Recipient, Phone: a.Phone, Address: a.Address,
		Notes: a.Notes, Lat: a.Lat, Lng: a.Lng, IsDefault: a.IsDefault}
}

// ===== Handler =====

type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// GET /v1/me 🔒
func (h *Handler) Me(w http.ResponseWriter, r *http.Request) {
	p, err := h.svc.Me(r.Context(), httpx.UserID(r.Context()))
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, toProfileDTO(p))
}

// PATCH /v1/me 🔒 — name, phone, avatar_url (sebagian saja boleh).
func (h *Handler) UpdateMe(w http.ResponseWriter, r *http.Request) {
	var req updateMeRequest
	if err := httpx.Decode(r, &req); err != nil {
		httpx.FromError(w, r, err)
		return
	}
	p, err := h.svc.UpdateMe(r.Context(), httpx.UserID(r.Context()), ProfilePatch{
		Name: req.Name, Phone: trimPtr(req.Phone), AvatarURL: trimPtr(req.AvatarURL),
	})
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, toProfileDTO(p))
}

// GET /v1/me/addresses 🔒
func (h *Handler) Addresses(w http.ResponseWriter, r *http.Request) {
	list, err := h.svc.Addresses(r.Context(), httpx.UserID(r.Context()))
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	out := make([]AddressDTO, len(list))
	for i, a := range list {
		out[i] = toAddressDTO(a)
	}
	httpx.JSON(w, http.StatusOK, out)
}

// POST /v1/me/addresses 🔒
func (h *Handler) CreateAddress(w http.ResponseWriter, r *http.Request) {
	var req createAddressRequest
	if err := httpx.Decode(r, &req); err != nil {
		httpx.FromError(w, r, err)
		return
	}
	a := Address{
		Label: strings.TrimSpace(req.Label), Recipient: strings.TrimSpace(req.Recipient),
		Phone: strings.TrimSpace(req.Phone), Address: strings.TrimSpace(req.Address),
		Notes: trimPtrNil(req.Notes), Lat: req.Lat, Lng: req.Lng, IsDefault: req.IsDefault,
	}
	var details []httpx.FieldDetail
	for _, f := range [][2]string{{"label", a.Label}, {"recipient", a.Recipient}, {"phone", a.Phone}, {"address", a.Address}} {
		if f[1] == "" {
			details = append(details, httpx.FieldDetail{Field: f[0], Message: "wajib diisi"})
		}
	}
	if len(details) > 0 {
		httpx.FromError(w, r, httpx.Validation("Data yang dikirim tidak valid.", details...))
		return
	}
	out, err := h.svc.CreateAddress(r.Context(), httpx.UserID(r.Context()), a)
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, toAddressDTO(out))
}

// PATCH /v1/me/addresses/{id} 🔒
func (h *Handler) UpdateAddress(w http.ResponseWriter, r *http.Request) {
	var req updateAddressRequest
	if err := httpx.Decode(r, &req); err != nil {
		httpx.FromError(w, r, err)
		return
	}
	out, err := h.svc.UpdateAddress(r.Context(), httpx.UserID(r.Context()), chi.URLParam(r, "id"), AddressPatch{
		Label: req.Label, Recipient: req.Recipient, Phone: req.Phone, Address: req.Address, Notes: req.Notes,
		Lat: req.Lat, Lng: req.Lng, IsDefault: req.IsDefault,
	})
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, toAddressDTO(out))
}

// DELETE /v1/me/addresses/{id} 🔒
func (h *Handler) DeleteAddress(w http.ResponseWriter, r *http.Request) {
	if err := h.svc.DeleteAddress(r.Context(), httpx.UserID(r.Context()), chi.URLParam(r, "id")); err != nil {
		httpx.FromError(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, nil)
}

// GET /v1/me/favorites 🔒
func (h *Handler) Favorites(w http.ResponseWriter, r *http.Request) {
	list, err := h.svc.Favorites(r.Context(), httpx.UserID(r.Context()), r.URL.Query().Get("store_id"))
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, catalog.ToProductDTOs(list))
}

// PUT /v1/me/favorites/{product_id} 🔒 (idempotent)
func (h *Handler) AddFavorite(w http.ResponseWriter, r *http.Request) {
	if err := h.svc.AddFavorite(r.Context(), httpx.UserID(r.Context()), chi.URLParam(r, "product_id")); err != nil {
		httpx.FromError(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, nil)
}

// DELETE /v1/me/favorites/{product_id} 🔒 (idempotent)
func (h *Handler) RemoveFavorite(w http.ResponseWriter, r *http.Request) {
	if err := h.svc.RemoveFavorite(r.Context(), httpx.UserID(r.Context()), chi.URLParam(r, "product_id")); err != nil {
		httpx.FromError(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, nil)
}

// GET /v1/me/vouchers 🔒
func (h *Handler) Vouchers(w http.ResponseWriter, r *http.Request) {
	list, err := h.svc.Vouchers(r.Context(), httpx.UserID(r.Context()))
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	out := make([]VoucherDTO, len(list))
	for i, v := range list {
		out[i] = VoucherDTO{Code: v.Code, Title: v.Title, Description: v.Description, DiscountType: v.DiscountType,
			DiscountValue: v.DiscountValue, MinSubtotal: v.MinSubtotal, MaxDiscount: v.MaxDiscount, EndsAt: httpx.ISO(v.EndsAt)}
	}
	httpx.JSON(w, http.StatusOK, out)
}

func trimPtr(s *string) *string {
	if s == nil {
		return nil
	}
	t := strings.TrimSpace(*s)
	return &t
}

func trimPtrNil(s *string) *string {
	if s == nil {
		return nil
	}
	t := strings.TrimSpace(*s)
	if t == "" {
		return nil
	}
	return &t
}
