package ubot

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cu/internal/slot"
)

func TestRollAndPlusRegex(t *testing.T) {
	for _, s := range []string{"r", "R", "r.", "roll!", "Rol", "ROLL.."} {
		if !rollRe.MatchString(s) {
			t.Errorf("rollRe harusnya cocok: %q", s)
		}
	}
	for _, s := range []string{"rr", "ro", "r r", "roll up", "", "ra"} {
		if rollRe.MatchString(s) {
			t.Errorf("rollRe tidak boleh cocok: %q", s)
		}
	}
	for _, s := range []string{"+2", " + 10 ", "+300"} {
		if !plusRe.MatchString(s) {
			t.Errorf("plusRe harusnya cocok: %q", s)
		}
	}
	for _, s := range []string{"+", "+a", "2+", "+2 k", "++2"} {
		if plusRe.MatchString(s) {
			t.Errorf("plusRe tidak boleh cocok: %q", s)
		}
	}
	m := gesTrigRe.FindStringSubmatch(" #DlzzCC b ")
	if m == nil || m[1] != "DlzzCC" || strings.ToUpper(m[2]) != "B" {
		t.Errorf("gesTrigRe salah: %v", m)
	}
	if gesTrigRe.MatchString("#key x") || gesTrigRe.MatchString("#key b extra") {
		t.Error("gesTrigRe terlalu longgar")
	}
}

func TestGeseranStore(t *testing.T) {
	dir := t.TempDir()
	g := openGeseran(dir)
	g.preset("123", "dlzz", 10, 2)

	if st, _ := g.tryClaim("123", "nope", "1"); st != gesNone {
		t.Errorf("key tak ada: status %d", st)
	}
	if st, amt := g.tryClaim("123", "dlzz", "1"); st != gesOK || amt != 10 {
		t.Errorf("klaim pertama: %d %d", st, amt)
	}
	if st, _ := g.tryClaim("123", "dlzz", "1"); st != gesDup {
		t.Errorf("klaim ganda harus dup, dapat %d", st)
	}
	if g.isFull("123", "dlzz", "2") {
		t.Error("belum penuh")
	}
	if st, _ := g.tryClaim("123", "dlzz", "2"); st != gesOK {
		t.Errorf("klaim kedua: %d", st)
	}
	if !g.isFull("123", "dlzz", "3") {
		t.Error("harusnya penuh untuk pemain lain")
	}
	if g.isFull("123", "dlzz", "1") {
		t.Error("pemakai lama bukan 'penuh'")
	}
	if st, _ := g.tryClaim("123", "dlzz", "3"); st != gesFull {
		t.Errorf("harus penuh, dapat %d", st)
	}

	// kembalikan slot
	g.release("123", "dlzz", "2")
	if st, _ := g.tryClaim("123", "dlzz", "3"); st != gesOK {
		t.Errorf("setelah release harus bisa: %d", st)
	}

	// .rs lalu .undors
	snap := g.snapshotUsed("123")
	if len(snap["dlzz"]) != 2 {
		t.Fatalf("snapshot: %v", snap)
	}
	g.resetUsed("123")
	if rows := g.list("123"); len(rows) != 1 || rows[0].Used != 0 {
		t.Errorf("setelah reset: %+v", rows)
	}
	g.restoreUsed("123", snap)
	if rows := g.list("123"); rows[0].Used != 2 {
		t.Errorf("setelah restore: %+v", rows)
	}

	// tersimpan & terbaca ulang
	g.commit()
	g2 := openGeseran(dir)
	if rows := g2.list("123"); len(rows) != 1 || rows[0].Key != "dlzz" || rows[0].Amount != 10 || rows[0].Max != 2 || rows[0].Used != 2 {
		t.Errorf("baca ulang: %+v", rows)
	}
	if !g2.del("123", "dlzz") || g2.del("123", "dlzz") {
		t.Error("del harus true sekali lalu false")
	}
	if len(g2.list("123")) != 0 {
		t.Error("harus kosong setelah del")
	}
}

