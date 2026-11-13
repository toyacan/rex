package ubot

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"cu/internal/slot"
)

func TestIsAccessCmd(t *testing.T) {
	yes := []string{".on", ".ON", ".akses", ".akses  ", ".on\nx", ".Akses!", ".on "}
	no := []string{"on", ".onx", ".aksesx", ".on1", ".list", "", ".o", "x.on"}
	for _, s := range yes {
		if !isAccessCmd(s) {
			t.Errorf("isAccessCmd(%q) = false, harusnya true", s)
		}
	}
	for _, s := range no {
		if isAccessCmd(s) {
			t.Errorf("isAccessCmd(%q) = true, harusnya false", s)
		}
	}
}

func TestParseCmd(t *testing.T) {
	cases := []struct{ in, cmd, arg string; ok bool }{
		{".list", "list", "", true},
		{".List  a  b", "list", "a b", true},
		{".setkk ALBOTZ 20", "setkk", "ALBOTZ 20", true},
		{"list", "", "", false},
		{".", "", "", false},
		{". ", "", "", false},
	}
	for _, c := range cases {
		cmd, arg, ok := parseCmd(c.in)
		if cmd != c.cmd || arg != c.arg || ok != c.ok {
			t.Errorf("parseCmd(%q) = (%q,%q,%v), mau (%q,%q,%v)", c.in, cmd, arg, ok, c.cmd, c.arg, c.ok)
		}
	}
}

func TestRekapRe(t *testing.T) {
	for _, s := range []string{"/rekapwin", "/rekapwin@botx", "/REKAPWIN 12", "/rekapwin\nabc"} {
		if !rekapRe.MatchString(s) {
			t.Errorf("rekapRe harusnya cocok: %q", s)
		}
	}
	for _, s := range []string{"/rekapwinx", "/rekapwin_1", "rekapwin", " /rekapwin"} {
		if rekapRe.MatchString(s) {
			t.Errorf("rekapRe tidak boleh cocok: %q", s)
		}
	}
}

func TestSudoSet(t *testing.T) {
	dir := t.TempDir()
	// format Node: array string; angka juga diterima
	if err := os.WriteFile(filepath.Join(dir, "sudo.json"), []byte(`["111", 222]`), 0o600); err != nil {
		t.Fatal(err)
	}
	s := openSudo(dir)
	if !s.has("111") || !s.has("222") || s.has("333") {
		t.Fatalf("isi awal salah: %v", s.list())
	}
	if !s.add("333") || s.add("333") {
		t.Fatal("add: pertama harus true, kedua false")
	}
	if !s.del("111") || s.del("111") {
		t.Fatal("del: pertama harus true, kedua false")
	}
	s2 := openSudo(dir) // dibaca ulang dari file
	if s2.has("111") || !s2.has("222") || !s2.has("333") {
		t.Fatalf("hasil simpan salah: %v", s2.list())
	}
}

func TestSerialOrder(t *testing.T) {
	var s serial
	var mu sync.Mutex
	var got []int
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		i := i
		wg.Add(1)
		s.do("grup", func() {
			defer wg.Done()
			if i%10 == 0 {
				time.Sleep(2 * time.Millisecond)
			}
			mu.Lock()
			got = append(got, i)
			mu.Unlock()
		})
	}
	wg.Wait()
	for i, v := range got {
		if v != i {
			t.Fatalf("urutan rusak di %d: %v", i, got)
		}
	}
}

func TestSerialPanicTidakMenghentikan(t *testing.T) {
	var s serial
	done := make(chan struct{})
	s.do("g", func() { panic("uji") })
	s.do("g", func() { close(done) })
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pekerjaan setelah panic tidak jalan")
	}
}

func TestResetDanUndo(t *testing.T) {
	dir := t.TempDir()
	kk := slot.KKCfg{K: &slot.KKSide{Name: "ALBOTZ", Amount: 15}, B: &slot.KKSide{Name: "KWINGZ", Amount: 15}}
	u := New(Config{DataDir: dir, KK: kk, ListSuffix: "P"}, "1")
	key := "123"
	u.slots.Edit(key, func(b *slot.Book) {
		b.Started, b.Alert = true, true
		if ok, why := b.Record("K", 50, "ANDI", &slot.Player{UID: "9"}, slot.RecordOpts{Direct: true, KK: kk}); !ok {
			t.Fatalf("record gagal: %s", why)
		}
	})
	u.resetRound(key)
	u.slots.View(key, func(b *slot.Book) {
		if b == nil || !b.Started || !b.Alert {
			t.Errorf("setelah .rs status .on/.alert harus tetap: %+v", b)
		}
		if b != nil && b.HasPicks() {
			t.Error("setelah .rs pik harus kosong")
		}
	})
	if msg := u.undoReset(key); !strings.HasPrefix(msg, "↩️") {
		t.Fatalf("undo harus berhasil, dapat: %s", msg)
	}
	u.slots.View(key, func(b *slot.Book) {
		if !b.HasPicks() || len(b.K) == 0 {
			t.Error("pik harus kembali setelah .undors")
		}
	})
	if msg := u.undoReset(key); strings.HasPrefix(msg, "↩️") {
		t.Errorf("undo kedua harus ditolak, dapat: %s", msg)
	}
}
