package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

const (
	openMin  = 10 * 60 // 10:00
	closeMin = 21 * 60 // 21:00
	stepMin  = 30
)

var (
	db      *sql.DB
	mu      sync.Mutex // serialises booking writes (plus a single DB connection)
	phoneRe = regexp.MustCompile(`^\+?[0-9]{10,15}$`)
)

func main() {
	var err error
	db, err = sql.Open("sqlite", "dady.db?_pragma=busy_timeout(5000)")
	if err != nil {
		log.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if err = initDB(); err != nil {
		log.Fatal(err)
	}
	startBot()
	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.Dir("static")))
	mux.HandleFunc("GET /api/services", listServices)
	mux.HandleFunc("GET /api/barbers", listBarbers)
	mux.HandleFunc("GET /api/slots", slotsH)
	mux.HandleFunc("POST /api/book", bookH)
	mux.HandleFunc("GET /api/my", myH)
	mux.HandleFunc("POST /api/cancel", cancelH)
	mux.HandleFunc("POST /api/move", moveH)
	mux.HandleFunc("GET /api/admin/bookings", admin(adminList))
	mux.HandleFunc("POST /api/admin/status", admin(adminStatus))
	addr := ":" + env("PORT", "8080")
	log.Println("DADY on", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func initDB() error {
	_, err := db.Exec(`
CREATE TABLE IF NOT EXISTS services(id INTEGER PRIMARY KEY, name TEXT, price INT, dur INT);
CREATE TABLE IF NOT EXISTS barbers(id INTEGER PRIMARY KEY, name TEXT, spec TEXT, years INT);
CREATE TABLE IF NOT EXISTS bookings(id INTEGER PRIMARY KEY, service INT, barber INT, date TEXT, start INT, dur INT,
  name TEXT, phone TEXT, status TEXT DEFAULT 'new', created TEXT DEFAULT CURRENT_TIMESTAMP);
CREATE INDEX IF NOT EXISTS ix_b ON bookings(barber, date);`)
	if err != nil {
		return err
	}
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM services`).Scan(&n)
	if n == 0 { // demo data
		_, err = db.Exec(`
INSERT INTO services(name,price,dur) VALUES('Мужская стрижка',2000,60),('Стрижка машинкой',1200,30),
 ('Оформление бороды',1500,30),('Стрижка + борода',3000,90),('Детская стрижка',1300,30),('Комплексный уход',4500,120);
INSERT INTO barbers(name,spec,years) VALUES('Артём','Классика, fade',8),('Марк','Борода, бритьё',5),('Иван','Креативные стрижки',3);`)
	}
	return err
}

// ---------- helpers ----------
func js(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}
func fail(w http.ResponseWriter, code int, msg string) { js(w, code, map[string]string{"error": msg}) }
func hm(m int) string                                  { return time.Date(0, 1, 1, m/60, m%60, 0, 0, time.UTC).Format("15:04") }
func cleanPhone(p string) string {
	return strings.NewReplacer(" ", "", "-", "", "(", "", ")", "").Replace(p)
}
func parseHM(s string) (int, error) {
	t, err := time.Parse("15:04", s)
	return t.Hour()*60 + t.Minute(), err
}

func rows(w http.ResponseWriter, q string, args ...any) {
	r, err := db.Query(q, args...)
	if err != nil {
		fail(w, 500, "ошибка базы данных")
		return
	}
	defer r.Close()
	cols, _ := r.Columns()
	out := []map[string]any{}
	for r.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		r.Scan(ptrs...)
		m := map[string]any{}
		for i, c := range cols {
			if b, ok := vals[i].([]byte); ok {
				vals[i] = string(b)
			}
			m[c] = vals[i]
		}
		out = append(out, m)
	}
	js(w, 200, out)
}

func listServices(w http.ResponseWriter, _ *http.Request) { rows(w, `SELECT * FROM services`) }
func listBarbers(w http.ResponseWriter, _ *http.Request)  { rows(w, `SELECT * FROM barbers`) }

// ---------- availability ----------
func isFuture(date string, start int) bool {
	d, err := time.ParseInLocation("2006-01-02", date, time.Local)
	if err != nil {
		return false
	}
	return d.Add(time.Duration(start) * time.Minute).After(time.Now())
}

// free reports whether barber is free in [start,start+dur), ignoring booking `skip`.
func free(barber int, date string, start, dur, skip int) bool {
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM bookings WHERE barber=? AND date=? AND status!='cancelled' AND id!=? AND start<? AND start+dur>?`,
		barber, date, skip, start+dur, start).Scan(&n)
	return n == 0
}

// pick returns a free barber id (0 if none). barber>0 means a specific master.
func pick(barber int, date string, start, dur, skip int) int {
	if start < openMin || start+dur > closeMin || !isFuture(date, start) {
		return 0
	}
	if barber > 0 {
		if free(barber, date, start, dur, skip) {
			return barber
		}
		return 0
	}
	r, _ := db.Query(`SELECT id FROM barbers ORDER BY id`)
	var ids []int
	for r.Next() {
		var id int
		r.Scan(&id)
		ids = append(ids, id)
	}
	r.Close()
	for _, id := range ids {
		if free(id, date, start, dur, skip) {
			return id
		}
	}
	return 0
}

func svcDur(id int) (int, bool) {
	var d int
	return d, db.QueryRow(`SELECT dur FROM services WHERE id=?`, id).Scan(&d) == nil
}

