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

type googleRequest struct {
	IDToken  string `json:"id_token" validate:"required,max=8192"`
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
	// true = akun baru dibuat (register / Google pertama kali) → iOS bisa menampilkan sambutan.
	IsNewUser bool `json:"is_new_user"`
	// false = minta user memasukkan kode verifikasi yang dikirim ke email (dibutuhkan untuk memesan).
	EmailVerified bool `json:"email_verified"`
}

func toSessionDTO(s Session) SessionDTO {
	img := ""
	if s.User.AvatarURL != nil {
		img = *s.User.AvatarURL
	}
	return SessionDTO{
		UserName:      s.User.Name,
		UserImage:     img,
		AccessToken:   s.AccessToken,
		RefreshToken:  s.RefreshToken,
		ExpiresAt:     s.ExpiresAt,
		IsNewUser:     s.IsNewUser,
		EmailVerified: s.User.EmailVerified,
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

// POST /v1/auth/google — daftar / masuk dengan Google (body: id_token dari Google Sign-In).
// 201 bila akun baru dibuat, 200 bila akun sudah ada.
func (h *Handler) Google(w http.ResponseWriter, r *http.Request) {
	var req googleRequest
	if err := httpx.Decode(r, &req); err != nil {
		httpx.FromError(w, r, err)
		return
	}
	key := httpx.ClientIP(r) + "|google"
	if !h.loginLimiter.Allow(key) {
		httpx.Error(w, http.StatusTooManyRequests, "RATE_LIMITED", "Terlalu banyak percobaan masuk. Coba lagi dalam 1 menit.")
		return
	}
	sess, err := h.svc.Google(r.Context(), GoogleInput{IDToken: req.IDToken, FCMToken: req.FCMToken, UserAgent: r.UserAgent()})
	if err != nil {
		httpx.FromError(w, r, err)
		return
	}
	h.loginLimiter.Reset(key)
	status := http.StatusOK
	if sess.IsNewUser {
		status = http.StatusCreated
	}
	httpx.JSON(w, status, toSessionDTO(sess))
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
