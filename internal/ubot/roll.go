package ubot

// Tahap 3b-3: "r" (roll request setelah .rk) dan "+N" (tambah pik pemain yang sudah ada di list).
// Keduanya membuat kartu Konfirm / Tolak lewat mode inline bot ("via @bot"); bot.go menerima klik
// tombol dan memanggil Resolve milik entri di paket rolls. Logika mengikuti ubot.js
// (handleRollMove / applyRoll / handlePlus).

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math/rand/v2"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gotd/td/tg"

	"cu/internal/config"
	"cu/internal/rolls"
	"cu/internal/slot"
)

var (
	rollRe = regexp.MustCompile(`(?i)^(r|rol|roll)[.!]*$`)
	plusRe = regexp.MustCompile(`^\s*\+\s*(\d+)\s*$`)
)

// rollState: status sementara (di memori) untuk "r" dan "+N".
type rollState struct {
	mu      sync.Mutex
	pending map[string]string // "r:<chat>:<uid>" / "p:<chat>:<uid>" -> token permintaan yang menunggu admin
	busy    map[string]bool   // sedang diproses (cegah "r" / "+N" ganda bersamaan)
	bot     *tg.InputUser
	botName string
}

func (s *rollState) getPending(k string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pending[k]
}

func (s *rollState) setPending(k, token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pending == nil {
		s.pending = map[string]string{}
	}
	s.pending[k] = token
}

func (s *rollState) delPending(k string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.pending, k)
}

func (s *rollState) delPendingIf(k, token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pending[k] == token {
		delete(s.pending, k)
	}
}

func (s *rollState) tryBusy(k string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.busy == nil {
		s.busy = map[string]bool{}
	}
	if s.busy[k] {
		return false
	}
	s.busy[k] = true
	return true
}

func (s *rollState) doneBusy(k string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.busy, k)
}

// isAdmin: akun userbot ini, pemilik bot (OWNER_ID), atau sudo.
func (u *Userbot) isAdmin(fromID string) bool {
	if fromID == "" {
		return false
	}
	u.mu.Lock()
	me := u.meID
	u.mu.Unlock()
	if fromID == u.id || (me != 0 && fromID == strconv.FormatInt(me, 10)) {
		return true
	}
	if o := strings.TrimSpace(config.String("OWNER_ID", "")); o != "" && fromID == o {
		return true
	}
	return u.sudo.has(fromID)
}

func (u *Userbot) ownerIDs() []string {
	ids := []string{u.id}
	u.mu.Lock()
	me := u.meID
	u.mu.Unlock()
	if me != 0 {
		ids = append(ids, strconv.FormatInt(me, 10))
	}
	return ids
}

// senderName: nama pemain huruf besar (nama lengkap, lalu username, lalu ID).
func senderName(us *tg.User, uid string) string {
	if us != nil {
		if n := strings.ToUpper(displayName(us)); n != "" {
			return n
		}
	}
	return uid
}

// pinSaldo membaca saldo pemain di pin (segar, maks ~8 dtk). readable=false bila pin tidak terbaca.
func (u *Userbot) pinSaldo(ctx context.Context, api *tg.Client, peer tg.InputPeerClass, name, uid string) (have int, found, readable bool) {
	cctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	players := u.fetchPinPlayers(cctx, api, peer, 0)
	if players == nil {
		return 0, false, false
	}
	have, found = slot.PinHave(players, name, uid, u.contactNames())
	return have, found, true
}

// sendList: kirim .list terbaru sebagai pesan baru (setelah roll / +N dikonfirmasi).
func (u *Userbot) sendList(ctx context.Context, api *tg.Client, peer tg.InputPeerClass, key string) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[%s] panic saat kirim list: %v", u.id, r)
		}
	}()
	players := u.fetchPinPlayers(ctx, api, peer, 3*time.Second)
	fc := u.formatCtx()
	var out string
	u.slots.Edit(key, func(b *slot.Book) { out = b.FormatPicks(fc, players, nil) })
	chunks := []string{"K:\n-\n\nB:\n-"}
	if out != "" {
		chunks = slot.ChunkLines(out, 3800)
	}
	for _, c := range chunks {
		u.more(ctx, api, peer, c, 0)
	}
}

// ---------- kartu inline ----------

