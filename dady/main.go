package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
	_ "time/tzdata"
)

const stepMin = 30

var (
	db      *sql.DB
	mu      sync.Mutex // записи создаются по одной, чтобы не было двойной записи
	phoneRe = regexp.MustCompile(`^\+?[0-9]{10,15}$`)
	statuses = map[string]bool{"pending": true, "confirmed": true, "arrived": true, "done": true,
		"cancelled_client": true, "cancelled_admin": true, "no_show": true}
)

// активные записи занимают время; отменённые и «не пришёл» — нет
const busy = `status NOT IN('cancelled_client','cancelled_admin','no_show')`

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func main() {
	if loc, e := time.LoadLocation(env("TZ_NAME", "Europe/Moscow")); e == nil {
		time.Local = loc
	}
	var err error
	db, err = sql.Open("sqlite", env("DB_PATH", "salon.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		log.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if err = initDB(); err != nil {
		log.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.Dir("static")))
	mux.HandleFunc("GET /api/services", servicesH)
	mux.HandleFunc("GET /api/masters", mastersH)
	mux.HandleFunc("GET /api/slots", slotsH)
	mux.HandleFunc("POST /api/book", bookH)
	mux.HandleFunc("GET /api/manage/{token}", manageH)
	mux.HandleFunc("POST /api/manage/{token}/cancel", manageCancelH)
	mux.HandleFunc("POST /api/admin/login", loginH)
	mux.HandleFunc("POST /api/admin/logout", logoutH)
	mux.HandleFunc("GET /api/admin/me", auth(func(w http.ResponseWriter, r *http.Request) { js(w, 200, map[string]bool{"ok": true}) }))
	mux.HandleFunc("GET /api/admin/stats", auth(statsH))
	mux.HandleFunc("GET /api/admin/bookings", auth(adminBookingsH))
	mux.HandleFunc("POST /api/admin/status", auth(adminStatusH))
	mux.HandleFunc("GET /api/admin/services", auth(adminServicesH))
	mux.HandleFunc("POST /api/admin/service", auth(adminServiceSaveH))
	mux.HandleFunc("GET /api/admin/masters", auth(adminMastersH))
	mux.HandleFunc("POST /api/admin/master", auth(adminMasterSaveH))
	mux.HandleFunc("GET /api/admin/offs", auth(adminOffsH))
	mux.HandleFunc("POST /api/admin/off", auth(adminOffAddH))
	mux.HandleFunc("POST /api/admin/off/delete", auth(adminOffDelH))
	addr := ":" + env("PORT", "8080")
	log.Println("Салон запущен на", addr)
	log.Fatal(http.ListenAndServe(addr, secure(mux)))
}

