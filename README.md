# Port Node.js -> Go

## Status
- Tahap 1 (selesai): `.env`, enkripsi sesi (kompatibel `secure.js`), manager pengguna.
- Tahap 2 (selesai ditulis, BELUM dikompilasi): bot utama, /buy QRIS Kasera, trial, admin, /bcpm,
  emoji premium, menu `.cmd` + Roll Request via inline.
- Tahap 3a (selesai ditulis, BELUM dikompilasi): kerangka userbot gotd: sambung/putus + sambung ulang,
  sesi terenkripsi, login OTP/2FA lewat chat, logout, perintah `.ping` dan `.id`.
  Sesi lama versi Node (`data/<id>/session.txt`) dibaca & dikonversi otomatis (file lama TIDAK diubah;
  sesi Go disimpan di `session.gotd`). Perintah lain (`.list`, `.rk`, `.sk`, ...) = tahap 3b dst.

- Tahap 3b-2 (selesai ditulis, BELUM dikompilasi): paket `internal/slot` disambung ke userbot (lihat bagian bawah).
- Tahap 3b-3 (selesai ditulis, BELUM dikompilasi): roll `r`, `+N` (kartu Konfirm/Tolak lewat inline bot) dan geseran `#key` + `.geseran` (lihat bagian bawah).

Sengaja tidak diport: `/broadcast` (kirim ke semua grup lewat userbot semua pelanggan) dan alur manual `buy-manual.js`.

## Cek sesi lama dulu (penting)
    go run ./cmd/checksession data/<id>/session.txt
Harus menampilkan "format GramJS OK". Kalau gagal, kirim pesannya (tanpa isi sesi) supaya parsernya kusesuaikan.
JANGAN jalankan userbot Node dan Go bersamaan untuk akun yang sama (Telegram akan memutus sesi: AUTH_KEY_DUPLICATED).

## Cara coba
    go mod tidy            # mengunduh gotd/td dan skip2/go-qrcode (butuh Go 1.23+ / toolchain terbaru)
    go vet ./... && go test ./...
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o cu .

Format `data/users.json`, `trials.json`, `orders.json`, `bot_pm.json` sama dengan versi Node,
jadi data lama dipakai langsung. Jangan jalankan versi Node dan Go bersamaan dengan BOT_TOKEN yang sama.

## Tanpa Go di komputer: kompilasi lewat GitHub
1. Buat repositori PRIVAT di GitHub, unggah isi folder ini (JANGAN unggah `.env` atau folder `data/`).
2. Tab Actions -> "build" -> Run workflow. Satu kali jalan menampilkan semua error `go vet`, `go test`, dan build.
3. Bila hijau, unduh artifact `cu-linux-amd64` (file `cu`) lalu unggah ke panel.
Kirim log merah (salin teksnya) ke Claude untuk diperbaiki.

## Paket `internal/slot` (tahap 3b-1)
Inti pencatatan pik K/B tanpa dependensi Telegram: parsing (`k50`, `.perak`, `kall`), `.list`, `.rk`,
aturan sisi tertutup / setelah `.rk` / anti-clone, `.kk`, `.rs`, dan `picks.json` (format sama dengan Node).
Nilai harapan di tes parsing diambil dari menjalankan fungsi asli di ubot.js. (Sudah disambung di tahap 3b-2, lihat bawah.)

## Tahap 3b-2: pencatatan pik + perintah dasar (`internal/ubot/handler.go`, `tgutil.go`)
Sudah jalan (logika mengikuti ubot.js):
- Pencatatan di grup yang sudah `.on`/`.akses`: pik langsung (`k50`, mode `.perak`: `k10,9`), konfirmasi `Pik: K 30 NAMA`,
  `kall`/`ball` (saldo dari pin), balasan `.alert` dengan mention pemain, pesan "🔒 ... sudah CLOSE".
- Perintah: `.on/.akses`, `.off/.delakses`, `.rs`, `.undors` (10 menit, di memori), `.list`, `.rk`, `.ck/.cb/.ok`,
  `.kk on|off`, `.setkk/.setkb`, `.alert`, `.autors` (+ `/rekapwin`), `.perak/.nonperak`, `.addsudo/.delsudo/.sudolist`, `.ping`, `.id`.
- Sudo: perintah dari akun sudo dibalas (bukan di-edit), seperti `wrapForSudo`. Pesan dalam satu grup diproses berurutan.
- `.env` baru yang dibaca: `KK_K`, `KK_B` (bawaan `ALBOTZ 15` / `KWINGZ 15`), `LIST_SUFFIX` (bawaan `P`).
- `picks.json`, `sudo.json` dipakai langsung dari versi Node; `contacts.json` dan `svown.json` dibaca (belum ditulis Go).

Belum (menyusul): `r`/roll + `+N`, geseran (`#key`), `said.json` ("pik 20"), `.p/.c`, `.sk/.sb`, `.tambah/.kurang`, `.wd`, `.tf`, `.sv*`,
`.open/.close`, `.font`, `.setemoji/.eid*`, `.cmd/.help`, emoji premium di pesan userbot, `LIST_NEWMSG`.

Beda kecil yang disengaja: pesan tidak dideduplikasi (Node memakai polling cadangan), dan daftar anggota untuk pengikatan ID
di `.on` memakai maksimal ~5000 anggota terbaru.

## Tahap 3b-3: roll `r`, `+N`, geseran (`internal/ubot/roll.go`, `geseran.go`)
Logika mengikuti ubot.js (`handleRollMove`, `applyRoll`, `handlePlus`, geseran):
- `r` / `rol` / `roll` setelah `.rk`: pemain di sisi kurang = pik ditambah sebesar selisih; pemain di sisi berlebih = pik dikurangi
  (habis = keluar, sisa selisih masuk sisi kurang sebagai satu pik); pemain belum ada di list = masuk sisi kurang. Saldo pin dicek
  (`.svown` = bypass), anti-clone berlaku. Admin menekan Konfirm/Tolak pada kartu "via @bot"; kedaluwarsa otomatis (`ROLL_TTL_SEC`, bawaan 15 dtk).
- `+N` dari pemain yang sudah ada di list (mode `.perak`: `+2` = 2000): kartu Konfirm/Tolak, saldo pin dicek (pik lama + tambahan),
  saldo kurang = tanda `.` di `.list`. Kedaluwarsa `PLUS_TTL_SEC` (bawaan 25 dtk).
- Geseran: `.geseran <key> <nominal> <max user>`, `.geseran`, `.geseran del <key>`; pemain memicu dengan `#key b` / `#key k`.
  `geseran.json` sama dengan Node; hitungan terpakai direset `.rs` dan dipulihkan `.undors`.
- Admin yang boleh menekan tombol: akun userbot itu sendiri, `OWNER_ID`, dan sudo.
- Syarat: mode inline bot aktif (BotFather -> `/setinline`) dan `rolls.SetBotUsername` terisi saat bot jalan (sudah di `bot.go`).

Belum (menyusul): `said.json` ("pik 20"), `.p/.c`, `.sk/.sb`, `.tambah/.kurang`, `.h`, `.wd`, `.tf`, `.cr`, `.sv*`,
`.open/.close`, `.font`, `.setemoji/.eid*`, `.cmd/.help`, emoji premium di pesan userbot, `LIST_NEWMSG`.
