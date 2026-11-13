package ubot

// Tahap 3b-2: sambungan paket slot ke userbot. Pencatatan pik di grup + perintah
// .on/.akses .off/.delakses .rs .undors .list .rk .ck/.cb/.ok .kk .setkk* .alert .autors .perak/.nonperak
// dan sudo (.addsudo .delsudo .sudolist). Perintah lain (.p .c .sk .sb .wd .tf .sv ... ) = tahap berikutnya.

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/gotd/td/tg"

	"cu/internal/slot"
)

var (
	rekapRe = regexp.MustCompile(`(?i)^/rekapwin(?:@\w+)?(?:[^\p{L}\d_]|$)`)
	setkkRe = regexp.MustCompile(`^(.+?)\s+(\d+)$`)
	digitRe = regexp.MustCompile(`^\d+$`)
)

// perintah yang tidak ikut antrean grup (jalan langsung)
var fastCmds = map[string]bool{"ping": true, "id": true, "addsudo": true, "delsudo": true, "sudolist": true}

const undoRSWindow = 10 * time.Minute

type resetSnap struct {
	at   time.Time
	book *slot.Book
	used map[string][]string // geseran: key -> pemakai (dipulihkan oleh .undors)
}

// isAccessCmd: ".akses" / ".on" (tidak diikuti huruf/angka).
func isAccessCmd(text string) bool {
	for _, p := range []string{".akses", ".on"} {
		if len(text) >= len(p) && strings.EqualFold(text[:len(p)], p) {
			rest := text[len(p):]
			if rest == "" {
				return true
			}
			r, _ := utf8.DecodeRuneInString(rest)
			return !(unicode.IsLetter(r) || unicode.IsDigit(r))
		}
	}
	return false
}

// parseCmd: ".List  abc def" -> ("list", "abc def").
func parseCmd(text string) (cmd, arg string, ok bool) {
	if !strings.HasPrefix(text, ".") {
		return "", "", false
	}
	f := strings.Fields(text[1:])
	if len(f) == 0 {
		return "", "", false
	}
	return strings.ToLower(f[0]), strings.Join(f[1:], " "), true
}

func (u *Userbot) onMessage(ctx context.Context, api *tg.Client, e tg.Entities, m tg.MessageClass) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[%s] panic di handler pesan: %v", u.id, r)
		}
	}()
	if svc, ok := m.(*tg.MessageService); ok {
		if _, isPin := svc.Action.(*tg.MessageActionPinMessage); isPin {
			u.dropPin(pinKeyOfPeer(svc.PeerID)) // pin baru -> cache saldo pin dibuang
		}
		return
	}
	msg, ok := m.(*tg.Message)
	if !ok {
		return
	}
	u.mu.Lock()
	meID := u.meID
	u.mu.Unlock()

	switch msg.PeerID.(type) {
	case *tg.PeerUser:
		// chat pribadi: hanya .ping / .id dari akun sendiri (perilaku tahap 3a)
		if msg.Out {
			if cmd, _, ok := parseCmd(msg.Message); ok && (cmd == "ping" || cmd == "id") {
				if peer, ok := inputPeer(e, msg.PeerID, meID); ok {
					u.simpleCmd(ctx, &replier{u: u, api: api, peer: peer, msg: msg, own: true}, cmd, meID)
				}
			}
		}
		return
	case *tg.PeerChat, *tg.PeerChannel:
	default:
		return
	}

	key := slot.NormID(chatIDText(msg.PeerID))
	from := fromUser(msg)
	isOwn := msg.Out
	isSudo := !isOwn && from != 0 && u.sudo.has(strconv.FormatInt(from, 10))
	allowed := isOwn || isSudo
	text := msg.Message

	// grup belum .akses / sudah .delakses: tidak dibaca sama sekali, kecuali .akses / .on dari pemilik atau sudo
	if !u.recording(key) && !(allowed && isAccessCmd(text)) {
		return
	}
	peer, ok := inputPeer(e, msg.PeerID, meID)
	if !ok {
		return
	}

	if cmd, arg, isCmd := parseCmd(text); isCmd && allowed && fastCmds[cmd] {
		go func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("[%s] panic di perintah .%s: %v", u.id, cmd, r)
				}
			}()
			u.fastCmd(ctx, api, e, msg, peer, cmd, arg, meID, isOwn)
		}()
		return
	}
	u.lanes.do(key, func() { u.process(ctx, api, e, msg, peer, key, meID, isOwn, allowed) })
}