func (u *Userbot) inlineBot(ctx context.Context, api *tg.Client) (*tg.InputUser, error) {
	name := rolls.BotUsername()
	if name == "" {
		return nil, errors.New("username bot belum diketahui")
	}
	u.rs.mu.Lock()
	if u.rs.bot != nil && u.rs.botName == name {
		b := u.rs.bot
		u.rs.mu.Unlock()
		return b, nil
	}
	u.rs.mu.Unlock()
	res, err := api.ContactsResolveUsername(ctx, &tg.ContactsResolveUsernameRequest{Username: strings.TrimPrefix(name, "@")})
	if err != nil {
		return nil, err
	}
	pu, ok := res.Peer.(*tg.PeerUser)
	if !ok {
		return nil, errors.New("username bot bukan akun bot")
	}
	for _, x := range res.Users {
		if us, ok := x.(*tg.User); ok && us.ID == pu.UserID {
			in := &tg.InputUser{UserID: us.ID, AccessHash: us.AccessHash}
			u.rs.mu.Lock()
			u.rs.bot, u.rs.botName = in, name
			u.rs.mu.Unlock()
			return in, nil
		}
	}
	return nil, errors.New("akun bot tidak ditemukan")
}

// sendInlineRequest mengirim kartu "via @bot" (+ tombol Konfirm / Tolak) tanpa me-reply pesan pemain.
// Mengembalikan ID pesan kartu (dipakai untuk menandai kedaluwarsa).
func (u *Userbot) sendInlineRequest(ctx context.Context, api *tg.Client, peer tg.InputPeerClass, token string) (int, error) {
	bot, err := u.inlineBot(ctx, api)
	if err != nil {
		return 0, err
	}
	rolls.SetLastReject("")
	res, err := api.MessagesGetInlineBotResults(ctx, &tg.MessagesGetInlineBotResultsRequest{
		Bot: bot, Peer: peer, Query: "roll:" + token,
	})
	if err != nil {
		return 0, err
	}
	if res == nil || len(res.Results) == 0 {
		d := "bot tidak menerima inline_query ini (cek: ada instance bot lain dengan token sama, atau /setinline mati)"
		if lr := rolls.LastReject(); lr != "" {
			d = "bot menolak: " + lr
		}
		return 0, errors.New("bot tidak mengembalikan hasil inline. " + d)
	}
	upd, err := api.MessagesSendInlineBotResult(ctx, &tg.MessagesSendInlineBotResultRequest{
		Peer: peer, RandomID: rand.Int64(), QueryID: res.QueryID, ID: res.Results[0].GetID(),
	})
	if err != nil {
		return 0, err
	}
	return sentMsgID(upd), nil
}

// mentionPos: posisi (UTF-16) nama pemain setelah "👤" di teks kartu, untuk mention biru di pesan inline.
func mentionPos(text, nm, uid string) *rolls.Mention {
	from := strings.Index(text, "👤")
	if from < 0 {
		return nil
	}
	i := strings.Index(text[from:], nm)
	if i < 0 {
		return nil
	}
	return &rolls.Mention{UserID: uid, Offset: u16(text[:from+i]), Length: u16(nm)}
}

func ttlOf(env string, def int) time.Duration {
	n := config.Int(env, def)
	if n < 5 {
		n = 5
	}
	return time.Duration(n) * time.Second
}

// failHint: petunjuk bila kartu inline gagal dibuat.
func failHint(why string, roll bool) string {
	switch {
	case strings.Contains(why, "BOT_INLINE_DISABLED"):
		return "Mode inline bot belum aktif (BotFather → /setinline)."
	case roll && strings.Contains(why, "BOT_RESPONSE_TIMEOUT"):
		return "Bot tidak menjawab inline query. Cek log bot: ada tulisan 'inline_query masuk'? Kalau tidak ada, kemungkinan ada instance bot lain yang memakai token yang sama."
	case roll && strings.Contains(why, "tidak mengembalikan hasil"):
		return "Bot menolak inline query (akun userbot beda dengan pemilik, atau token sudah kedaluwarsa). Cek log bot."
	}
	return ""
}