func initDB() error {
	_, err := db.Exec(`
CREATE TABLE IF NOT EXISTS services(id INTEGER PRIMARY KEY, name TEXT, descr TEXT, price INT, dur INT, active INT DEFAULT 1);
CREATE TABLE IF NOT EXISTS masters(id INTEGER PRIMARY KEY, name TEXT, spec TEXT, open_min INT DEFAULT 600, close_min INT DEFAULT 1200, active INT DEFAULT 1);
CREATE TABLE IF NOT EXISTS bookings(id INTEGER PRIMARY KEY, service INT, master INT, date TEXT, start INT, dur INT, price INT,
  name TEXT, phone TEXT, comment TEXT, status TEXT DEFAULT 'pending', token TEXT UNIQUE, req TEXT UNIQUE, created TEXT DEFAULT CURRENT_TIMESTAMP);
CREATE INDEX IF NOT EXISTS ix_bk ON bookings(master, date);
CREATE TABLE IF NOT EXISTS offs(id INTEGER PRIMARY KEY, master INT DEFAULT 0, from_date TEXT, to_date TEXT, from_min INT, to_min INT, note TEXT);
CREATE TABLE IF NOT EXISTS users(id INTEGER PRIMARY KEY, login TEXT UNIQUE, hash TEXT);
CREATE TABLE IF NOT EXISTS sessions(sid TEXT PRIMARY KEY, exp INT);`)
	if err != nil {
		return err
	}
	for _, col := range []string{"descr TEXT DEFAULT ''", "days TEXT DEFAULT '0,1,2,3,4,5,6'", "break_from INT DEFAULT 0", "break_to INT DEFAULT 0"} {
		db.Exec(`ALTER TABLE masters ADD COLUMN ` + col) // ошибка «колонка уже есть» нормальна
	}
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM services`).Scan(&n)
	if n == 0 {
		db.Exec(`INSERT INTO services(name,descr,price,dur) VALUES
 ('Маникюр без покрытия','Аккуратная обработка ногтей и кутикулы',1500,60),
 ('Маникюр с гель-лаком','Маникюр и стойкое покрытие на выбор',2500,90),
 ('Снятие покрытия','Бережное снятие гель-лака',700,30),
 ('Наращивание ногтей','Моделирование формы и длины',4000,150),
 ('Коррекция ногтей','Поддержание формы нарощенных ногтей',3200,120),
 ('Дизайн ногтей','Рисунки, стразы, втирки',500,30),
 ('Укрепление ногтей','Укрепляющее покрытие для натуральных ногтей',1200,45),
 ('Педикюр','Комплексный уход за стопами',3000,90);
INSERT INTO masters(name,spec) VALUES('Анна','Маникюр, дизайн'),('Мария','Наращивание, коррекция'),('Елена','Педикюр, укрепление');`)
	}
	// первый администратор из переменных окружения
	db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n)
	if n == 0 {
		login, pw := env("ADMIN_LOGIN", "admin"), os.Getenv("ADMIN_PASSWORD")
		if pw == "" {
			pw = randHex(6)
			log.Println("ПАРОЛЬ АДМИНИСТРАТОРА (показан один раз):", pw)
		}
		db.Exec(`INSERT INTO users(login,hash) VALUES(?,?)`, login, hashPw(pw))
	}
	return nil
}

// ---------- вспомогательное ----------
func js(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}
func fail(w http.ResponseWriter, code int, msg string) { js(w, code, map[string]string{"error": msg}) }
func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}
func hm(m int) string          { return time.Date(0, 1, 1, m/60, m%60, 0, 0, time.UTC).Format("15:04") }
func cleanPhone(p string) string { return strings.NewReplacer(" ", "", "-", "", "(", "", ")", "").Replace(p) }
func parseHM(s string) (int, error) {
	t, err := time.Parse("15:04", s)
	return t.Hour()*60 + t.Minute(), err
}

func secure(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "same-origin")
		if r.Method == "POST" && r.Header.Get("X-Requested-With") != "fetch" { // защита от межсайтовых запросов
			fail(w, 403, "запрос отклонён")
			return
		}
		h.ServeHTTP(w, r)
	})
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

// ---------- пароли и сессии ----------
func pbkdf2(pw, salt []byte, iter int) []byte {
	prf := hmac.New(sha256.New, pw)
	prf.Write(salt)
	prf.Write([]byte{0, 0, 0, 1})
	u := prf.Sum(nil)
	t := append([]byte(nil), u...)
	for i := 1; i < iter; i++ {
		prf.Reset()
		prf.Write(u)
		u = prf.Sum(nil)
		for j := range t {
			t[j] ^= u[j]
		}
	}
	return t
}
func hashPw(pw string) string {
	salt := make([]byte, 16)
	rand.Read(salt)
	return hex.EncodeToString(salt) + "$" + hex.EncodeToString(pbkdf2([]byte(pw), salt, 120000))
}
func checkPw(pw, stored string) bool {
	p := strings.SplitN(stored, "$", 2)
	if len(p) != 2 {
		return false
	}
	salt, _ := hex.DecodeString(p[0])
	want, _ := hex.DecodeString(p[1])
	return subtle.ConstantTimeCompare(pbkdf2([]byte(pw), salt, 120000), want) == 1
}
func sidHash(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }

var (
	tries   = map[string][]time.Time{}
	triesMu sync.Mutex
)

func tooMany(ip string) bool { // не более 8 попыток входа за 10 минут с одного адреса
	triesMu.Lock()
	defer triesMu.Unlock()
	var keep []time.Time
	for _, t := range tries[ip] {
		if time.Since(t) < 10*time.Minute {
			keep = append(keep, t)
		}
	}
	tries[ip] = keep
	return len(keep) >= 8
}

func loginH(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if tooMany(ip) {
		fail(w, 429, "слишком много попыток, подождите 10 минут")
		return
	}
	var q struct{ Login, Password string }
	json.NewDecoder(r.Body).Decode(&q)
	var stored string
	err := db.QueryRow(`SELECT hash FROM users WHERE login=?`, q.Login).Scan(&stored)
	if err != nil || !checkPw(q.Password, stored) {
		triesMu.Lock()
		tries[ip] = append(tries[ip], time.Now())
		triesMu.Unlock()
		fail(w, 401, "неверный логин или пароль")
		return
	}
	sid := randHex(32)
	db.Exec(`INSERT INTO sessions(sid,exp) VALUES(?,?)`, sidHash(sid), time.Now().Add(12*time.Hour).Unix())
	http.SetCookie(w, &http.Cookie{Name: "sid", Value: sid, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode,
		Secure: os.Getenv("COOKIE_SECURE") == "1", MaxAge: 12 * 3600})
	js(w, 200, map[string]bool{"ok": true})
}

func logoutH(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie("sid"); err == nil {
		db.Exec(`DELETE FROM sessions WHERE sid=?`, sidHash(c.Value))
	}
	http.SetCookie(w, &http.Cookie{Name: "sid", Path: "/", MaxAge: -1})
	js(w, 200, map[string]bool{"ok": true})
}

func auth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie("sid")
		var exp int64
		if err != nil || db.QueryRow(`SELECT exp FROM sessions WHERE sid=?`, sidHash(c.Value)).Scan(&exp) != nil || exp < time.Now().Unix() {
			fail(w, 401, "нужен вход")
			return
		}
		h(w, r)
	}
}

// ---------- услуги, мастера, свободное время ----------
func servicesH(w http.ResponseWriter, _ *http.Request) {
	rows(w, `SELECT id,name,descr,price,dur FROM services WHERE active=1 ORDER BY id`)
}
func mastersH(w http.ResponseWriter, _ *http.Request) {
	rows(w, `SELECT id,name,spec,COALESCE(descr,'') AS descr FROM masters WHERE active=1 ORDER BY id`)
}

func isFuture(date string, start int) bool {
	d, err := time.ParseInLocation("2006-01-02", date, time.Local)
	return err == nil && d.Add(time.Duration(start)*time.Minute).After(time.Now())
}

// works: мастер работает в этот день и время, не на перерыве и не в отпуске/выходном
func works(master int, date string, start, dur int) bool {
	var o, c, bf, bt int
	var days string
	if db.QueryRow(`SELECT open_min,close_min,COALESCE(break_from,0),COALESCE(break_to,0),COALESCE(days,'0,1,2,3,4,5,6') FROM masters WHERE id=? AND active=1`,
		master).Scan(&o, &c, &bf, &bt, &days) != nil {
		return false
	}
	d, err := time.ParseInLocation("2006-01-02", date, time.Local)
	if err != nil || !strings.Contains(","+days+",", ","+strconv.Itoa(int(d.Weekday()))+",") {
		return false
	}
	if start < o || start+dur > c || (bt > bf && start < bt && start+dur > bf) {
		return false
	}
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM offs WHERE master IN(0,?) AND from_date<=? AND to_date>=? AND from_min<? AND to_min>?`,
		master, date, date, start+dur, start).Scan(&n)
	return n == 0
}

