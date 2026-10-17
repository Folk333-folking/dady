package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
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

// startBot слушает сообщения боту. На /start отвечает id чата, который нужно записать в TG_CHAT.
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
				Result []struct {
					ID  int `json:"update_id"`
					Msg struct {
						Text string `json:"text"`
						Chat struct {
							ID int64 `json:"id"`
						} `json:"chat"`
					} `json:"message"`
				} `json:"result"`
			}
			json.NewDecoder(r.Body).Decode(&res)
			r.Body.Close()
			for _, u := range res.Result {
				offset = u.ID + 1
				if strings.HasPrefix(u.Msg.Text, "/start") {
					id := fmt.Sprint(u.Msg.Chat.ID)
					msg := "DADY бот работает.\nВаш chat id: " + id + "\nВпишите его в Render как переменную TG_CHAT, чтобы получать уведомления о записях."
					if id == os.Getenv("TG_CHAT") {
						msg = "Уведомления о записях включены."
					}
					tgSend(id, msg)
				}
			}
			if err == nil && len(res.Result) == 0 {
				time.Sleep(time.Second)
			}
		}
	}()
}
