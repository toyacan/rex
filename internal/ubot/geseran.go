package ubot

// Tahap 3b-3: geseran. Preset lewat .geseran <key> <nominal> <max user>; pemain memicu dengan
// "#key b" / "#key k" (pik sebesar nominal, maksimal sejumlah user). "used" direset oleh .rs.
// Format geseran.json sama dengan versi Node: { "<chatId>": { "<key>": { amount, max, used: ["<userId>"] } } }

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/gotd/td/tg"

	"cu/internal/slot"
)

var (
	gesTrigRe = regexp.MustCompile(`(?i)^\s*#(\w{1,32})\s+([KB])\s*$`) // "#dlzzcc b" / "#dlzzcc k"
	gesDelRe  = regexp.MustCompile(`(?i)^(?:del|hapus)\s+#?(\w{1,32})$`)
	gesSetRe  = regexp.MustCompile(`^#?(\w{1,32})\s+(\d+)(?:\s+(\d+))?$`)
)

const gesUsage = "Gunakan:\n" +
	".geseran <key> <nominal> <max user> - Preset key\n" +
	".geseran - Daftar key geseran\n" +
	".geseran del <key> - Hapus key\n" +
	"Contoh: .geseran #dlzzcc 10 1"

type gesEntry struct {
	Amount int      `json:"amount"`
	Max    int      `json:"max"`
	Used   []string `json:"used"`
}

type geseranStore struct {
	mu   sync.Mutex
	path string
	data map[string]map[string]*gesEntry
}

func openGeseran(dir string) *geseranStore {
	g := &geseranStore{path: filepath.Join(dir, "geseran.json"), data: map[string]map[string]*gesEntry{}}
	if b, err := os.ReadFile(g.path); err == nil {
		var m map[string]map[string]*gesEntry
		if json.Unmarshal(b, &m) == nil && m != nil {
			g.data = m
		} else {
			log.Printf("geseran: %s tidak terbaca, mulai kosong", g.path)
		}
	}
	for _, set := range g.data {
		for k, e := range set {
			if e == nil {
				delete(set, k)
				continue
			}
			if e.Used == nil {
				e.Used = []string{}
			}
		}
	}
	return g
}

func (g *geseranStore) saveLocked() {
	j, err := json.Marshal(g.data)
	if err != nil {
		return
	}
	tmp := g.path + ".tmp"
	if err := os.WriteFile(tmp, j, 0o600); err != nil {
		log.Printf("geseran: gagal menyimpan: %v", err)
		return
	}
	_ = os.Rename(tmp, g.path)
}

func containsStr(a []string, s string) bool {
	for _, x := range a {
		if x == s {
			return true
		}
	}
	return false
}

// preset menyimpan / mengganti satu key (hitungan terpakai mulai dari 0).
func (g *geseranStore) preset(chat, key string, amount, max int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.data[chat] == nil {
		g.data[chat] = map[string]*gesEntry{}
	}
	g.data[chat][key] = &gesEntry{Amount: amount, Max: max, Used: []string{}}
	g.saveLocked()
}

func (g *geseranStore) del(chat, key string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	set := g.data[chat]
	if set == nil || set[key] == nil {
		return false
	}
	delete(set, key)
	if len(set) == 0 {
		delete(g.data, chat)
	}
	g.saveLocked()
	return true
}

type gesRow struct {
	Key    string
	Amount int
	Max    int
	Used   int
}

func (g *geseranStore) list(chat string) []gesRow {
	g.mu.Lock()
	defer g.mu.Unlock()
	var rows []gesRow
	for k, e := range g.data[chat] {
		rows = append(rows, gesRow{Key: k, Amount: e.Amount, Max: e.Max, Used: len(e.Used)})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Key < rows[j].Key })
	return rows
}

// Hasil tryClaim.
const (
	gesNone = iota // key tidak ada
	gesDup         // pemain ini sudah memakai key (tidak ada yang terjadi)
	gesFull        // sudah penuh
	gesOK          // slot dipesan
)

// tryClaim memesan satu slot untuk uid (atomik, supaya pemicu bersamaan tidak melebihi max).
func (g *geseranStore) tryClaim(chat, key, uid string) (status, amount int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	e := g.data[chat][key]
	if e == nil {
		return gesNone, 0
	}
	if containsStr(e.Used, uid) {
		return gesDup, e.Amount
	}
	if len(e.Used) >= e.Max {
		return gesFull, e.Amount
	}
	e.Used = append(e.Used, uid)
	return gesOK, e.Amount
}

// isFull: key penuh dan uid belum termasuk pemakainya.
func (g *geseranStore) isFull(chat, key, uid string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	e := g.data[chat][key]
	return e != nil && len(e.Used) >= e.Max && !containsStr(e.Used, uid)
}

func (g *geseranStore) commit() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.saveLocked()
}