// process: dijalankan berurutan per grup (urutan pik tetap benar).
func (u *Userbot) process(ctx context.Context, api *tg.Client, e tg.Entities, msg *tg.Message, peer tg.InputPeerClass, key string, meID int64, isOwn, allowed bool) {
	text := msg.Message

	// .autors on: /rekapwin dari pemilik / sudo = .rs otomatis
	if allowed && rekapRe.MatchString(text) && u.recording(key) &&
		u.bookFlag(key, func(b *slot.Book) bool { return b.AutoRS }) {
		u.resetRound(key)
		log.Printf("[%s] /rekapwin -> auto .rs (grup %s)", u.id, key)
	}

	if text != "" && u.recording(key) {
		u.recordIncoming(ctx, api, e, msg, peer, key, meID)
		// "r" (setelah .rk) dan "+N": dijalankan terpisah supaya pik berikutnya di grup ini tidak tertahan
		if rollRe.MatchString(strings.TrimSpace(text)) {
			go u.handleRollMove(ctx, api, e, msg, peer, key, meID)
		} else if m := plusRe.FindStringSubmatch(text); m != nil {
			go u.handlePlus(ctx, api, e, msg, peer, key, meID, m[1])
		}
	}

	if !strings.HasPrefix(text, ".") || !allowed {
		return
	}
	cmd, arg, ok := parseCmd(text)
	if !ok {
		return
	}
	if cmd != "akses" && cmd != "on" && !u.recording(key) {
		return
	}
	r := &replier{u: u, api: api, peer: peer, msg: msg, own: isOwn}
	u.runCmd(ctx, r, key, cmd, arg, meID, isOwn)
}

// ---------- pencatatan pik ----------

func (u *Userbot) commitPick(key, side string, amount int, name string, player *slot.Player, o slot.RecordOpts) (bool, string) {
	var ok bool
	var why string
	o.KK = u.cfg.KK
	u.slots.Edit(key, func(b *slot.Book) { ok, why = b.Record(side, amount, name, player, o) })
	return ok, why
}