// sendCard mengirim kartu dan memasang kedaluwarsa otomatis. false = gagal (entri sudah dibersihkan).
func (u *Userbot) sendCard(ctx context.Context, api *tg.Client, peer tg.InputPeerClass, ent *rolls.Entry, token, pkey, nm string, ttl time.Duration, expiredText string, roll bool) bool {
	cardID, err := u.sendInlineRequest(ctx, api, peer, token)
	if err != nil {
		log.Printf("[%s] gagal kirim kartu konfirmasi: %v", u.id, err)
		rolls.Remove(token)
		u.rs.delPending(pkey)
		why := err.Error()
		msg := "⚠️ Tombol konfirmasi gagal dibuat.\nPenyebab: " + why
		if h := failHint(why, roll); h != "" {
			msg += "\n" + h
		}
		u.more(ctx, api, peer, msg, 0) // pemberitahuan biasa (bukan reply ke pemain)
		return false
	}
	// kedaluwarsa otomatis: permintaan yang belum diproses admin dibatalkan, tombol tidak berlaku lagi
	time.AfterFunc(ttl, func() {
		if !ent.Expire() {
			return // sudah dikonfirmasi / ditolak
		}
		u.rs.delPendingIf(pkey, token)
		log.Printf("[%s] permintaan %s kedaluwarsa", u.id, nm)
		if cardID != 0 {
			u.edit(ctx, api, peer, cardID, fmt.Sprintf(expiredText, int(ttl.Seconds()), nm))
		}
	})
	return true
}

// ---------- "r" ----------

