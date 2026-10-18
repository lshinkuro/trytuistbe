package auth

import (
	"net/http"
	"strings"

	"trytuist-api/internal/httpx"
)

// ===== DTO =====

type registerRequest struct {
	Email    string `json:"email" validate:"required,email,max=254"`
	Password string `json:"password" validate:"required,min=8,max=72"`
	Name     string `json:"name" validate:"max=100"`
	FCMToken string `json:"fcm_token" validate:"max=4096"`
}

type loginRequest struct {
	Email    string `json:"email" validate:"required,email,max=254"`
	Password string `json:"password" validate:"required,max=72"`
	FCMToken string `json:"fcm_token" validate:"max=4096"`
}

type refreshRequest struct {
	RefreshToken string `json:"refresh_token" validate:"required,max=200"`
}

type logoutRequest struct {
	RefreshToken string `json:"refresh_token" validate:"max=200"`
}

// SessionDTO = SessionDTO di iOS.
type SessionDTO struct {
	UserName     string `json:"user_name"`
	UserImage    string `json:"user_image"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresAt    int64  `json:"expires_at"`
}

func toSessionDTO(s Session) SessionDTO {
	img := ""
	if s.User.AvatarURL != nil {
		img = *s.User.AvatarURL
	}
	return SessionDTO{
		UserName:     s.User.Name,
		UserImage:    img,
		AccessToken:  s.AccessToken,
		RefreshToken: s.RefreshToken,
		ExpiresAt:    s.ExpiresAt,
	}
}

// ===== Handler =====

type Handler struct {
	svc          *Service
	loginLimiter *httpx.RateLimiter
}

func NewHandler(svc *Service, loginLimiter *httpx.RateLimiter) *Handler {
	return &Handler{svc: svc, loginLimiter: loginLimiter}
}

// POST /v1/auth/register
func (h *Handler) Register(w http.ResponseWriter, r *http.Request) {
	var req registerRequest
	if err := httpx.Decode(r, &req); err != nil {
		httpx.FromError(w, r, err)
		return
	}
	sess, err := h.svc.Register(r.Context(), RegisterInput{
		Email: req.Email, Password: req.Password, Name: req.Name, FCMToken: req.FCMToken, UserAgent: r.UserAgent(),
	})
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, toSessionDTO(sess))
}

// POST /v1/auth/login — rate limit per IP + email.
func (h *Handler) Login(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := httpx.Decode(r, &req); err != nil {
		httpx.FromError(w, r, err)
		return
	}
	key := httpx.ClientIP(r) + "|" + strings.ToLower(strings.TrimSpace(req.Email))
	if !h.loginLimiter.Allow(key) {
		httpx.Error(w, http.StatusTooManyRequests, "RATE_LIMITED", "Terlalu banyak percobaan masuk. Coba lagi dalam 1 menit.")
		return
	}
	sess, err := h.svc.Login(r.Context(), LoginInput{
		Email: req.Email, Password: req.Password, FCMToken: req.FCMToken, UserAgent: r.UserAgent(),
	})
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	h.loginLimiter.Reset(key)
	httpx.JSON(w, http.StatusOK, toSessionDTO(sess))
}

// POST /v1/auth/refresh
func (h *Handler) Refresh(w http.ResponseWriter, r *http.Request) {
	var req refreshRequest
	if err := httpx.Decode(r, &req); err != nil {
		httpx.FromError(w, r, err)
		return
	}
	sess, err := h.svc.Refresh(r.Context(), req.RefreshToken, r.UserAgent())
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, toSessionDTO(sess))
}

// POST /v1/auth/logout 🔒 — body boleh kosong.
func (h *Handler) Logout(w http.ResponseWriter, r *http.Request) {
	var req logoutRequest
	if r.ContentLength != 0 {
		if err := httpx.Decode(r, &req); err != nil {
			httpx.FromError(w, r, err)
			return
		}
	}
	if err := h.svc.Logout(r.Context(), httpx.UserID(r.Context()), req.RefreshToken); err != nil {
		httpx.FromError(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, nil)
}