func (u *Userbot) recordIncoming(ctx context.Context, api *tg.Client, e tg.Entities, msg *tg.Message, peer tg.InputPeerClass, key string, meID int64) {
	text := msg.Message
	users := userMap(e)

	// 1) pesan konfirmasi "✅ Pik: K 30 NAMA" (dari bot / alert userbot): pemain = pengirim pesan yang di-reply
	if side, amount, name, ok := slot.ParsePick(text); ok {
		rid := replyID(msg)
		var player *slot.Player
		if rid != 0 {
			if rm, more, err := u.fetchMsg(ctx, api, peer, rid); err == nil && rm != nil {
				for id, us := range more {
					users[id] = us
				}
				player = u.resolvePlayer(ctx, api, peer, rm, users, meID)
			}
		}
		o := slot.RecordOpts{MsgIDs: []int{msg.ID, rid}, PMID: rid}
		if ok, why := u.commitPick(key, side, amount, name, player, o); !ok {
			log.Printf("[%s] pik %s %d %s diabaikan (%s)", u.id, side, amount, name, why)
		}
	}

	if msg.Out {
		return
	}
	uid := fromUser(msg)
	us := users[uid]

	// 2) pik langsung "k50" (mode .perak: "k10,9")
	perak := u.bookFlag(key, func(b *slot.Book) bool { return b.Perak })
	var side string
	var amount int
	var direct bool
	if perak {
		side, amount, direct = slot.ParsePerakText(text)
	} else {
		side, amount, direct = slot.ParseDirect(text)
	}
	if direct {
		if us == nil {
			log.Printf("[%s] pik %q dilewati: pengirim %d tidak terbaca", u.id, truncate(text, 20), uid)
		} else if !us.Bot {
			nm := displayName(us)
			if nm == "" {
				nm = strconv.FormatInt(uid, 10)
			}
			player := &slot.Player{UID: strconv.FormatInt(uid, 10), AH: userAH(us)}
			o := slot.RecordOpts{Direct: true, MsgIDs: []int{msg.ID}, PMID: msg.ID}
			ok, why := u.commitPick(key, side, amount, strings.ToUpper(nm), player, o)
			switch {
			case ok:
				go u.sendAlert(ctx, api, peer, key, msg, us, side, amount, nm)
			case why == slot.RejectClosed || why == slot.RejectRK:
				u.more(ctx, api, peer, fmt.Sprintf("🔒 %s sudah CLOSE\nTidak bisa taruh %s. Buka: .ok", side, side), msg.ID)
			}
		}
	}

	// 3) "kall" / "ball": pik sebesar saldo di pin
	if aside, ok := slot.ParseAll(text); ok && us != nil && !us.Bot {
		u.pickAll(ctx, api, peer, key, msg, us, aside)
	}

	// 4) geseran: "#key b" / "#key k" = pik sebesar nominal preset, maksimal sejumlah user
	if gm := gesTrigRe.FindStringSubmatch(text); gm != nil && us != nil && !us.Bot {
		u.geseranTrigger(ctx, api, peer, key, msg, us, strings.ToLower(gm[1]), strings.ToUpper(gm[2]))
	}
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func (u *Userbot) more(ctx context.Context, api *tg.Client, peer tg.InputPeerClass, text string, replyTo int) {
	_, _ = u.send(ctx, api, peer, text, replyTo, nil)
}

func (u *Userbot) pickAll(ctx context.Context, api *tg.Client, peer tg.InputPeerClass, key string, msg *tg.Message, us *tg.User, side string) {
	nm := displayName(us)
	uid := strconv.FormatInt(us.ID, 10)
	if nm == "" {
		nm = uid
	}
	say := func(t string) { u.more(ctx, api, peer, t, msg.ID) }
	shown := slot.CleanName(strings.ToUpper(nm))
	lower := strings.ToLower(side)

	ownUID := ""
	u.slots.View(key, func(b *slot.Book) {
		if b != nil && b.Owners != nil {
			if ow := b.Owners[slot.Nick(nm)]; ow != nil {
				ownUID = ow.UID
			}
		}
	})
	if ownUID != "" && ownUID != uid {
		log.Printf("[%s] %sall ditolak: nama %s terikat ke akun lain (clone?)", u.id, side, nm)
		say(fmt.Sprintf("⚠️ Nama %s sudah dipakai akun lain, saldo pin tidak bisa dipakai. Ketik jumlahnya (mis. %s50).", shown, lower))
		return
	}
	players := u.fetchPinPlayers(ctx, api, peer, 8*time.Second)
	have, found := slot.PinHave(players, nm, uid, u.contactNames())
	switch {
	case !found:
		say(fmt.Sprintf("❓ Saldo %s tidak ketemu di pin, ketik jumlahnya (mis. %s50).", shown, lower))
	case have <= 0:
		say(fmt.Sprintf("⚠️ Saldo %s di pin 0.", shown))
	default:
		player := &slot.Player{UID: uid, AH: userAH(us)}
		o := slot.RecordOpts{Direct: true, MsgIDs: []int{msg.ID}, PMID: msg.ID}
		ok, why := u.commitPick(key, side, have, strings.ToUpper(nm), player, o)
		switch {
		case ok:
			go u.sendAlert(ctx, api, peer, key, msg, us, side, have, nm)
		case why == slot.RejectClosed || why == slot.RejectRK:
			say(fmt.Sprintf("🔒 %s sudah CLOSE\nTidak bisa taruh %s. Buka: .ok", side, side))
		}
	}
}

// sendAlert: "✅ Pik: K 22 NAMA" (nama = mention pemain) sebagai balasan pesan pik, bila .alert aktif.
func (u *Userbot) sendAlert(ctx context.Context, api *tg.Client, peer tg.InputPeerClass, key string, msg *tg.Message, us *tg.User, side string, amount int, nm string) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[%s] panic di .alert: %v", u.id, r)
		}
	}()
	if !u.bookFlag(key, func(b *slot.Book) bool { return b.Alert }) {
		return
	}
	prefix := fmt.Sprintf("✅ Pik: %s %d ", side, amount)
	name := strings.ToUpper(slot.CleanName(nm))
	if name == "" {
		return
	}
	ents := []tg.MessageEntityClass{&tg.InputMessageEntityMentionName{
		Offset: u16(prefix), Length: u16(name),
		UserID: &tg.InputUser{UserID: us.ID, AccessHash: us.AccessHash},
	}}
	if _, err := u.send(ctx, api, peer, prefix+name, msg.ID, ents); err != nil {
		_, _ = u.send(ctx, api, peer, prefix+name, msg.ID, nil) // tanpa tag
	}
}

// ---------- reset ronde ----------

