package admin

import (
	"net/url"
	"time"

	"trytuist-api/internal/httpx"
)

// MaxRangeDays: batas rentang tanggal dashboard / filter pesanan.
const MaxRangeDays = 366

// DateRange: [From, To) di zona waktu toko. Day* = tanggal yang dipilih (inklusif).
type DateRange struct {
	From, To       time.Time
	DayFrom, DayTo time.Time
}

func (d DateRange) Days() int { return int(d.To.Sub(d.From).Hours()/24 + 0.5) }

// Previous: periode sebelumnya dengan panjang sama (untuk persentase naik/turun).
func (d DateRange) Previous() DateRange {
	n := d.Days()
	return DateRange{From: d.From.AddDate(0, 0, -n), To: d.From,
		DayFrom: d.DayFrom.AddDate(0, 0, -n), DayTo: d.DayFrom.AddDate(0, 0, -1)}
}

func dayStart(t time.Time, loc *time.Location) time.Time {
	y, m, d := t.In(loc).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, loc)
}

// ParseDateRange membaca ?from=YYYY-MM-DD&to=YYYY-MM-DD. Default: defDays hari terakhir
// sampai hari ini. defDays = 0 → tanpa default (ok=false bila keduanya kosong).
func ParseDateRange(q url.Values, loc *time.Location, now time.Time, defDays int) (DateRange, bool, error) {
	fromS, toS := q.Get("from"), q.Get("to")
	if fromS == "" && toS == "" && defDays == 0 {
		return DateRange{}, false, nil
	}
	var details []httpx.FieldDetail
	parse := func(field, v string) time.Time {
		t, err := time.ParseInLocation("2006-01-02", v, loc)
		if err != nil {
			details = append(details, httpx.FieldDetail{Field: field, Message: "format tanggal harus YYYY-MM-DD"})
		}
		return t
	}
	to := dayStart(now, loc)
	if toS != "" {
		to = parse("to", toS)
	}
	from := to.AddDate(0, 0, -(max(defDays, 1) - 1))
	if fromS != "" {
		from = parse("from", fromS)
	} else if defDays == 0 {
		from = to
	}
	if len(details) == 0 {
		switch {
		case from.After(to):
			details = append(details, httpx.FieldDetail{Field: "from", Message: "tidak boleh setelah tanggal akhir"})
		case to.Sub(from).Hours()/24 >= MaxRangeDays:
			details = append(details, httpx.FieldDetail{Field: "from", Message: "rentang maksimal 366 hari"})
		}
	}
	if len(details) > 0 {
		return DateRange{}, false, httpx.Validation("Rentang tanggal tidak valid.", details...)
	}
	return DateRange{From: from, To: to.AddDate(0, 0, 1), DayFrom: from, DayTo: to}, true, nil
}