func (u *Userbot) handleRollMove(ctx context.Context, api *tg.Client, e tg.Entities, msg *tg.Message, peer tg.InputPeerClass, key string, meID int64) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[%s] panic di roll: %v", u.id, r)
			u.more(ctx, api, peer, "⚠️ Roll gagal diproses, coba ketik r sekali lagi.", 0)
		}
	}()
	uidN := fromUser(msg)
	if uidN == 0 && msg.Out {
		uidN = meID
	}
	if uidN == 0 {
		return
	}
	uid := strconv.FormatInt(uidN, 10)
	bk := "r:" + key + ":" + uid
	if !u.rs.tryBusy(bk) {
		return
	}
	defer u.rs.doneBusy(bk)

	// 1) keadaan buku sekarang
	var (
		rk              bool
		kTot, bTot      int
		closedTo        bool
		onToFound       bool
		onToAmt         int
		mineFound       bool
		mineAmt         int
		ownedByOther    bool
		to, from        string
		diff            int
		nm              string
		us              = userMap(e)[uidN]
		reachedOwnerChk bool
	)
	if us != nil && us.Bot {
		return
	}
	nm = slot.CleanName(senderName(us, uid))
	u.slots.View(key, func(b *slot.Book) {
		if b == nil {
			return
		}
		rk = b.RK
		kTot, bTot = sumPicks(b.K), sumPicks(b.B)
		if kTot < bTot {
			to, from = "K", "B"
		} else {
			to, from = "B", "K"
		}
		closedTo = b.Closed != nil && b.Closed[to]
		if kTot > bTot {
			diff = kTot - bTot
		} else {
			diff = bTot - kTot
		}
		reachedOwnerChk = true
		if ow := b.Owners[slot.Nick(nm)]; ow != nil && ow.UID != uid {
			ownedByOther = true
		}
		for _, p := range *b.Side(to) {
			if p.UID == uid {
				onToFound, onToAmt = true, p.Amount
				break
			}
		}
		for _, p := range *b.Side(from) {
			if p.UID == uid {
				mineFound, mineAmt = true, p.Amount
				break
			}
		}
	})
	if !reachedOwnerChk || !rk || kTot == bTot || closedTo {
		return
	}
	// Anti-clone: nama ini terikat ke akun lain -> "r" ditolak (saldo pin bukan milik akun ini)
	if ownedByOther {
		log.Printf("[%s] Roll %s ditolak: nama terikat ke akun lain (clone?)", u.id, nm)
		return
	}

	// sudah ada permintaan yang menunggu dari pemain ini -> abaikan
	pkey := "r:" + key + ":" + uid
	if old := u.rs.getPending(pkey); old != "" {
		if r := rolls.Get(old); r != nil && !r.IsDone() && time.Since(r.CreatedAt) < 10*time.Minute {
			return
		}
		u.rs.delPending(pkey)
	}

	// jumlah yang akan diambil
	amount := diff
	removeMode := false // pemain di sisi berlebih: pik-nya dikurangi sebesar selisih supaya K = B
	newAmt, extra := 0, 0
	if !onToFound && mineFound {
		removeMode = true
		amount = diff
		if mineAmt < amount {
			amount = mineAmt
		}
		newAmt = mineAmt - amount
		extra = diff - amount // >0: pik pemain habis tapi belum seimbang -> sisanya masuk sisi kurang
	}
	need := amount
	switch {
	case onToFound:
		need = onToAmt + amount
	case removeMode:
		need = extra
	}

	// cek saldo dari pin
	var saldoLine string
	switch {
	case removeMode && extra == 0:
		saldoLine = fmt.Sprintf("ℹ️ Pik %s dikurangi, tidak perlu cek saldo", from)
	case u.svownSet()[uid]:
		saldoLine = "✅ Saldo bypass (.svown) — tidak perlu pin"
	default:
		have, found, readable := u.pinSaldo(ctx, api, peer, nm, uid)
		switch {
		case !readable:
			saldoLine = "❓ Pin tidak terbaca — saldo tidak bisa dicek"
		case !found:
			saldoLine = fmt.Sprintf("❓ Saldo tidak ada — %s tidak di pinned", nm)
		case have >= need:
			saldoLine = fmt.Sprintf("💰 Saldo %d — cukup", have)
		default:
			saldoLine = fmt.Sprintf("⚠️ Saldo %d — kurang dari %d", have, need)
		}
	}

	var head string
	switch {
	case removeMode && extra > 0:
		head = fmt.Sprintf("👤 %s mau keluar dari %s (%d) dan masuk %s %d supaya K dan B sama\n", nm, from, amount, to, extra)
	case removeMode:
		after := "keluar dari list"
		if newAmt > 0 {
			after = fmt.Sprintf("jadi %d", newAmt)
		}
		head = fmt.Sprintf("👤 %s mau kurangi pik %s %d (%s) supaya K dan B sama\n", nm, from, amount, after)
	case onToFound:
		head = fmt.Sprintf("👤 %s mau tambah %s %d (jadi %d)\n", nm, to, amount, need)
	default:
		head = fmt.Sprintf("👤 %s mau roll %s %d\n", nm, to, amount)
	}
	text := "🎲 Roll Request\n" + head + saldoLine + "\nAdmin: konfirmasi atau tolak di bawah"

	ah := userAH(us)
	var ent *rolls.Entry
	ent = &rolls.Entry{
		UserID:   u.id,
		OwnerIDs: u.ownerIDs(),
		Text:     text,
		Mention:  mentionPos(text, nm, uid),
		Resolve: func(action, fromID string) rolls.Result {
			if !u.isAdmin(fromID) {
				return rolls.Result{Alert: "Hanya admin yang bisa memproses permintaan ini."}
			}
			if ent.IsExpired() {
				return rolls.Result{Alert: "Permintaan sudah kedaluwarsa.", Text: "⌛ Roll kedaluwarsa\n👤 " + nm}
			}
			if !ent.Claim() {
				return rolls.Result{Alert: "Permintaan ini sudah diproses."}
			}
			u.rs.delPending(pkey)
			if action != "ok" {
				return rolls.Result{Alert: "Ditolak.", Text: "❌ Roll ditolak\n👤 " + nm}
			}
			result := u.applyRoll(key, msg.ID, uid, nm, ah)
			// roll berhasil diterapkan -> tampilkan .list terbaru
			if !hasAnyPrefix(result, "ℹ️", "🔒", "🔓", "⚠️") {
				go u.sendList(ctx, api, peer, key)
			}
			return rolls.Result{Alert: "Dikonfirmasi.", Text: "✅ Roll dikonfirmasi\n👤 " + nm + "\n" + result}
		},
	}
	token := rolls.Create(ent)
	u.rs.setPending(pkey, token)

	// Pemain yang sudah di sisi kurang menambah pik: umumkan dulu (nama di-mention, bukan reply ke pemain)
	if onToFound {
		prefix := "🎲 "
		notice := fmt.Sprintf("%s%s menambah %s %d → %d (+%d), tunggu konfirmasi admin.", prefix, nm, to, onToAmt, need, amount)
		u.sendMention(ctx, api, peer, notice, prefix, nm, us)
	}

	u.sendCard(ctx, api, peer, ent, token, pkey, nm, ttlOf("ROLL_TTL_SEC", 15), "⌛ Roll kedaluwarsa (%d detik)\n👤 %s", true)
}