// resetRound (.rs): data pik dikosongkan; snapshot disimpan 10 menit untuk .undors.
func (u *Userbot) resetRound(key string) {
	var snap, work *slot.Book
	u.slots.View(key, func(b *slot.Book) {
		snap = cloneBook(b)
		work = cloneBook(b)
	})
	used := u.ges.snapshotUsed(key)
	if (snap != nil && snap.HasPicks()) || len(used) > 0 {
		u.resetMu.Lock()
		u.lastReset[key] = &resetSnap{at: time.Now(), book: snap, used: used}
		u.resetMu.Unlock()
	}
	u.ges.resetUsed(key) // preset geseran tetap ada, hitungan terpakai mulai dari 0
	var nb *slot.Book
	if work != nil {
		nb = work.Reset(u.cfg.KK)
	}
	u.slots.Replace(key, nb)
}

func (u *Userbot) undoReset(key string) string {
	u.resetMu.Lock()
	snap := u.lastReset[key]
	u.resetMu.Unlock()
	if snap == nil {
		return "ℹ️ Tidak ada reset yang bisa dibatalkan."
	}
	forget := func() {
		u.resetMu.Lock()
		delete(u.lastReset, key)
		u.resetMu.Unlock()
	}
	if time.Since(snap.at) > undoRSWindow {
		forget()
		return "⌛ Batas 10 menit sudah lewat, reset tidak bisa dibatalkan."
	}
	var cur *slot.Book
	u.slots.View(key, func(b *slot.Book) { cur = cloneBook(b) })
	if cur.HasPicks() {
		return "⚠️ Tidak bisa dibatalkan: sudah ada pik baru di ronde ini."
	}
	if len(snap.used) > 0 {
		u.ges.restoreUsed(key, snap.used)
	}
	nb := cloneBook(snap.book)
	if nb == nil {
		forget()
		if len(snap.used) > 0 {
			return "↩️ Reset dibatalkan\nHitungan geseran dikembalikan."
		}
		return "ℹ️ Tidak ada reset yang bisa dibatalkan."
	}
	// pengaturan chat (akses, .kk, .alert, .perak, dll) memakai kondisi sekarang bila diubah sesudah .rs
	if cur != nil {
		nb.Started, nb.Off, nb.KK, nb.Alert, nb.Perak, nb.AutoRS = cur.Started, cur.Off, cur.KK, cur.Alert, cur.Perak, cur.AutoRS
		if cur.KKCfg != nil {
			nb.KKCfg = cur.KKCfg
		}
		if nb.Owners == nil {
			nb.Owners = map[string]*slot.Owner{}
		}
		for k, v := range cur.Owners {
			nb.Owners[k] = v
		}
		if nb.Marked == nil {
			nb.Marked = map[string]any{}
		}
		for k, v := range cur.Marked {
			nb.Marked[k] = v
		}
	}
	nb.ApplyKk(u.cfg.KK)
	u.slots.Replace(key, nb)
	forget()
	return "↩️ Reset dibatalkan\nList ronde sebelumnya dikembalikan. Ketik .list untuk melihat."
}

// ---------- perintah ----------

func (u *Userbot) simpleCmd(ctx context.Context, r *replier, cmd string, meID int64) {
	switch cmd {
	case "ping":
		t := time.Now()
		r.edit(ctx, "🏓 ...")
		r.edit(ctx, fmt.Sprintf("🏓 Pong! %d ms", time.Since(t).Milliseconds()))
	case "id":
		r.edit(ctx, fmt.Sprintf("ID: %d\nChat ID: %s", meID, chatIDText(r.msg.PeerID)))
	}
}

// resolveTarget: ID dari reply, angka, atau @username. "" = tidak ketemu.
func (u *Userbot) resolveTarget(ctx context.Context, api *tg.Client, peer tg.InputPeerClass, msg *tg.Message, arg string) string {
	if rid := replyID(msg); rid != 0 {
		if rm, _, err := u.fetchMsg(ctx, api, peer, rid); err == nil && rm != nil {
			if uid := fromUser(rm); uid != 0 {
				return strconv.FormatInt(uid, 10)
			}
		}
	}
	if digitRe.MatchString(arg) {
		return arg
	}
	if strings.HasPrefix(arg, "@") {
		res, err := api.ContactsResolveUsername(ctx, &tg.ContactsResolveUsernameRequest{Username: strings.TrimPrefix(arg, "@")})
		if err != nil {
			return ""
		}
		if pu, ok := res.Peer.(*tg.PeerUser); ok {
			return strconv.FormatInt(pu.UserID, 10)
		}
	}
	return ""
}