func free(master int, date string, start, dur, skip int) bool {
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM bookings WHERE master=? AND date=? AND `+busy+` AND id!=? AND start<? AND start+dur>?`,
		master, date, skip, start+dur, start).Scan(&n)
	return n == 0
}

// pick возвращает свободного мастера (0 — нет). master>0 — конкретный мастер.
func pick(master int, date string, start, dur int) int {
	if !isFuture(date, start) {
		return 0
	}
	if master > 0 {
		if works(master, date, start, dur) && free(master, date, start, dur, 0) {
			return master
		}
		return 0
	}
	r, _ := db.Query(`SELECT id FROM masters WHERE active=1 ORDER BY id`)
	var ids []int
	for r.Next() {
		var id int
		r.Scan(&id)
		ids = append(ids, id)
	}
	r.Close()
	for _, id := range ids {
		if works(id, date, start, dur) && free(id, date, start, dur, 0) {
			return id
		}
	}
	return 0
}

func serviceInfo(id int) (price, dur int, ok bool) {
	ok = db.QueryRow(`SELECT price,dur FROM services WHERE id=? AND active=1`, id).Scan(&price, &dur) == nil
	return
}

func slotsH(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	sid, _ := strconv.Atoi(q.Get("service"))
	mid, _ := strconv.Atoi(q.Get("master"))
	_, dur, ok := serviceInfo(sid)
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
		if pick(mid, q.Get("date"), t, dur) > 0 {
			out = append(out, hm(t))
		}
	}
	js(w, 200, out)
}

// ---------- запись ----------
func notify(text string) {
	tok, chat := os.Getenv("TELEGRAM_BOT_TOKEN"), os.Getenv("TELEGRAM_CHAT_ID")
	if tok == "" || chat == "" {
		log.Println("уведомление:", text)
		return
	}
	go http.PostForm("https://api.telegram.org/bot"+tok+"/sendMessage", url.Values{"chat_id": {chat}, "text": {text}})
}

const bookingSQL = `SELECT b.id,b.date,printf('%02d:%02d',b.start/60,b.start%60) AS time,b.status,b.name,b.phone,b.comment,b.price,b.token,
 s.name AS service,m.name AS master FROM bookings b JOIN services s ON s.id=b.service JOIN masters m ON m.id=b.master`

func bookH(w http.ResponseWriter, r *http.Request) {
	var q struct {
		Service, Master                  int
		Date, Time, Name, Phone, Comment string
		Req                              string
	}
	if json.NewDecoder(r.Body).Decode(&q) != nil {
		fail(w, 400, "неверный запрос")
		return
	}
	phone, name := cleanPhone(q.Phone), strings.TrimSpace(q.Name)
	if name == "" || len([]rune(name)) > 60 || !phoneRe.MatchString(phone) || len([]rune(q.Comment)) > 300 || q.Req == "" {
		fail(w, 400, "укажите имя и корректный телефон (10–15 цифр)")
		return
	}
	price, dur, ok := serviceInfo(q.Service)
	start, err := parseHM(q.Time)
	if !ok || err != nil {
		fail(w, 400, "выберите услугу и время")
		return
	}
	mu.Lock()
	defer mu.Unlock()
	var existing int // повторная отправка той же формы возвращает ту же запись
	if db.QueryRow(`SELECT id FROM bookings WHERE req=?`, q.Req).Scan(&existing) == nil {
		rows(w, bookingSQL+` WHERE b.id=?`, existing)
		return
	}
	m := pick(q.Master, q.Date, start, dur)
	if m == 0 {
		fail(w, 409, "это время уже занято или недоступно — выберите другое")
		return
	}
	res, err := db.Exec(`INSERT INTO bookings(service,master,date,start,dur,price,name,phone,comment,token,req) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		q.Service, m, q.Date, start, dur, price, name, phone, strings.TrimSpace(q.Comment), randHex(16), q.Req)
	if err != nil {
		fail(w, 500, "не удалось сохранить запись")
		return
	}
	id, _ := res.LastInsertId()
	notify("Новая запись (ожидает подтверждения): " + name + ", " + phone + ", " + q.Date + " " + q.Time)
	rows(w, bookingSQL+` WHERE b.id=?`, id)
}