// sendMention mengirim notice dengan nama pemain (setelah prefix) di-mention; gagal -> tanpa tag.
func (u *Userbot) sendMention(ctx context.Context, api *tg.Client, peer tg.InputPeerClass, text, prefix, nm string, us *tg.User) {
	var ents []tg.MessageEntityClass
	if us != nil {
		ents = []tg.MessageEntityClass{&tg.InputMessageEntityMentionName{
			Offset: u16(prefix), Length: u16(nm),
			UserID: &tg.InputUser{UserID: us.ID, AccessHash: us.AccessHash},
		}}
	}
	if _, err := u.send(ctx, api, peer, text, 0, ents); err != nil && len(ents) > 0 {
		_, _ = u.send(ctx, api, peer, text, 0, nil) // cadangan: tanpa tag
	}
}

func hasAnyPrefix(s string, ps ...string) bool {
	for _, p := range ps {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// applyRoll: terapkan roll yang sudah dikonfirmasi (keadaan buku dibaca ulang saat konfirmasi).
func (u *Userbot) applyRoll(key string, msgID int, uid, nm, ah string) string {
	var out string
	u.slots.Edit(key, func(b *slot.Book) { out = u.applyRollTo(b, msgID, uid, nm, ah) })
	return out
}

// Aturan "r" setelah .rk (selisih = |K - B|):
//   - pemain di sisi yang kurang: pik-nya DITAMBAH sebesar selisih
//   - pemain di sisi yang berlebih: pik-nya DIKURANGI sebesar selisih (habis = keluar dari list);
//     kalau pik-nya kurang dari selisih, sisanya masuk sisi yang kurang sebagai satu pik
//   - pemain belum ada di list: masuk sisi yang kurang sebesar selisih
func (u *Userbot) applyRollTo(b *slot.Book, msgID int, uid, nm, ah string) string {
	if !b.RK {
		return "ℹ️ Aturan .rk tidak aktif lagi."
	}
	k, bb := sumPicks(b.K), sumPicks(b.B)
	if k == bb {
		return "ℹ️ K dan B sudah sama, tidak ada yang diambil."
	}
	to, from := "B", "K" // sisi yang kurang
	if k < bb {
		to, from = "K", "B"
	}
	if b.Closed != nil && b.Closed[to] {
		return fmt.Sprintf("🔒 Sisi %s sudah ditutup.", to)
	}
	diff := k - bb
	if diff < 0 {
		diff = -diff
	}
	// pemain sudah di sisi yang kurang: tambah jumlahnya sebesar kekurangan sampai seimbang
	for _, p := range *b.Side(to) {
		if p.UID == uid {
			p.Amount += diff
			log.Printf("[%s] r: %s tambah %s %d (jadi %d)", u.id, nm, to, diff, p.Amount)
			return fmt.Sprintf("%s ditambah %s %d (jadi %d)", p.Name, to, diff, p.Amount)
		}
	}
	var src *rolledPick
	fromList := b.Side(from)
	for i, p := range *fromList {
		if p.UID == uid {
			src = &rolledPick{p: p, idx: i}
			break
		}
	}
	// pemain belum ada di list: ambil seluruh kekurangan di sisi yang kurang
	if src == nil {
		ok, _ := b.Record(to, diff, nm, &slot.Player{UID: uid, AH: ah},
			slot.RecordOpts{Direct: true, MsgIDs: []int{msgID}, PMID: msgID, KK: u.cfg.KK})
		if !ok {
			return "⚠️ Gagal mencatat pik."
		}
		log.Printf("[%s] r: %s ambil %s %d", u.id, nm, to, diff)
		return fmt.Sprintf("%s masuk %s %d", nm, to, diff)
	}
	// pemain di sisi yang berlebih: kurangi pik-nya sebesar selisih (tidak pindah sisi)
	p := src.p
	cut := diff
	if p.Amount < cut {
		cut = p.Amount
	}
	if cut <= 0 {
		return "ℹ️ Tidak ada yang perlu dikurangi."
	}
	p.Amount -= cut
	if p.Amount <= 0 {
		keep := make([]*slot.Pick, 0, len(*fromList))
		for _, x := range *fromList {
			if x != p {
				keep = append(keep, x)
			}
		}
		*fromList = keep
		delete(b.Over, from+":"+slot.NormName(p.Name))
	}
	// pik pemain habis tapi belum seimbang: sisa selisih masuk ke sisi kurang atas nama pemain yang sama
	extra := diff - cut
	if extra > 0 {
		ids := p.IDs
		if len(ids) > 20 {
			ids = ids[len(ids)-20:]
		}
		entry := &slot.Pick{Name: p.Name, Amount: extra, UID: p.UID, AH: p.AH, PMID: p.PMID, IDs: append([]int{}, ids...)}
		toList := b.Side(to)
		*toList = append(*toList, entry)
	}
	log.Printf("[%s] r: %s kurangi %s %d (sisa ke %s: %d)", u.id, p.Name, from, cut, to, extra)
	if extra > 0 {
		return fmt.Sprintf("%s keluar dari %s (%d) dan masuk %s %d", p.Name, from, cut, to, extra)
	}
	if p.Amount > 0 {
		return fmt.Sprintf("%s dikurangi %s %d (jadi %d)", p.Name, from, cut, p.Amount)
	}
	return fmt.Sprintf("%s keluar dari %s (%d)", p.Name, from, cut)
}

type rolledPick struct {
	p   *slot.Pick
	idx int
}

// ---------- "+N" ----------

func (u *Userbot) handlePlus(ctx context.Context, api *tg.Client, e tg.Entities, msg *tg.Message, peer tg.InputPeerClass, key string, meID int64, numStr string) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[%s] panic di +N: %v", u.id, r)
		}
	}()
	uidN := fromUser(msg)
	if uidN == 0 && msg.Out {
		uidN = meID
	}
	if uidN == 0 {
		return
	}
	uid := strconv.FormatInt(uidN, 10)
	bk := "p:" + key + ":" + uid
	if !u.rs.tryBusy(bk) {
		return
	}
	defer u.rs.doneBusy(bk)

	// mode .perak: "+2" = 2000, sama seperti "k2"
	perak := u.bookFlag(key, func(b *slot.Book) bool { return b.Perak })
	var amount int
	if perak {
		amount = slot.ParsePerak(numStr, "")
	} else {
		amount, _ = strconv.Atoi(numStr)
	}
	if amount <= 0 {
		return
	}

	// hanya pemain yang SUDAH ada di list; yang belum ada diabaikan diam-diam
	var (
		found, both  bool
		side         string
		pName        string
		pAmt         int
		pKK          bool
		pUID         string
		ownedByOther bool
		closed       bool
	)
	u.slots.View(key, func(b *slot.Book) {
		if b == nil {
			return
		}
		n := 0
		for _, sd := range []string{"K", "B"} {
			for _, p := range *b.Side(sd) {
				if p.UID == uid {
					n++
					if n == 1 {
						side, pName, pAmt, pKK, pUID = sd, p.Name, p.Amount, p.KK, p.UID
					}
				}
			}
		}
		found, both = n > 0, n > 1
		if !found || both {
			return
		}
		closed = b.Closed != nil && b.Closed[side]
		if ow := b.Owners[slot.Nick(pName)]; ow != nil && ow.UID != uid {
			ownedByOther = true
		}
	})
	if !found {
		return
	}
	if both {
		log.Printf("[%s] +%d diabaikan: %s ada di dua sisi", u.id, amount, pName)
		return
	}
	if closed {
		return
	}
	us := userMap(e)[uidN]
	if us != nil && us.Bot {
		return
	}
	nm := slot.CleanName(strings.ToUpper(pName))
	// Anti-clone: nama terikat ke akun lain -> ditolak
	if ownedByOther {
		log.Printf("[%s] +%d %s ditolak: nama terikat ke akun lain (clone?)", u.id, amount, nm)
		return
	}
	pkey := "p:" + key + ":" + uid
	if old := u.rs.getPending(pkey); old != "" {
		if r := rolls.Get(old); r != nil && !r.IsDone() && time.Since(r.CreatedAt) < 10*time.Minute {
			return // masih menunggu admin
		}
		u.rs.delPending(pkey)
	}
	need := pAmt + amount

	// cek saldo pin (pik lama + tambahan)
	short := false
	var saldoLine string
	switch {
	case pKK:
		saldoLine = "✅ Pemain tetap (.kk) — selalu P"
	case u.svownSet()[uid]:
		saldoLine = "✅ Saldo bypass (.svown) — tidak perlu pin"
	default:
		short = true
		pu := pUID
		if pu == "" {
			pu = uid
		}
		have, hasPin, readable := u.pinSaldo(ctx, api, peer, pName, pu)
		switch {
		case !readable:
			saldoLine = "❓ Pin tidak terbaca — saldo tidak bisa dicek, diberi tanda ."
		case !hasPin:
			saldoLine = fmt.Sprintf("❓ %s tidak ada di pinned — diberi tanda .", nm)
		case have >= need:
			short = false
			saldoLine = fmt.Sprintf("💰 Saldo %d — cukup", have)
		default:
			saldoLine = fmt.Sprintf("⚠️ Saldo %d — kurang dari %d, diberi tanda .", have, need)
		}
	}

	text := "➕ Tambah Pik\n" +
		fmt.Sprintf("👤 %s mau tambah %s %d (+%d, jadi %d)\n", nm, side, pAmt, amount, need) +
		saldoLine + "\n" +
		"Admin: konfirmasi atau tolak di bawah"

	var ent *rolls.Entry
	ent = &rolls.Entry{
		UserID:   u.id,
		OwnerIDs: u.ownerIDs(),
		Text:     text,
		Mention:  mentionPos(text, nm, uid),
		Resolve: func(action, fromID string) rolls.Result {
			if !u.isAdmin(fromID) {
				return rolls.Result{Alert: "Hanya admin yang bisa memproses permintaan ini."}
			}
			if ent.IsExpired() {
				return rolls.Result{Alert: "Permintaan sudah kedaluwarsa.", Text: "⌛ Tambah pik kedaluwarsa\n👤 " + nm}
			}
			if !ent.Claim() {
				return rolls.Result{Alert: "Permintaan ini sudah diproses."}
			}
			u.rs.delPending(pkey)
			if action != "ok" {
				return rolls.Result{Alert: "Ditolak.", Text: "❌ Tambah pik ditolak\n👤 " + nm}
			}
			var res rolls.Result
			applied := false
			u.slots.Edit(key, func(b *slot.Book) {
				var cur *slot.Pick
				for _, x := range *b.Side(side) {
					if x.UID == uid {
						cur = x
						break
					}
				}
				if cur == nil {
					res = rolls.Result{Alert: "Pemain sudah tidak ada di list.", Text: fmt.Sprintf("ℹ️ %s sudah tidak ada di list, tidak ditambah.", nm)}
					return
				}
				if b.Closed != nil && b.Closed[side] {
					res = rolls.Result{Alert: "Sisi ditutup.", Text: fmt.Sprintf("🔒 Sisi %s sudah ditutup.\n👤 %s", side, nm)}
					return
				}
				before := cur.Amount
				cur.Amount = before + amount
				// saldo kurang: buang penimpa .p lama supaya .list otomatis memberi tanda . (bukan P)
				if short {
					delete(b.Over, side+":"+slot.NormName(cur.Name))
				}
				log.Printf("[%s] +%d: %s %s %d -> %d (saldo kurang: %v)", u.id, amount, cur.Name, side, before, cur.Amount, short)
				t := fmt.Sprintf("✅ Tambah pik dikonfirmasi\n👤 %s\n%s %d + %d = %d", nm, side, before, amount, cur.Amount)
				if short {
					t += "\n⚠️ Saldo kurang, diberi tanda ."
				}
				res = rolls.Result{Alert: "Dikonfirmasi.", Text: t}
				applied = true
			})
			if applied {
				go u.sendList(ctx, api, peer, key)
			}
			return res
		},
	}
	token := rolls.Create(ent)
	u.rs.setPending(pkey, token)

	u.sendCard(ctx, api, peer, ent, token, pkey, nm, ttlOf("PLUS_TTL_SEC", 25), "⌛ Tambah pik kedaluwarsa (%d detik)\n👤 %s", false)
}
