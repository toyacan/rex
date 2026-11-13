package rolls

import "testing"

func TestClaimExpire(t *testing.T) {
	e := &Entry{}
	if e.IsDone() || e.IsExpired() {
		t.Fatal("awal harus belum selesai")
	}
	if !e.Claim() {
		t.Fatal("klaim pertama harus berhasil")
	}
	if e.Claim() {
		t.Fatal("klaim kedua harus gagal")
	}
	if e.Expire() || e.IsExpired() {
		t.Fatal("sudah diklaim: tidak boleh jadi kedaluwarsa")
	}

	e2 := &Entry{}
	if !e2.Expire() {
		t.Fatal("expire pertama harus berhasil")
	}
	if !e2.IsExpired() || !e2.IsDone() {
		t.Fatal("kedaluwarsa harus ikut done")
	}
	if e2.Claim() {
		t.Fatal("sudah kedaluwarsa: klaim harus gagal")
	}
}