// ---------- управление записью клиентом по ссылке ----------
func manageH(w http.ResponseWriter, r *http.Request) {
	rows(w, bookingSQL+` WHERE b.token=?`, r.PathValue("token"))
}

func manageCancelH(w http.ResponseWriter, r *http.Request) {
	mu.Lock()
	defer mu.Unlock()
	var id, start int
	var date, st string
	if db.QueryRow(`SELECT id,date,start,status FROM bookings WHERE token=?`, r.PathValue("token")).Scan(&id, &date, &start, &st) != nil {
		fail(w, 404, "запись не найдена")
		return
	}
	hours, _ := strconv.Atoi(env("MIN_CANCEL_HOURS", "2"))
	d, _ := time.ParseInLocation("2006-01-02", date, time.Local)
	if st != "pending" && st != "confirmed" {
		fail(w, 400, "эту запись уже нельзя отменить")
		return
	}
	if time.Until(d.Add(time.Duration(start)*time.Minute)) < time.Duration(hours)*time.Hour {
		fail(w, 400, "отмена возможна не позднее чем за "+strconv.Itoa(hours)+" ч до визита — позвоните в салон")
		return
	}
	db.Exec(`UPDATE bookings SET status='cancelled_client' WHERE id=?`, id)
	notify("Клиент отменил запись: " + date + " " + hm(start))
	js(w, 200, map[string]string{"ok": "запись отменена"})
}