func (u *Userbot) fastCmd(ctx context.Context, api *tg.Client, e tg.Entities, msg *tg.Message, peer tg.InputPeerClass, cmd, arg string, meID int64, isOwn bool) {
	r := &replier{u: u, api: api, peer: peer, msg: msg, own: isOwn}
	switch cmd {
	case "ping", "id":
		u.simpleCmd(ctx, r, cmd, meID)
	case "addsudo", "delsudo":
		if !isOwn { // hanya pemilik akun yang boleh mengatur sudo
			return
		}
		target := u.resolveTarget(ctx, api, peer, msg, arg)
		if target == "" {
			r.edit(ctx, fmt.Sprintf("Gunakan: .%s (reply pesan user) | .%s <id> | .%s @username", cmd, cmd, cmd))
			return
		}
		if cmd == "addsudo" {
			switch {
			case target == strconv.FormatInt(meID, 10):
				r.edit(ctx, "Itu akun kamu sendiri, tidak perlu jadi sudo.")
			case !u.sudo.add(target):
				r.edit(ctx, fmt.Sprintf("ℹ️ %s sudah jadi sudo.", target))
			default:
				r.edit(ctx, fmt.Sprintf("✅ %s ditambahkan sebagai sudo.", target))
			}
			return
		}
		if !u.sudo.del(target) {
			r.edit(ctx, fmt.Sprintf("ℹ️ %s tidak ada di daftar sudo.", target))
			return
		}
		r.edit(ctx, fmt.Sprintf("🗑 %s dihapus dari sudo.", target))
	case "sudolist":
		if !isOwn {
			return
		}
		l := u.sudo.list()
		if len(l) == 0 {
			r.edit(ctx, "Belum ada sudo.")
			return
		}
		r.edit(ctx, fmt.Sprintf("👥 Sudo (%d):\n%s", len(l), strings.Join(l, "\n")))
	}
}

// sendChunks: potongan pertama menggantikan pesan perintah, sisanya dikirim sebagai pesan baru.
func (u *Userbot) sendChunks(ctx context.Context, r *replier, chunks []string) {
	for i, c := range chunks {
		if i == 0 {
			r.edit(ctx, c)
		} else {
			r.more(ctx, c, 0)
		}
	}
}