// release mengembalikan slot (pik gagal dicatat: sisi ditutup, dll).
func (g *geseranStore) release(chat, key, uid string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	e := g.data[chat][key]
	if e == nil {
		return
	}
	keep := make([]string, 0, len(e.Used))
	for _, x := range e.Used {
		if x != uid {
			keep = append(keep, x)
		}
	}
	e.Used = keep
}

// resetUsed (.rs): preset tetap ada, hitungan terpakai mulai dari 0 lagi.
func (g *geseranStore) resetUsed(chat string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	set := g.data[chat]
	if len(set) == 0 {
		return
	}
	for _, e := range set {
		e.Used = []string{}
	}
	g.saveLocked()
}

// snapshotUsed: key -> daftar pemakai yang masih terisi (kosong bila tidak ada; dipakai .undors).
func (g *geseranStore) snapshotUsed(chat string) map[string][]string {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := map[string][]string{}
	for k, e := range g.data[chat] {
		if len(e.Used) > 0 {
			out[k] = append([]string{}, e.Used...)
		}
	}
	return out
}

func (g *geseranStore) restoreUsed(chat string, used map[string][]string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for k, e := range g.data[chat] {
		if v, ok := used[k]; ok {
			e.Used = append([]string{}, v...)
		}
	}
	g.saveLocked()
}

// geseranTrigger: pemain mengetik "#key b" / "#key k".
func (u *Userbot) geseranTrigger(ctx context.Context, api *tg.Client, peer tg.InputPeerClass, key string, msg *tg.Message, us *tg.User, gkey, side string) {
	if us == nil || us.Bot {
		return
	}
	uid := strconv.FormatInt(us.ID, 10)
	// geseran sudah penuh: balas pemain yang mencoba memicunya
	if u.ges.isFull(key, gkey, uid) {
		u.more(ctx, api, peer, "Full Next Mingdep Bangg🤩", msg.ID)
		return
	}
	status, amount := u.ges.tryClaim(key, gkey, uid)
	if status != gesOK {
		return
	}
	nm := displayName(us)
	if nm == "" {
		nm = uid
	}
	player := &slot.Player{UID: uid, AH: userAH(us)}
	o := slot.RecordOpts{Direct: true, MsgIDs: []int{msg.ID}, PMID: msg.ID}
	ok, why := u.commitPick(key, side, amount, strings.ToUpper(nm), player, o)
	if ok {
		u.ges.commit()
		go u.sendAlert(ctx, api, peer, key, msg, us, side, amount, nm)
		return
	}
	u.ges.release(key, gkey, uid) // gagal (sisi ditutup, dll): slot dikembalikan
	if why == slot.RejectClosed || why == slot.RejectRK {
		u.more(ctx, api, peer, fmt.Sprintf("🔒 %s sudah CLOSE\nTidak bisa taruh %s. Buka: .ok", side, side), msg.ID)
	}
}

// cmdGeseran: .geseran <key> <nominal> <max user> | .geseran | .geseran del <key>
func (u *Userbot) cmdGeseran(ctx context.Context, r *replier, key, arg string) {
	a0 := strings.TrimSpace(arg)
	if a0 == "" {
		rows := u.ges.list(key)
		if len(rows) == 0 {
			r.edit(ctx, "Belum ada geseran di grup ini.\n\n"+gesUsage)
			return
		}
		lines := make([]string, 0, len(rows))
		for _, x := range rows {
			lines = append(lines, fmt.Sprintf("• %s — %d (max %d, terpakai %d)", x.Key, x.Amount, x.Max, x.Used))
		}
		r.edit(ctx, "🔑 Geseran grup ini\n\n"+strings.Join(lines, "\n"))
		return
	}
	if m := gesDelRe.FindStringSubmatch(a0); m != nil {
		k := strings.ToLower(m[1])
		if !u.ges.del(key, k) {
			r.edit(ctx, fmt.Sprintf("ℹ️ Geseran %s tidak ada.", k))
			return
		}
		r.edit(ctx, fmt.Sprintf("🗑️ Geseran %s dihapus.", k))
		return
	}
	m := gesSetRe.FindStringSubmatch(a0)
	if m == nil {
		r.edit(ctx, gesUsage)
		return
	}
	gkey := strings.ToLower(m[1])
	amount, _ := strconv.Atoi(m[2])
	max := 1
	if m[3] != "" {
		max, _ = strconv.Atoi(m[3])
	}
	if amount <= 0 || max <= 0 {
		r.edit(ctx, "⚠️ Nominal dan max user harus lebih dari 0.\n\n"+gesUsage)
		return
	}
	u.ges.preset(key, gkey, amount, max)
	r.edit(ctx, fmt.Sprintf("✅ Geseran %s\nNominal: %d\nMax user: %d\nTrigger: #%s b / #%s k", gkey, amount, max, gkey, gkey))
}