func TestGeseranReadsNodeFormat(t *testing.T) {
	dir := t.TempDir()
	js := `{"555":{"abc":{"amount":25,"max":3,"used":["7","8"]}}}`
	if err := os.WriteFile(filepath.Join(dir, "geseran.json"), []byte(js), 0o600); err != nil {
		t.Fatal(err)
	}
	g := openGeseran(dir)
	rows := g.list("555")
	if len(rows) != 1 || rows[0].Amount != 25 || rows[0].Max != 3 || rows[0].Used != 2 {
		t.Errorf("format Node tidak terbaca: %+v", rows)
	}
}

func pk(name string, amount int, uid string) *slot.Pick {
	return &slot.Pick{Name: name, Amount: amount, UID: uid}
}

func rollBook(k, b []*slot.Pick) *slot.Book {
	return &slot.Book{K: k, B: b, Started: true, RK: true}
}

func TestApplyRollTo(t *testing.T) {
	u := &Userbot{id: "t"}

	// K tidak ada, bukan RK
	nb := &slot.Book{K: []*slot.Pick{pk("A", 10, "1")}, B: []*slot.Pick{pk("B", 30, "2")}}
	if got := u.applyRollTo(nb, 1, "1", "A", ""); !strings.HasPrefix(got, "ℹ️") {
		t.Errorf("tanpa RK: %q", got)
	}

	// seimbang
	b := rollBook([]*slot.Pick{pk("A", 30, "1")}, []*slot.Pick{pk("B", 30, "2")})
	if got := u.applyRollTo(b, 1, "1", "A", ""); !strings.HasPrefix(got, "ℹ️") {
		t.Errorf("seimbang: %q", got)
	}

	// sisi kurang ditutup
	b = rollBook([]*slot.Pick{pk("A", 10, "1")}, []*slot.Pick{pk("B", 30, "2")})
	b.Closed = map[string]bool{"K": true}
	if got := u.applyRollTo(b, 1, "9", "Z", ""); !strings.HasPrefix(got, "🔒") {
		t.Errorf("ditutup: %q", got)
	}

	// pemain sudah di sisi kurang: ditambah sebesar selisih
	b = rollBook([]*slot.Pick{pk("A", 10, "1")}, []*slot.Pick{pk("B", 30, "2")})
	got := u.applyRollTo(b, 1, "1", "A", "")
	if got != "A ditambah K 20 (jadi 30)" || b.K[0].Amount != 30 {
		t.Errorf("tambah: %q amount=%d", got, b.K[0].Amount)
	}

	// pemain di sisi berlebih: dikurangi, tetap ada
	b = rollBook([]*slot.Pick{pk("A", 10, "1")}, []*slot.Pick{pk("B", 50, "2")})
	got = u.applyRollTo(b, 1, "2", "B", "")
	if got != "B dikurangi B 40 (jadi 10)" || b.B[0].Amount != 10 {
		t.Errorf("kurangi: %q amount=%d", got, b.B[0].Amount)
	}

	// pemain di sisi berlebih tepat habis: keluar dari list
	b = rollBook([]*slot.Pick{pk("A", 10, "1")}, []*slot.Pick{pk("B", 50, "2"), pk("C", 10, "3")})
	// K=10, B=60 -> selisih 50; C hanya 10 -> habis, sisa 40 masuk K atas nama C
	got = u.applyRollTo(b, 1, "3", "C", "")
	if got != "C keluar dari B (10) dan masuk K 40" {
		t.Errorf("habis + sisa: %q", got)
	}
	if len(b.B) != 1 || b.B[0].Name != "B" {
		t.Errorf("C harus keluar dari B: %+v", b.B)
	}
	if len(b.K) != 2 || b.K[1].Name != "C" || b.K[1].Amount != 40 || b.K[1].UID != "3" {
		t.Errorf("sisa harus masuk K sebagai satu pik: %+v", b.K[len(b.K)-1])
	}

	// pemain belum ada di list: masuk sisi kurang sebesar selisih
	b = rollBook([]*slot.Pick{pk("A", 10, "1")}, []*slot.Pick{pk("B", 50, "2")})
	got = u.applyRollTo(b, 7, "9", "NEWB", "")
	if got != "NEWB masuk K 40" {
		t.Errorf("pemain baru: %q", got)
	}
	found := false
	for _, p := range b.K {
		if p.UID == "9" && p.Amount == 40 {
			found = true
		}
	}
	if !found {
		t.Errorf("pemain baru tidak tercatat di K: %+v", b.K)
	}
}