func slotsH(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	sid, _ := strconv.Atoi(q.Get("service"))
	bid, _ := strconv.Atoi(q.Get("barber"))
	dur, ok := svcDur(sid)
	if !ok {
		fail(w, 400, "выберите услугу")
		return
	}
	out := []string{}
	for t := openMin; t+dur <= closeMin; t += stepMin {
		if pick(bid, q.Get("date"), t, dur, 0) > 0 {
			out = append(out, hm(t))
		}
	}
	js(w, 200, out)
}

// ---------- booking ----------
type req struct {
	ID, Service, Barber int
	Date, Time, Name, Phone string
}

func bookH(w http.ResponseWriter, r *http.Request) {
	var q req
	if json.NewDecoder(r.Body).Decode(&q) != nil {
		fail(w, 400, "неверный запрос")
		return
	}
	q.Phone = cleanPhone(q.Phone)
	if strings.TrimSpace(q.Name) == "" || !phoneRe.MatchString(q.Phone) {
		fail(w, 400, "укажите имя и корректный телефон (10–15 цифр)")
		return
	}
	dur, ok := svcDur(q.Service)
	start, err := parseHM(q.Time)
	if !ok || err != nil {
		fail(w, 400, "выберите услугу и время")
		return
	}
	mu.Lock()
	defer mu.Unlock()
	b := pick(q.Barber, q.Date, start, dur, 0) // checked under lock => no double booking
	if b == 0 {
		fail(w, 409, "это время уже занято или недоступно — выберите другое")
		return
	}
	res, err := db.Exec(`INSERT INTO bookings(service,barber,date,start,dur,name,phone) VALUES(?,?,?,?,?,?,?)`,
		q.Service, b, q.Date, start, dur, strings.TrimSpace(q.Name), q.Phone)
	if err != nil {
		fail(w, 500, "не удалось сохранить запись")
		return
	}
	id, _ := res.LastInsertId()
	notify("Новая запись:\n" + brief(id))
	rows(w, bookingSQL+` WHERE b.id=?`, id)
}

const bookingSQL = `SELECT b.id,b.date,printf('%02d:%02d',b.start/60,b.start%60) AS time,b.status,b.name,b.phone,
 s.name AS service,s.price,br.name AS barber FROM bookings b JOIN services s ON s.id=b.service JOIN barbers br ON br.id=b.barber`

func myH(w http.ResponseWriter, r *http.Request) {
	p := cleanPhone(r.URL.Query().Get("phone"))
	if !phoneRe.MatchString(p) {
		fail(w, 400, "введите корректный телефон")
		return
	}
	rows(w, bookingSQL+` WHERE b.phone=? ORDER BY b.date DESC,b.start DESC`, p)
}

// own loads a booking that belongs to phone and can still be changed.
func own(q req) (date string, start, svc, barber int, err error) {
	e := db.QueryRow(`SELECT date,start,service,barber FROM bookings WHERE id=? AND phone=? AND status IN('new','confirmed')`,
		q.ID, cleanPhone(q.Phone)).Scan(&date, &start, &svc, &barber)
	if e != nil {
		err = errors.New("запись не найдена или уже закрыта")
	} else if d, _ := time.ParseInLocation("2006-01-02", date, time.Local); time.Until(d.Add(time.Duration(start)*time.Minute)) < 2*time.Hour {
		err = errors.New("отмена и перенос возможны не позднее чем за 2 часа до визита")
	}
	return
}

func cancelH(w http.ResponseWriter, r *http.Request) {
	var q req
	json.NewDecoder(r.Body).Decode(&q)
	mu.Lock()
	defer mu.Unlock()
	if _, _, _, _, err := own(q); err != nil {
		fail(w, 400, err.Error())
		return
	}
	db.Exec(`UPDATE bookings SET status='cancelled' WHERE id=?`, q.ID)
	notify("Запись отменена:\n" + brief(int64(q.ID)))
	js(w, 200, map[string]string{"ok": "запись отменена"})
}

func moveH(w http.ResponseWriter, r *http.Request) {
	var q req
	json.NewDecoder(r.Body).Decode(&q)
	mu.Lock()
	defer mu.Unlock()
	_, _, svc, barber, err := own(q)
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	dur, _ := svcDur(svc)
	start, e := parseHM(q.Time)
	if e != nil || pick(barber, q.Date, start, dur, q.ID) == 0 {
		fail(w, 409, "это время недоступно — выберите другое")
		return
	}
	db.Exec(`UPDATE bookings SET date=?,start=? WHERE id=?`, q.Date, start, q.ID)
	notify("Запись перенесена:\n" + brief(int64(q.ID)))
	js(w, 200, map[string]string{"ok": "запись перенесена"})
}

// ---------- admin ----------
func admin(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		pw := os.Getenv("ADMIN_PASSWORD")
		if pw == "" || r.Header.Get("X-Admin") != pw {
			fail(w, 401, "нет доступа")
			return
		}
		h(w, r)
	}
}

func adminList(w http.ResponseWriter, r *http.Request) {
	d := r.URL.Query().Get("date")
	if d == "" {
		rows(w, bookingSQL+` ORDER BY b.date DESC,b.start LIMIT 300`)
		return
	}
	rows(w, bookingSQL+` WHERE b.date=? ORDER BY b.start`, d)
}

func adminStatus(w http.ResponseWriter, r *http.Request) {
	var q struct {
		ID     int
		Status string
	}
	json.NewDecoder(r.Body).Decode(&q)
	switch q.Status {
	case "new", "confirmed", "done", "cancelled":
		db.Exec(`UPDATE bookings SET status=? WHERE id=?`, q.Status, q.ID)
		js(w, 200, map[string]string{"ok": "статус обновлён"})
	default:
		fail(w, 400, "неизвестный статус")
	}
}