// ---------- администратор ----------
func statsH(w http.ResponseWriter, _ *http.Request) {
	today := time.Now().Format("2006-01-02")
	month := today[:7] + "%"
	var a, b, c, d, rd, rm, cl int
	db.QueryRow(`SELECT COUNT(*) FROM bookings WHERE date=? AND `+busy, today).Scan(&a)
	db.QueryRow(`SELECT COUNT(*) FROM bookings WHERE status='pending'`).Scan(&b)
	db.QueryRow(`SELECT COUNT(*) FROM bookings WHERE status='confirmed'`).Scan(&c)
	db.QueryRow(`SELECT COUNT(*) FROM bookings WHERE status IN('cancelled_client','cancelled_admin') AND date LIKE ?`, month).Scan(&d)
	db.QueryRow(`SELECT COALESCE(SUM(price),0) FROM bookings WHERE status='done' AND date=?`, today).Scan(&rd)
	db.QueryRow(`SELECT COALESCE(SUM(price),0) FROM bookings WHERE status='done' AND date LIKE ?`, month).Scan(&rm)
	db.QueryRow(`SELECT COUNT(DISTINCT phone) FROM bookings`).Scan(&cl)
	js(w, 200, map[string]int{"today": a, "pending": b, "confirmed": c, "cancelled_month": d, "revenue_day": rd, "revenue_month": rm, "clients": cl})
}

func adminBookingsH(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	sql := bookingSQL + ` WHERE 1=1`
	var args []any
	if v := q.Get("date"); v != "" {
		sql += ` AND b.date=?`
		args = append(args, v)
	}
	if v := q.Get("status"); v != "" {
		sql += ` AND b.status=?`
		args = append(args, v)
	}
	if v := strings.TrimSpace(q.Get("q")); v != "" {
		sql += ` AND (b.name LIKE ? OR b.phone LIKE ?)`
		args = append(args, "%"+v+"%", "%"+v+"%")
	}
	rows(w, sql+` ORDER BY b.date DESC,b.start LIMIT 300`, args...)
}

func adminStatusH(w http.ResponseWriter, r *http.Request) {
	var q struct {
		ID     int
		Status string
	}
	json.NewDecoder(r.Body).Decode(&q)
	if !statuses[q.Status] {
		fail(w, 400, "неизвестный статус")
		return
	}
	db.Exec(`UPDATE bookings SET status=? WHERE id=?`, q.Status, q.ID)
	js(w, 200, map[string]string{"ok": "статус обновлён"})
}

func adminServicesH(w http.ResponseWriter, _ *http.Request) {
	rows(w, `SELECT id,name,descr,price,dur,active FROM services ORDER BY id`)
}

func adminServiceSaveH(w http.ResponseWriter, r *http.Request) {
	var q struct {
		ID, Price, Dur, Active int
		Name, Descr          string
	}
	json.NewDecoder(r.Body).Decode(&q)
	if strings.TrimSpace(q.Name) == "" || q.Price < 0 || q.Dur < 15 || q.Dur > 480 {
		fail(w, 400, "проверьте название, цену и длительность (15–480 мин)")
		return
	}
	if q.ID > 0 {
		db.Exec(`UPDATE services SET name=?,descr=?,price=?,dur=?,active=? WHERE id=?`, q.Name, q.Descr, q.Price, q.Dur, q.Active, q.ID)
	} else {
		db.Exec(`INSERT INTO services(name,descr,price,dur,active) VALUES(?,?,?,?,?)`, q.Name, q.Descr, q.Price, q.Dur, q.Active)
	}
	js(w, 200, map[string]string{"ok": "сохранено"})
}

// ---------- мастера и график ----------
func adminMastersH(w http.ResponseWriter, _ *http.Request) {
	rows(w, `SELECT id,name,spec,COALESCE(descr,'') AS descr,open_min,close_min,COALESCE(break_from,0) AS break_from,COALESCE(break_to,0) AS break_to,
 COALESCE(days,'0,1,2,3,4,5,6') AS days,active FROM masters ORDER BY id`)
}