func (u *Userbot) runCmd(ctx context.Context, r *replier, key, cmd, arg string, meID int64, isOwn bool) {
	api, peer, msg := r.api, r.peer, r.msg
	switch cmd {
	case "ping", "id":
		u.simpleCmd(ctx, r, cmd, meID)

	case "rs":
		u.resetRound(key)
		t := "🔄 List dikosongkan\nKetik K atau B untuk mulai ronde baru."
		u.resetMu.Lock()
		_, has := u.lastReset[key]
		u.resetMu.Unlock()
		if has {
			t += "\n↩️ Salah reset? Ketik .undors (maks 10 menit)."
		}
		r.edit(ctx, t)

	case "undors":
		r.edit(ctx, u.undoReset(key))

	case "on", "akses":
		u.cmdAkses(ctx, r, key, meID)

	case "off", "delakses":
		u.slots.Edit(key, func(b *slot.Book) {
			b.Off = true
			b.Alert = false
			if b.KK {
				b.KK = false
				b.RemoveKk()
			}
			// ID pemain yang sudah terikat (pin / .p / .c) ikut dihapus; .akses berikutnya mulai dari nol
			b.Owners = map[string]*slot.Owner{}
			b.Marked = map[string]any{}
		})
		r.edit(ctx, "⛔ Bot nonaktif\nSemua fitur ubot di chat ini mati tanpa terkecuali (pencatatan pik, .alert, .kk, .list, .wd, .rk, geseran, semua perintah). Data pik lama tetap ada. Hanya .akses yang masih bisa dipakai untuk menghidupkan lagi.")

	case "alert":
		switch strings.ToLower(arg) {
		case "on":
			u.slots.Edit(key, func(b *slot.Book) { b.Alert = true })
			r.edit(ctx, "✅ .alert aktif. Setiap pik pemain yang tercatat dibalas \"Pik: K/B jumlah\" + tag pemain. Ketik .alert off untuk mematikan.")
		case "off":
			if !u.bookFlag(key, func(b *slot.Book) bool { return b.Alert }) {
				r.edit(ctx, "ℹ️ .alert memang belum aktif di chat ini.")
				return
			}
			u.slots.Edit(key, func(b *slot.Book) { b.Alert = false })
			r.edit(ctx, "⛔ .alert nonaktif.")
		default:
			r.edit(ctx, fmt.Sprintf(".alert saat ini: %s\nGunakan: .alert on | .alert off", onOff(u.bookFlag(key, func(b *slot.Book) bool { return b.Alert }))))
		}

	case "autors":
		switch strings.ToLower(arg) {
		case "on":
			if u.bookFlag(key, func(b *slot.Book) bool { return b.AutoRS }) {
				r.edit(ctx, "ℹ️ .autors sudah aktif di chat ini.")
				return
			}
			u.slots.Edit(key, func(b *slot.Book) { b.AutoRS = true })
			r.edit(ctx, "✅ .autors aktif. Setiap kamu ketik /rekapwin, list otomatis dikosongkan (.rs). Salah reset? Ketik .undors (maks 10 menit). Ketik .autors off untuk mematikan.")
		case "off":
			if !u.bookFlag(key, func(b *slot.Book) bool { return b.AutoRS }) {
				r.edit(ctx, "ℹ️ .autors memang belum aktif di chat ini.")
				return
			}
			u.slots.Edit(key, func(b *slot.Book) { b.AutoRS = false })
			r.edit(ctx, "⛔ .autors nonaktif. /rekapwin tidak lagi mereset list.")
		default:
			r.edit(ctx, fmt.Sprintf(".autors saat ini: %s\nGunakan: .autors on | .autors off", onOff(u.bookFlag(key, func(b *slot.Book) bool { return b.AutoRS }))))
		}

	case "perak", "nonperak":
		on := cmd == "perak"
		if u.bookFlag(key, func(b *slot.Book) bool { return b.Perak }) == on {
			if on {
				r.edit(ctx, "ℹ️ Mode .perak sudah aktif di chat ini.")
			} else {
				r.edit(ctx, "ℹ️ Mode .nonperak (satuan biasa) sudah aktif di chat ini.")
			}
			return
		}
		u.slots.Edit(key, func(b *slot.Book) { b.Perak = on })
		if on {
			r.edit(ctx, "✅ Mode .perak aktif\nk10 = 10000 · k10,9 = 10900 · k3,6 = 3600 · k1k = 1000 · k1000 = 1000 · k300 = 300\nKetik .nonperak untuk kembali ke satuan biasa.")
		} else {
			r.edit(ctx, "✅ Mode .nonperak aktif (satuan biasa)\nk10 = 10 · k300 = 300\nKetik .perak untuk input ribuan.")
		}

	case "ck", "cb":
		side := "K"
		if cmd == "cb" {
			side = "B"
		}
		u.slots.Edit(key, func(b *slot.Book) {
			if b.Closed == nil {
				b.Closed = map[string]bool{}
			}
			b.Closed[side] = true
		})
		r.edit(ctx, fmt.Sprintf("🔒 Sisi %s ditutup. Pik %s baru tidak dicatat.\nBuka lagi dengan .ok %s (atau .ok untuk semua).", side, side, strings.ToLower(side)))

	case "ok":
		which := strings.ToLower(arg)
		if which == "k" || which == "b" {
			s := strings.ToUpper(which)
			u.slots.Edit(key, func(b *slot.Book) { delete(b.Closed, s) })
			r.edit(ctx, fmt.Sprintf("🔓 Sisi %s dibuka, pik %s baru bisa masuk lagi.", s, s))
			return
		}
		u.slots.Edit(key, func(b *slot.Book) {
			b.RK = false
			b.Closed = nil
		})
		r.edit(ctx, "🔓 Semua dibuka (sisi K & B, dan aturan .rk). Pik baru bisa masuk lagi.")

	case "kk":
		u.cmdKK(ctx, r, key, arg)

	case "geseran", "gseran":
		u.cmdGeseran(ctx, r, key, arg)

	case "setkk", "setkkk", "setkb", "setkkb":
		u.cmdSetKK(ctx, r, key, cmd, arg)

	case "list":
		players := u.fetchPinPlayers(ctx, api, peer, 3*time.Second)
		fc := u.formatCtx()
		var out string
		u.slots.Edit(key, func(b *slot.Book) { out = b.FormatPicks(fc, players, nil) })
		if out == "" {
			r.edit(ctx, "K:\n-\n\nB:\n-")
			return
		}
		u.sendChunks(ctx, r, slot.ChunkLines(out, 3800))

	case "rk":
		var out, list string
		var kt, bt int
		players := u.fetchPinPlayers(ctx, api, peer, 0)
		fc := u.formatCtx()
		u.slots.Edit(key, func(b *slot.Book) {
			out = b.FormatRk(u.cfg.KK)
			if out == "" {
				return
			}
			b.RK = true // sisi yang lebih besar (K atau B) tidak lagi menerima pik baru
			list = b.FormatPicks(fc, players, nil)
			kt, bt = sumPicks(b.K), sumPicks(b.B)
		})
		if out == "" {
			r.edit(ctx, "Belum ada pik yang tercatat di chat ini.")
			return
		}
		// urutan: list pik pemain dulu (pesan .rk diubah jadi list), lalu rekap, lalu ajakan
		chunks := slot.ChunkLines(list, 3800)
		if list == "" || len(chunks) == 0 {
			r.edit(ctx, out)
		} else {
			u.sendChunks(ctx, r, chunks)
		}
		rekapID := 0
		if len(chunks) > 0 {
			rekapID = r.more(ctx, out, 0)
		}
		if kt != bt {
			need, diff := "B", kt-bt
			if bt > kt {
				need, diff = "K", bt-kt
			}
			to := rekapID
			if to == 0 {
				to = msg.ID
			}
			r.more(ctx, fmt.Sprintf("%s -%d ALL // ECER", need, diff), to)
		}
	}
}

