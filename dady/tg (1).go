package main

import (
	"sync"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Telegram: TG_TOKEN — токен бота, TG_CHAT — id чата владельца для уведомлений.

func tgSend(chat, text string) {
	tok := os.Getenv("TG_TOKEN")
	if tok == "" || chat == "" {
		return
	}
	r, err := http.PostForm("https://api.telegram.org/bot"+tok+"/sendMessage", url.Values{"chat_id": {chat}, "text": {text}})
	if err != nil {
		log.Println("telegram:", err)
		return
	}
	r.Body.Close()
}

// notify отправляет сообщение владельцу, если задан TG_CHAT.
func notify(text string) { go tgSend(os.Getenv("TG_CHAT"), text) }

// brief — короткое описание записи для уведомления.
func brief(id int64) string {
	var d, t, s, b, n, p string
	err := db.QueryRow(`SELECT b.date,printf('%02d:%02d',b.start/60,b.start%60),s.name,br.name,b.name,b.phone
	 FROM bookings b JOIN services s ON s.id=b.service JOIN barbers br ON br.id=b.barber WHERE b.id=?`, id).Scan(&d, &t, &s, &b, &n, &p)
	if err != nil {
		return fmt.Sprintf("запись №%d", id)
	}
	return fmt.Sprintf("%s %s\n%s, мастер %s\n%s, %s", d, t, s, b, n, p)
}

type tgFrom struct {
	First string `json:"first_name"`
}
type tgMsg struct {
	Text    string `json:"text"`
	Chat    struct{ ID int64 `json:"id"` } `json:"chat"`
	From    tgFrom `json:"from"`
	Contact *struct {
		Phone string `json:"phone_number"`
	} `json:"contact"`
}
type tgUpd struct {
	ID  int    `json:"update_id"`
	Msg *tgMsg `json:"message"`
	CB  *struct {
		ID   string `json:"id"`
		Data string `json:"data"`
		From tgFrom `json:"from"`
		Msg  tgMsg  `json:"message"`
	} `json:"callback_query"`
}
type sess struct {
	svc, barber int
	date, tm    string
	wait        bool
}

var (
	sessions = map[int64]*sess{}
	smu      sync.Mutex
	wd       = []string{"вс", "пн", "вт", "ср", "чт", "пт", "сб"}
)

func api(method string, v url.Values) {
	r, err := http.PostForm("https://api.telegram.org/bot"+os.Getenv("TG_TOKEN")+"/"+method, v)
	if err == nil {
		r.Body.Close()
	}
}
func ask(chat int64, text, markup string) {
	v := url.Values{"chat_id": {fmt.Sprint(chat)}, "text": {text}}
	if markup != "" {
		v.Set("reply_markup", markup)
	}
	api("sendMessage", v)
}
func kbd(cols int, items ...[2]string) string {
	var rows [][]map[string]string
	var row []map[string]string
	for i, it := range items {
		row = append(row, map[string]string{"text": it[0], "callback_data": it[1]})
		if len(row) == cols || i == len(items)-1 {
			rows = append(rows, row)
			row = nil
		}
	}
	b, _ := json.Marshal(map[string]any{"inline_keyboard": rows})
	return string(b)
}

func handle(u tgUpd) {
	if u.CB != nil {
		api("answerCallbackQuery", url.Values{"callback_query_id": {u.CB.ID}})
		step(u.CB.Msg.Chat.ID, u.CB.Data)
		return
	}
	if u.Msg == nil {
		return
	}
	chat, text := u.Msg.Chat.ID, strings.TrimSpace(u.Msg.Text)
	smu.Lock()
	s := sessions[chat]
	smu.Unlock()
	if s != nil && s.wait && !strings.HasPrefix(text, "/") {
		phone := text
		if u.Msg.Contact != nil {
			phone = u.Msg.Contact.Phone
		}
		finish(chat, s, u.Msg.From.First, phone)
		return
	}
	if strings.HasPrefix(text, "/start") || strings.HasPrefix(text, "/book") {
		id := fmt.Sprint(chat)
		if os.Getenv("TG_CHAT") == "" {
			ask(chat, "DADY бот работает.\nВаш chat id: "+id+"\nВпишите его в Render как TG_CHAT, чтобы получать уведомления о записях.", "")
		}
		ask(chat, "DADY — барбершоп. Записаться можно прямо здесь.", kbd(1, [2]string{"Записаться", "new"}))
	}
}

func step(chat int64, data string) {
	p := strings.SplitN(data, ":", 2)
	arg := ""
	if len(p) > 1 {
		arg = p[1]
	}
	smu.Lock()
	s := sessions[chat]
	if s == nil || p[0] == "new" {
		s = &sess{}
		sessions[chat] = s
	}
	smu.Unlock()
	switch p[0] {
	case "new":
		var it [][2]string
		r, _ := db.Query(`SELECT id,name,price,dur FROM services`)
		for r.Next() {
			var id, pr, d int
			var n string
			r.Scan(&id, &n, &pr, &d)
			it = append(it, [2]string{fmt.Sprintf("%s — %d ₽, %d мин", n, pr, d), fmt.Sprint("s:", id)})
		}
		r.Close()
		ask(chat, "Выберите услугу:", kbd(1, it...))
	case "s":
		s.svc, _ = strconv.Atoi(arg)
		it := [][2]string{{"Любой свободный мастер", "b:0"}}
		r, _ := db.Query(`SELECT id,name FROM barbers`)
		for r.Next() {
			var id int
			var n string
			r.Scan(&id, &n)
			it = append(it, [2]string{n, fmt.Sprint("b:", id)})
		}
		r.Close()
		ask(chat, "Выберите мастера:", kbd(1, it...))
	case "b":
		s.barber, _ = strconv.Atoi(arg)
		var it [][2]string
		for i := 0; i < 7; i++ {
			d := time.Now().AddDate(0, 0, i)
			it = append(it, [2]string{d.Format("02.01") + " " + wd[d.Weekday()], "d:" + d.Format("2006-01-02")})
		}
		ask(chat, "Выберите дату:", kbd(4, it...))
	case "d":
		s.date = arg
		dur, _ := svcDur(s.svc)
		var it [][2]string
		for t := openMin; t+dur <= closeMin; t += stepMin {
			if pick(s.barber, s.date, t, dur, 0) > 0 {
				it = append(it, [2]string{hm(t), "t:" + hm(t)})
			}
		}
		if len(it) == 0 {
			ask(chat, "На этот день свободного времени нет. Выберите другую дату.", kbd(1, [2]string{"Выбрать дату", "b:" + fmt.Sprint(s.barber)}))
			return
		}
		ask(chat, "Свободное время:", kbd(4, it...))
	case "t":
		s.tm, s.wait = arg, true
		ask(chat, "Отправьте номер телефона: нажмите кнопку ниже или напишите его сообщением.",
			`{"keyboard":[[{"text":"Отправить мой номер","request_contact":true}]],"resize_keyboard":true,"one_time_keyboard":true}`)
	}
}

func finish(chat int64, s *sess, name, phone string) {
	if name == "" {
		name = "Клиент Telegram"
	}
	id, _, err := createBooking(s.svc, s.barber, s.date, s.tm, name, phone, chat)
	smu.Lock()
	delete(sessions, chat)
	smu.Unlock()
	if err != nil {
		ask(chat, err.Error()+"\nНажмите /start, чтобы начать заново.", `{"remove_keyboard":true}`)
		return
	}
	ask(chat, "Вы записаны:\n"+brief(id), `{"remove_keyboard":true}`)
}

// startBot слушает сообщения боту.
func startBot() {
	tok := os.Getenv("TG_TOKEN")
	if tok == "" {
		log.Println("TG_TOKEN не задан — Telegram-бот выключен")
		return
	}
	go func() {
		offset := 0
		for {
			r, err := http.Get(fmt.Sprintf("https://api.telegram.org/bot%s/getUpdates?timeout=30&offset=%d", tok, offset))
			if err != nil {
				time.Sleep(5 * time.Second)
				continue
			}
			var res struct {
				Result []tgUpd `json:"result"`
			}
			json.NewDecoder(r.Body).Decode(&res)
			r.Body.Close()
			for _, u := range res.Result {
				offset = u.ID + 1
				handle(u)
			}
			if len(res.Result) == 0 {
				time.Sleep(time.Second)
			}
		}
	}()
}
