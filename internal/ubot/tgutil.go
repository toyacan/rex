package ubot

// Pembantu tahap 3b-2: kirim/ambil pesan, baca pin, daftar anggota, sudo, antrean per grup.

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"github.com/gotd/td/tg"

	"cu/internal/slot"
)

// ---------- sudo.json ----------

// sudoSet: akun lain yang boleh memakai perintah userbot ini. Format file sama dengan Node (array ID).
type sudoSet struct {
	mu   sync.Mutex
	path string
	ids  map[string]bool
}

func anyStr(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', 0, 64)
	}
	return fmt.Sprint(v)
}

func openSudo(dir string) *sudoSet {
	s := &sudoSet{path: filepath.Join(dir, "sudo.json"), ids: map[string]bool{}}
	if b, err := os.ReadFile(s.path); err == nil {
		var arr []any
		if json.Unmarshal(b, &arr) == nil {
			for _, v := range arr {
				if id := anyStr(v); id != "" {
					s.ids[id] = true
				}
			}
		}
	}
	return s
}

func (s *sudoSet) has(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ids[id]
}

func (s *sudoSet) list() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.ids))
	for id := range s.ids {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func (s *sudoSet) saveLocked() {
	ids := make([]string, 0, len(s.ids))
	for id := range s.ids {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	j, _ := json.Marshal(ids)
	if err := os.WriteFile(s.path, j, 0o600); err != nil {
		log.Printf("sudo: gagal menyimpan: %v", err)
	}
}

// add: false bila sudah ada.
func (s *sudoSet) add(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ids[id] {
		return false
	}
	s.ids[id] = true
	s.saveLocked()
	return true
}

// del: false bila tidak ada.
func (s *sudoSet) del(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.ids[id] {
		return false
	}
	delete(s.ids, id)
	s.saveLocked()
	return true
}

// ---------- antrean berurutan per grup ----------

// serial menjalankan pekerjaan satu kunci (grup) BERURUTAN, kunci berbeda tidak saling menunggu.
type serial struct {
	mu sync.Mutex
	q  map[string]*lane
}

type lane struct {
	jobs    []func()
	running bool
}

func (s *serial) do(key string, fn func()) {
	s.mu.Lock()
	if s.q == nil {
		s.q = map[string]*lane{}
	}
	l := s.q[key]
	if l == nil {
		l = &lane{}
		s.q[key] = l
	}
	l.jobs = append(l.jobs, fn)
	if l.running {
		s.mu.Unlock()
		return
	}
	l.running = true
	s.mu.Unlock()
	go func() {
		for {
			s.mu.Lock()
			if len(l.jobs) == 0 {
				l.running = false
				delete(s.q, key)
				s.mu.Unlock()
				return
			}
			j := l.jobs[0]
			l.jobs = l.jobs[1:]
			s.mu.Unlock()
			func() {
				defer func() {
					if r := recover(); r != nil {
						log.Printf("panic di antrean grup %s: %v", key, r)
					}
				}()
				j()
			}()
		}
	}()
}

// ---------- pembantu pesan ----------

func fromUser(m *tg.Message) int64 {
	if p, ok := m.FromID.(*tg.PeerUser); ok {
		return p.UserID
	}
	return 0
}

func replyID(m *tg.Message) int {
	if h, ok := m.ReplyTo.(*tg.MessageReplyHeader); ok {
		return h.ReplyToMsgID
	}
	return 0
}

func displayName(us *tg.User) string {
	n := strings.TrimSpace(us.FirstName + " " + us.LastName)
	if n == "" {
		n = us.Username
	}
	return n
}

func userAH(us *tg.User) string {
	if us != nil && us.AccessHash != 0 {
		return strconv.FormatInt(us.AccessHash, 10)
	}
	return ""
}

func u16(s string) int { return len(utf16.Encode([]rune(s))) }

func sentMsgID(upd tg.UpdatesClass) int {
	switch v := upd.(type) {
	case *tg.UpdateShortSentMessage:
		return v.ID
	case *tg.Updates:
		for _, x := range v.Updates {
			if m, ok := x.(*tg.UpdateMessageID); ok {
				return m.ID
			}
		}
	case *tg.UpdatesCombined:
		for _, x := range v.Updates {
			if m, ok := x.(*tg.UpdateMessageID); ok {
				return m.ID
			}
		}
	}
	return 0
}

// send mengirim pesan baru (replyTo 0 = bukan balasan). Mengembalikan ID pesan yang terkirim.
func (u *Userbot) send(ctx context.Context, api *tg.Client, peer tg.InputPeerClass, text string, replyTo int, ents []tg.MessageEntityClass) (int, error) {
	req := &tg.MessagesSendMessageRequest{Peer: peer, Message: text, RandomID: rand.Int64()}
	if len(ents) > 0 {
		req.Entities = ents
	}
	if replyTo != 0 {
		req.ReplyTo = &tg.InputReplyToMessage{ReplyToMsgID: replyTo}
	}
	upd, err := api.MessagesSendMessage(ctx, req)
	if err != nil {
		log.Printf("[%s] gagal mengirim pesan: %v", u.id, err)
		return 0, err
	}
	return sentMsgID(upd), nil
}

func unpackMessages(r tg.MessagesMessagesClass) ([]tg.MessageClass, []tg.UserClass) {
	switch v := r.(type) {
	case *tg.MessagesMessages:
		return v.Messages, v.Users
	case *tg.MessagesMessagesSlice:
		return v.Messages, v.Users
	case *tg.MessagesChannelMessages:
		return v.Messages, v.Users
	}
	return nil, nil
}

func usersToMap(us []tg.UserClass, into map[int64]*tg.User) {
	for _, x := range us {
		if v, ok := x.(*tg.User); ok {
			into[v.ID] = v
		}
	}
}

// userMap menyalin pengguna dari entitas update (supaya aman diubah).
func userMap(e tg.Entities) map[int64]*tg.User {
	m := make(map[int64]*tg.User, len(e.Users)+2)
	for id, us := range e.Users {
		m[id] = us
	}
	return m
}

// fetchMsg mengambil satu pesan berdasarkan ID (batas 8 detik) beserta pengguna yang menyertainya.
func (u *Userbot) fetchMsg(ctx context.Context, api *tg.Client, peer tg.InputPeerClass, id int) (*tg.Message, map[int64]*tg.User, error) {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	ids := []tg.InputMessageClass{&tg.InputMessageID{ID: id}}
	var res tg.MessagesMessagesClass
	var err error
	if ch, ok := peer.(*tg.InputPeerChannel); ok {
		res, err = api.ChannelsGetMessages(ctx, &tg.ChannelsGetMessagesRequest{
			Channel: &tg.InputChannel{ChannelID: ch.ChannelID, AccessHash: ch.AccessHash},
			ID:      ids,
		})
	} else {
		res, err = api.MessagesGetMessages(ctx, ids)
	}
	if err != nil {
		return nil, nil, err
	}
	msgs, users := unpackMessages(res)
	um := map[int64]*tg.User{}
	usersToMap(users, um)
	for _, m := range msgs {
		if mm, ok := m.(*tg.Message); ok {
			return mm, um, nil
		}
	}
	return nil, um, nil
}

// resolvePlayer: pemain asli dari sebuah pesan. Bila pengirimnya bot (mis. konfirmasi "Pik: K 30 ..."),
// ikuti balasannya satu tingkat ke pesan pemainnya. nil = tidak terbaca / akun sendiri.
func (u *Userbot) resolvePlayer(ctx context.Context, api *tg.Client, peer tg.InputPeerClass, m *tg.Message, users map[int64]*tg.User, meID int64) *slot.Player {
	for i := 0; i < 2 && m != nil; i++ {
		if uid := fromUser(m); uid != 0 {
			us := users[uid]
			if us == nil || !us.Bot {
				if uid == meID {
					return nil
				}
				return &slot.Player{UID: strconv.FormatInt(uid, 10), AH: userAH(us)}
			}
		}
		rid := replyID(m)
		if rid == 0 {
			return nil
		}
		next, more, err := u.fetchMsg(ctx, api, peer, rid)
		if err != nil || next == nil {
			return nil
		}
		for id, us := range more {
			users[id] = us
		}
		m = next
	}
	return nil
}

// ---------- pin ----------

type pinEntry struct {
	at   time.Time
	text string
}

func pinKeyOfPeer(p tg.PeerClass) string {
	switch v := p.(type) {
	case *tg.PeerChat:
		return strconv.FormatInt(v.ChatID, 10)
	case *tg.PeerChannel:
		return strconv.FormatInt(v.ChannelID, 10)
	}
	return ""
}

func pinKeyOfInput(p tg.InputPeerClass) string {
	switch v := p.(type) {
	case *tg.InputPeerChat:
		return strconv.FormatInt(v.ChatID, 10)
	case *tg.InputPeerChannel:
		return strconv.FormatInt(v.ChannelID, 10)
	}
	return ""
}

func (u *Userbot) dropPin(key string) {
	u.pinMu.Lock()
	delete(u.pins, key)
	u.pinMu.Unlock()
}

// fetchPinText: teks pin terbaru grup ("" bila tidak ada).
func (u *Userbot) fetchPinText(ctx context.Context, api *tg.Client, peer tg.InputPeerClass) (string, error) {
	pinID := 0
	switch p := peer.(type) {
	case *tg.InputPeerChannel:
		full, err := api.ChannelsGetFullChannel(ctx, &tg.InputChannel{ChannelID: p.ChannelID, AccessHash: p.AccessHash})
		if err != nil {
			log.Printf("[%s] gagal membaca info pin: %v", u.id, err)
		} else if cf, ok := full.FullChat.(*tg.ChannelFull); ok {
			pinID = cf.PinnedMsgID
		}
	case *tg.InputPeerChat:
		full, err := api.MessagesGetFullChat(ctx, p.ChatID)
		if err != nil {
			log.Printf("[%s] gagal membaca info pin: %v", u.id, err)
		} else if cf, ok := full.FullChat.(*tg.ChatFull); ok {
			pinID = cf.PinnedMsgID
		}
	}
	if pinID != 0 {
		if m, _, err := u.fetchMsg(ctx, api, peer, pinID); err == nil && m != nil && m.Message != "" {
			return m.Message, nil
		}
	}
	res, err := api.MessagesSearch(ctx, &tg.MessagesSearchRequest{
		Peer: peer, Filter: &tg.InputMessagesFilterPinned{}, Limit: 1,
	})
	if err != nil {
		return "", err
	}
	msgs, _ := unpackMessages(res)
	for _, m := range msgs {
		if mm, ok := m.(*tg.Message); ok && mm.Message != "" {
			return mm.Message, nil
		}
	}
	return "", nil
}

// fetchPinPlayers: { NICK -> jumlah } dari pin terakhir; nil bila pin tidak ada / tidak berisi daftar.
// ttl > 0: pakai hasil baca yang lebih muda dari ttl (pin baru otomatis membuang cache). 0 = baca segar.
func (u *Userbot) fetchPinPlayers(ctx context.Context, api *tg.Client, peer tg.InputPeerClass, ttl time.Duration) map[string]int {
	pk := pinKeyOfInput(peer)
	u.pinMu.Lock()
	hit, have := u.pins[pk]
	u.pinMu.Unlock()
	var text string
	if ttl > 0 && have && time.Since(hit.at) < ttl {
		text = hit.text
	} else {
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		t, err := u.fetchPinText(cctx, api, peer)
		cancel()
		if err != nil {
			// jaringan macet: pakai pin lama dari cache supaya perintah tetap dijawab
			log.Printf("[%s] baca pin gagal/lambat (%v), pakai cache lama", u.id, err)
			text = hit.text
		} else {
			text = t
			u.pinMu.Lock()
			u.pins[pk] = pinEntry{at: time.Now(), text: t}
			u.pinMu.Unlock()
		}
	}
	return slot.ParsePinPlayers(text)
}

// ---------- anggota grup ----------

// participants: anggota grup (maks ~5000 untuk supergrup).
func (u *Userbot) participants(ctx context.Context, api *tg.Client, peer tg.InputPeerClass) ([]*tg.User, error) {
	var all []*tg.User
	switch p := peer.(type) {
	case *tg.InputPeerChannel:
		ch := &tg.InputChannel{ChannelID: p.ChannelID, AccessHash: p.AccessHash}
		for off := 0; off < 5000; off += 200 {
			res, err := api.ChannelsGetParticipants(ctx, &tg.ChannelsGetParticipantsRequest{
				Channel: ch, Filter: &tg.ChannelParticipantsRecent{}, Offset: off, Limit: 200,
			})
			if err != nil {
				if len(all) > 0 {
					return all, nil
				}
				return nil, err
			}
			cp, ok := res.(*tg.ChannelsChannelParticipants)
			if !ok || len(cp.Participants) == 0 {
				break
			}
			for _, x := range cp.Users {
				if us, ok := x.(*tg.User); ok {
					all = append(all, us)
				}
			}
			if len(cp.Participants) < 200 {
				break
			}
		}
	case *tg.InputPeerChat:
		full, err := api.MessagesGetFullChat(ctx, p.ChatID)
		if err != nil {
			return nil, err
		}
		for _, x := range full.Users {
			if us, ok := x.(*tg.User); ok {
				all = append(all, us)
			}
		}
	}
	return all, nil
}

// ---------- data pendukung (baca saja, ditulis versi Node) ----------

// contactNames: uid -> nama kontak hasil .sv (contacts.json). Kosong bila belum ada.
func (u *Userbot) contactNames() map[string]string {
	out := map[string]string{}
	b, err := os.ReadFile(filepath.Join(u.dir, "contacts.json"))
	if err != nil {
		return out
	}
	var raw map[string]struct {
		Name string `json:"name"`
	}
	if json.Unmarshal(b, &raw) != nil {
		return out
	}
	for uid, v := range raw {
		if v.Name != "" {
			out[uid] = v.Name
		}
	}
	return out
}

// svownSet: pemain yang selalu bertanda P (svown.json).
func (u *Userbot) svownSet() map[string]bool {
	out := map[string]bool{}
	b, err := os.ReadFile(filepath.Join(u.dir, "svown.json"))
	if err != nil {
		return out
	}
	var arr []any
	if json.Unmarshal(b, &arr) != nil {
		return out
	}
	for _, v := range arr {
		if id := anyStr(v); id != "" {
			out[id] = true
		}
	}
	return out
}

func (u *Userbot) formatCtx() slot.FormatCtx {
	return slot.FormatCtx{Suffix: u.cfg.ListSuffix, Contacts: u.contactNames(), Svown: u.svownSet(), KK: u.cfg.KK}
}

// ---------- buku pik ----------

func cloneBook(b *slot.Book) *slot.Book {
	if b == nil {
		return nil
	}
	j, err := json.Marshal(b)
	if err != nil {
		return nil
	}
	nb := &slot.Book{}
	if err := json.Unmarshal(j, nb); err != nil {
		return nil
	}
	return nb
}

func sumPicks(a []*slot.Pick) int {
	n := 0
	for _, p := range a {
		n += p.Amount
	}
	return n
}

// bookFlag membaca satu nilai dari buku chat (false bila belum ada data).
func (u *Userbot) bookFlag(key string, f func(b *slot.Book) bool) bool {
	r := false
	u.slots.View(key, func(b *slot.Book) {
		if b != nil {
			r = f(b)
		}
	})
	return r
}

func (u *Userbot) recording(key string) bool {
	r := false
	u.slots.View(key, func(b *slot.Book) { r = b.Recording() })
	return r
}

// ---------- balasan perintah ----------

// replier: perintah dari akun sendiri = edit pesan perintah; dari sudo = balas pesannya (pesan sudo bukan milik kita).
type replier struct {
	u    *Userbot
	api  *tg.Client
	peer tg.InputPeerClass
	msg  *tg.Message
	own  bool
	sent int
}

func (r *replier) edit(ctx context.Context, text string) {
	if r.own {
		r.u.edit(ctx, r.api, r.peer, r.msg.ID, text)
		return
	}
	if r.sent != 0 {
		r.u.edit(ctx, r.api, r.peer, r.sent, text)
		return
	}
	if id, err := r.u.send(ctx, r.api, r.peer, text, r.msg.ID, nil); err == nil {
		r.sent = id
	}
}

// more: pesan tambahan (potongan list berikutnya, rekap, dll).
func (r *replier) more(ctx context.Context, text string, replyTo int) int {
	id, _ := r.u.send(ctx, r.api, r.peer, text, replyTo, nil)
	return id
}