func adminMasterSaveH(w http.ResponseWriter, r *http.Request) {
	var q struct {
		ID, Active                                        int
		Name, Spec, Descr, Open, Close, BreakFrom, BreakTo, Days string
	}
	json.NewDecoder(r.Body).Decode(&q)
	o, e1 := parseHM(q.Open)
	c, e2 := parseHM(q.Close)
	if strings.TrimSpace(q.Name) == "" || e1 != nil || e2 != nil || c <= o {
		fail(w, 400, "укажите имя и рабочие часы (начало раньше конца)")
		return
	}
	bf, bt := 0, 0
	if q.BreakFrom != "" || q.BreakTo != "" {
		var e3, e4 error
		bf, e3 = parseHM(q.BreakFrom)
		bt, e4 = parseHM(q.BreakTo)
		if e3 != nil || e4 != nil || bt <= bf {
			fail(w, 400, "перерыв: начало должно быть раньше конца")
			return
		}
	}
	days := strings.Split(q.Days, ",")
	for _, d := range days {
		if len(d) != 1 || d < "0" || d > "6" {
			fail(w, 400, "выберите рабочие дни")
			return
		}
	}
	if q.ID > 0 {
		db.Exec(`UPDATE masters SET name=?,spec=?,descr=?,open_min=?,close_min=?,break_from=?,break_to=?,days=?,active=? WHERE id=?`,
			q.Name, q.Spec, q.Descr, o, c, bf, bt, q.Days, q.Active, q.ID)
	} else {
		db.Exec(`INSERT INTO masters(name,spec,descr,open_min,close_min,break_from,break_to,days,active) VALUES(?,?,?,?,?,?,?,?,?)`,
			q.Name, q.Spec, q.Descr, o, c, bf, bt, q.Days, q.Active)
	}
	js(w, 200, map[string]string{"ok": "сохранено"})
}

func adminOffsH(w http.ResponseWriter, _ *http.Request) {
	rows(w, `SELECT o.id,o.master,COALESCE(m.name,'Весь салон') AS who,o.from_date,o.to_date,o.from_min,o.to_min,o.note
 FROM offs o LEFT JOIN masters m ON m.id=o.master WHERE o.to_date>=? ORDER BY o.from_date`, time.Now().Format("2006-01-02"))
}

// adminOffAddH: выходной, отпуск или блокировка времени. Master=0 — весь салон.
func adminOffAddH(w http.ResponseWriter, r *http.Request) {
	var q struct {
		Master                         int
		From, To, TimeFrom, TimeTo, Note string
	}
	json.NewDecoder(r.Body).Decode(&q)
	_, e1 := time.Parse("2006-01-02", q.From)
	_, e2 := time.Parse("2006-01-02", q.To)
	if q.To == "" {
		q.To, e2 = q.From, nil
	}
	if e1 != nil || e2 != nil || q.To < q.From {
		fail(w, 400, "укажите даты (конец не раньше начала)")
		return
	}
	f, t := 0, 24*60
	if q.TimeFrom != "" || q.TimeTo != "" {
		var e3, e4 error
		f, e3 = parseHM(q.TimeFrom)
		t, e4 = parseHM(q.TimeTo)
		if e3 != nil || e4 != nil || t <= f {
			fail(w, 400, "время «с» должно быть раньше «до»")
			return
		}
	}
	db.Exec(`INSERT INTO offs(master,from_date,to_date,from_min,to_min,note) VALUES(?,?,?,?,?,?)`, q.Master, q.From, q.To, f, t, strings.TrimSpace(q.Note))
	js(w, 200, map[string]string{"ok": "добавлено"})
}

func adminOffDelH(w http.ResponseWriter, r *http.Request) {
	var q struct{ ID int }
	json.NewDecoder(r.Body).Decode(&q)
	db.Exec(`DELETE FROM offs WHERE id=?`, q.ID)
	js(w, 200, map[string]string{"ok": "удалено"})
}

// clientIP: за прокси хостинга настоящий адрес клиента приходит в X-Forwarded-For
func clientIP(r *http.Request) string {
	if x := r.Header.Get("X-Forwarded-For"); x != "" {
		return strings.TrimSpace(strings.Split(x, ",")[0])
	}
	return strings.Split(r.RemoteAddr, ":")[0]
}