func onOff(v bool) string {
	if v {
		return "ON"
	}
	return "OFF"
}

func (u *Userbot) cmdKK(ctx context.Context, r *replier, key, arg string) {
	cfg := u.cfg.KK
	u.slots.View(key, func(b *slot.Book) {
		if b != nil && b.KKCfg != nil {
			if b.KKCfg.K != nil {
				cfg.K = b.KKCfg.K
			}
			if b.KKCfg.B != nil {
				cfg.B = b.KKCfg.B
			}
		}
	})
	line := func(side string, s *slot.KKSide) string {
		if s == nil {
			return side + ": -"
		}
		return strings.TrimRight(fmt.Sprintf("%s: %s %d %s", side, s.Name, s.Amount, u.cfg.ListSuffix), " ")
	}
	info := line("K", cfg.K) + "\n" + line("B", cfg.B)
	switch strings.ToLower(arg) {
	case "on":
		u.slots.Edit(key, func(b *slot.Book) {
			b.KK = true
			b.ApplyKk(u.cfg.KK)
		})
		r.edit(ctx, "✅ .kk aktif. Pemain tetap masuk .list & .rk (tetap ada setelah .rs, hilang dengan .kk off):\n"+info)
	case "off":
		if !u.bookFlag(key, func(b *slot.Book) bool { return b.KK }) {
			r.edit(ctx, "ℹ️ .kk memang belum aktif di chat ini.")
			return
		}
		u.slots.Edit(key, func(b *slot.Book) {
			b.KK = false
			b.RemoveKk()
		})
		r.edit(ctx, "⛔ .kk nonaktif. Pemain tetap dihapus dari .list.")
	default:
		r.edit(ctx, fmt.Sprintf(".kk saat ini: %s\nGunakan: .kk on | .kk off", onOff(u.bookFlag(key, func(b *slot.Book) bool { return b.KK }))))
	}
}

func (u *Userbot) cmdSetKK(ctx context.Context, r *replier, key, cmd, arg string) {
	side := "B"
	if cmd == "setkk" || cmd == "setkkk" {
		side = "K"
	}
	m := setkkRe.FindStringSubmatch(strings.TrimSpace(arg))
	if m == nil {
		def := u.cfg.KK.B
		if side == "K" {
			def = u.cfg.KK.K
		}
		ex := ""
		if def != nil {
			ex = fmt.Sprintf(" (mis. .%s %s %d)", cmd, def.Name, def.Amount)
		}
		r.edit(ctx, fmt.Sprintf("Gunakan: .%s NAMA JUMLAH%s", cmd, ex))
		return
	}
	name := slot.CleanName(strings.ToUpper(strings.TrimSpace(m[1])))
	amount, _ := strconv.Atoi(m[2])
	if amount <= 0 {
		r.edit(ctx, "⚠️ Jumlah harus lebih dari 0.")
		return
	}
	kkOn := false
	u.slots.Edit(key, func(b *slot.Book) {
		if b.KKCfg == nil {
			b.KKCfg = &slot.KKCfg{}
		}
		ks := &slot.KKSide{Name: name, Amount: amount}
		if side == "K" {
			b.KKCfg.K = ks
		} else {
			b.KKCfg.B = ks
		}
		kkOn = b.KK
		if b.KK {
			// .kk sedang aktif: ganti pemain tetap di sisi ini langsung
			list := b.Side(side)
			keep := make([]*slot.Pick, 0, len(*list))
			for _, p := range *list {
				if p.KK {
					delete(b.Over, side+":"+slot.NormName(p.Name))
				} else {
					keep = append(keep, p)
				}
			}
			*list = keep
			b.ApplyKk(u.cfg.KK)
		}
	})
	t := strings.TrimSpace(fmt.Sprintf("✅ Pemain tetap .kk sisi %s: %s %d %s", side, name, amount, u.cfg.ListSuffix))
	if !kkOn {
		t += "\nℹ️ .kk belum aktif, ketik .kk on."
	}
	r.edit(ctx, t)
}

