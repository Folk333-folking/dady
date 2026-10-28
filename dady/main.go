package main

import (
	"crypto/subtle"
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
	_ "time/tzdata"
)

const (
	openMin  = 10 * 60 // 10:00 — часы по умолчанию (сайт берёт реальные из таблицы barbers)
	closeMin = 21 * 60 // 21:00
	stepMin  = 30
)

var (
	db      *sql.DB
	mu      sync.Mutex // serialises booking writes (plus a single DB connection)
	phoneRe = regexp.MustCompile(`^\+?[0-9]{10,15}$`)
)

func main() {
	if loc, e := time.LoadLocation("Europe/Moscow"); e == nil {
		time.Local = loc // Архангельск живёт по московскому времени
	}
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
	mux.HandleFunc("GET /api/admin/blocks", admin(adminBlocks))
	mux.HandleFunc("POST /api/admin/block", admin(adminBlock))
	mux.HandleFunc("POST /api/admin/unblock", admin(adminUnblock))
	mux.HandleFunc("POST /api/admin/hours", admin(adminHours))
	mux.HandleFunc("GET /api/reviews", reviewsH)
	mux.HandleFunc("POST /api/review", reviewH)
	mux.HandleFunc("POST /api/admin/review/delete", admin(adminReviewDelete))
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
CREATE INDEX IF NOT EXISTS ix_b ON bookings(barber, date);
CREATE TABLE IF NOT EXISTS blocks(id INTEGER PRIMARY KEY, barber INT DEFAULT 0, date TEXT, from_min INT, to_min INT, note TEXT);
CREATE INDEX IF NOT EXISTS ix_blk ON blocks(date);
CREATE TABLE IF NOT EXISTS reviews(id INTEGER PRIMARY KEY, booking INT UNIQUE, barber INT, name TEXT, stars INT, text TEXT, created TEXT DEFAULT CURRENT_TIMESTAMP);`)
	if err != nil {
		return err
	}
	db.Exec(`ALTER TABLE bookings ADD COLUMN chat INT`) // ошибка «колонка уже есть» нормальна
	db.Exec(`ALTER TABLE barbers ADD COLUMN open_min INT DEFAULT 600`)   // рабочие часы мастера по умолчанию 10:00–21:00
	db.Exec(`ALTER TABLE barbers ADD COLUMN close_min INT DEFAULT 1260`)
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
func listBarbers(w http.ResponseWriter, _ *http.Request)  { rows(w, `SELECT b.*, COALESCE((SELECT ROUND(AVG(stars),1) FROM reviews WHERE barber=b.id),0) AS rating,
 (SELECT COUNT(*) FROM reviews WHERE barber=b.id) AS reviews FROM barbers b`) }

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
	if n > 0 {
		return false
	}
	// закрытое время: barber=0 значит «все мастера»
	db.QueryRow(`SELECT COUNT(*) FROM blocks WHERE date=? AND barber IN(0,?) AND from_min<? AND to_min>?`,
		date, barber, start+dur, start).Scan(&n)
	return n == 0
}

// works reports whether [start,start+dur) fits into the barber's working hours.
func works(barber, start, dur int) bool {
	var o, c int
	if db.QueryRow(`SELECT open_min,close_min FROM barbers WHERE id=?`, barber).Scan(&o, &c) != nil {
		return false
	}
	return start >= o && start+dur <= c
}

// pick returns a free barber id (0 if none). barber>0 means a specific master.
func pick(barber int, date string, start, dur, skip int) int {
	if !isFuture(date, start) {
		return 0
	}
	if barber > 0 {
		if works(barber, start, dur) && free(barber, date, start, dur, skip) {
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
		if works(id, start, dur) && free(id, date, start, dur, skip) {
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
	if _, e := time.Parse("2006-01-02", q.Get("date")); e != nil {
		fail(w, 400, "выберите дату")
		return
	}
	out := []string{}
	for t := 0; t+dur <= 24*60; t += stepMin {
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
	id, code, err := createBooking(q.Service, q.Barber, q.Date, q.Time, q.Name, q.Phone, 0)
	if err != nil {
		fail(w, code, err.Error())
		return
	}
	rows(w, bookingSQL+` WHERE b.id=?`, id)
}

// createBooking — общая логика записи для сайта и Telegram-бота.
func createBooking(svc, barber int, date, tm, name, phone string, chat int64) (int64, int, error) {
	phone, name = cleanPhone(phone), strings.TrimSpace(name)
	if name == "" || !phoneRe.MatchString(phone) {
		return 0, 400, errors.New("укажите имя и корректный телефон (10–15 цифр)")
	}
	dur, ok := svcDur(svc)
	start, err := parseHM(tm)
	if !ok || err != nil {
		return 0, 400, errors.New("выберите услугу и время")
	}
	mu.Lock()
	b := pick(barber, date, start, dur, 0)
	if b == 0 {
		mu.Unlock()
		return 0, 409, errors.New("это время уже занято или недоступно — выберите другое")
	}
	res, err := db.Exec(`INSERT INTO bookings(service,barber,date,start,dur,name,phone,chat) VALUES(?,?,?,?,?,?,?,?)`,
		svc, b, date, start, dur, name, phone, chat)
	mu.Unlock()
	if err != nil {
		return 0, 500, errors.New("не удалось сохранить запись")
	}
	id, _ := res.LastInsertId()
	notify("Новая запись:\n" + brief(id))
	return id, 200, nil
}

const bookingSQL = `SELECT b.id,b.date,printf('%02d:%02d',b.start/60,b.start%60) AS time,b.status,b.name,b.phone,
 s.name AS service,s.price,br.name AS barber,b.service AS sid,b.barber AS bid,(SELECT COUNT(*) FROM reviews rv WHERE rv.booking=b.id) AS reviewed FROM bookings b JOIN services s ON s.id=b.service JOIN barbers br ON br.id=b.barber`

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
		if pw == "" || subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Admin")), []byte(pw)) != 1 {
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

// ---------- график: закрытые дни/часы и рабочие часы мастеров ----------
func adminBlocks(w http.ResponseWriter, _ *http.Request) {
	rows(w, `SELECT bl.id, bl.date, bl.from_min, bl.to_min, bl.note, COALESCE(br.name,'Все мастера') AS barber
 FROM blocks bl LEFT JOIN barbers br ON br.id=bl.barber WHERE bl.date>=? ORDER BY bl.date, bl.from_min`,
		time.Now().Format("2006-01-02"))
}

// adminBlock закрывает время. Пустые From/To — закрыть весь день. Barber=0 — все мастера.
func adminBlock(w http.ResponseWriter, r *http.Request) {
	var q struct {
		Barber           int
		Date, From, To, Note string
	}
	json.NewDecoder(r.Body).Decode(&q)
	if _, err := time.Parse("2006-01-02", q.Date); err != nil {
		fail(w, 400, "укажите дату")
		return
	}
	f, t := 0, 24*60
	if q.From != "" || q.To != "" {
		var e1, e2 error
		f, e1 = parseHM(q.From)
		t, e2 = parseHM(q.To)
		if e1 != nil || e2 != nil || t <= f {
			fail(w, 400, "время «с» должно быть раньше времени «до»")
			return
		}
	}
	if _, err := db.Exec(`INSERT INTO blocks(barber,date,from_min,to_min,note) VALUES(?,?,?,?,?)`,
		q.Barber, q.Date, f, t, strings.TrimSpace(q.Note)); err != nil {
		fail(w, 500, "не удалось сохранить")
		return
	}
	js(w, 200, map[string]string{"ok": "закрыто"})
}

func adminUnblock(w http.ResponseWriter, r *http.Request) {
	var q struct{ ID int }
	json.NewDecoder(r.Body).Decode(&q)
	db.Exec(`DELETE FROM blocks WHERE id=?`, q.ID)
	js(w, 200, map[string]string{"ok": "открыто"})
}

func adminHours(w http.ResponseWriter, r *http.Request) {
	var q struct {
		Barber      int
		Open, Close string
	}
	json.NewDecoder(r.Body).Decode(&q)
	o, e1 := parseHM(q.Open)
	c, e2 := parseHM(q.Close)
	if e1 != nil || e2 != nil || c <= o {
		fail(w, 400, "начало работы должно быть раньше конца")
		return
	}
	db.Exec(`UPDATE barbers SET open_min=?, close_min=? WHERE id=?`, o, c, q.Barber)
	js(w, 200, map[string]string{"ok": "часы сохранены"})
}

// ---------- отзывы ----------
func reviewsH(w http.ResponseWriter, _ *http.Request) {
	rows(w, `SELECT r.id, r.name, r.stars, r.text, substr(r.created,1,10) AS date, br.name AS barber
 FROM reviews r JOIN barbers br ON br.id=r.barber ORDER BY r.id DESC LIMIT 30`)
}

// reviewH: отзыв можно оставить только на завершённую запись и только один раз.
func reviewH(w http.ResponseWriter, r *http.Request) {
	var q struct {
		ID, Stars   int
		Phone, Text string
	}
	if json.NewDecoder(r.Body).Decode(&q) != nil || q.Stars < 1 || q.Stars > 5 {
		fail(w, 400, "поставьте оценку от 1 до 5")
		return
	}
	text := strings.TrimSpace(q.Text)
	if len([]rune(text)) > 500 {
		fail(w, 400, "отзыв не длиннее 500 символов")
		return
	}
	var barber int
	var name string
	if db.QueryRow(`SELECT barber,name FROM bookings WHERE id=? AND phone=? AND status='done'`,
		q.ID, cleanPhone(q.Phone)).Scan(&barber, &name) != nil {
		fail(w, 400, "отзыв можно оставить только после завершённого визита")
		return
	}
	first := "Гость" // показываем только имя, без фамилии и телефона
	if f := strings.Fields(name); len(f) > 0 {
		first = f[0]
	}
	if _, err := db.Exec(`INSERT INTO reviews(booking,barber,name,stars,text) VALUES(?,?,?,?,?)`,
		q.ID, barber, first, q.Stars, text); err != nil {
		fail(w, 409, "на этот визит отзыв уже оставлен")
		return
	}
	js(w, 200, map[string]string{"ok": "спасибо за отзыв"})
}

func adminReviewDelete(w http.ResponseWriter, r *http.Request) {
	var q struct{ ID int }
	json.NewDecoder(r.Body).Decode(&q)
	db.Exec(`DELETE FROM reviews WHERE id=?`, q.ID)
	js(w, 200, map[string]string{"ok": "удалено"})
}