// cmdAkses (.on / .akses): hidupkan pencatatan + .alert + .kk, ikat ID pemain di pin ke akunnya (anti-clone).
func (u *Userbot) cmdAkses(ctx context.Context, r *replier, key string, meID int64) {
	markedCount := 0
	u.slots.Edit(key, func(b *slot.Book) {
		b.Started = true
		b.Off = false
		b.Alert = true
		b.KK = true
		b.ApplyKk(u.cfg.KK)
		if b.Owners == nil {
			b.Owners = map[string]*slot.Owner{}
		}
		// pemain yang pernah diberi .p / .c: ID akunnya diikat dulu (menang atas pencocokan nama pin)
		for nk, v := range b.Marked {
			mm, ok := v.(map[string]any)
			if !ok {
				continue
			}
			uid := anyStr(mm["uid"])
			if uid == "" {
				continue
			}
			b.Owners[nk] = &slot.Owner{UID: uid, AH: anyStr(mm["ah"])}
			markedCount++
		}
	})
	bindInfo := ""
	if markedCount > 0 {
		bindInfo += fmt.Sprintf("\n🔐 ID pemain .p/.c diikat: %d", markedCount)
	}
	onText := "✅ Bot aktif\nSemua fitur ubot di chat ini hidup: pencatatan pik, .alert, .kk, .list, .wd, .rk, geseran, dll. Ketik .delakses untuk mematikan semuanya."

	// pengikatan ID pemain di pin diselesaikan DULU sebelum membalas, supaya pik yang masuk
	// tepat setelah .akses sudah bisa dicocokkan dengan ID pemilik namanya
	if pinMap := u.fetchPinPlayers(ctx, r.api, r.peer, 0); pinMap != nil {
		parts, err := u.participants(ctx, r.api, r.peer)
		if err != nil {
			log.Printf("[%s] gagal ikat ID pemain pin: %v", u.id, err)
		} else {
			byNick := map[string][]slot.Owner{}
			for _, us := range parts {
				if us == nil || us.Bot || us.ID == meID {
					continue
				}
				nk := slot.Nick(displayName(us))
				if nk == "" {
					continue
				}
				byNick[nk] = append(byNick[nk], slot.Owner{UID: strconv.FormatInt(us.ID, 10), AH: userAH(us)})
			}
			bound := 0
			var dup []string
			u.slots.Edit(key, func(b *slot.Book) {
				if b.Owners == nil {
					b.Owners = map[string]*slot.Owner{}
				}
				for nk := range pinMap {
					list := byNick[nk]
					ow := b.Owners[nk]
					same := -1
					if ow != nil {
						for i, x := range list {
							if x.UID == ow.UID {
								same = i
								break
							}
						}
					}
					switch {
					case ow != nil && same >= 0:
						if list[same].AH != "" {
							ow.AH = list[same].AH
						}
					case ow == nil && len(list) == 1:
						b.Owners[nk] = &slot.Owner{UID: list[0].UID, AH: list[0].AH}
						bound++
					case ow == nil && len(list) > 1:
						dup = append(dup, nk)
					}
				}
			})
			bindInfo += fmt.Sprintf("\n🔐 ID pemain di pin tersimpan: %d", bound)
			if len(dup) > 0 {
				sort.Strings(dup)
				bindInfo += fmt.Sprintf("\n⚠️ Nama kembar di grup, belum bisa diikat: %s (pemilik = yang pik duluan)", strings.Join(dup, ", "))
			}
		}
	}
	r.edit(ctx, onText+bindInfo)
}
