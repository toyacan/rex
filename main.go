// Port Node.js -> Go. Tahap 1: fondasi. Tahap 2: bot utama + /buy QRIS + inline. Tahap 3a: kerangka userbot gotd. Tahap 3b-2: pencatatan pik + .on/.list/.rk dst. Tahap 3b-3: roll r, +N, geseran.
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"cu/internal/bot"
	"cu/internal/buy"
	"cu/internal/config"
	"cu/internal/manager"
	"cu/internal/secure"
	"cu/internal/slot"
	"cu/internal/tg"
	"cu/internal/ubot"
	"cu/internal/util"
)

func main() {
	if err := config.LoadDotenv(".env"); err != nil {
		log.Printf("catatan: .env tidak dimuat (%v), memakai variabel lingkungan", err)
	}
	token := config.String("BOT_TOKEN", "")
	if token == "" {
		log.Fatal("Isi BOT_TOKEN (dan OWNER_ID, API_ID, API_HASH) di .env")
	}
	const dataDir = "data"
	ownerID := config.String("OWNER_ID", "")

	var prem *tg.Premium
	if config.String("PREMIUM_EMOJI", "") != "0" {
		prem = tg.NewPremium(tg.DefaultEmoji)
	}
	if prem != nil {
		log.Printf("Emoji premium aktif (%d emoji).", prem.Count())
	} else {
		log.Println("Emoji premium: nonaktif (ID belum diisi atau PREMIUM_EMOJI=0)")
	}
	api := tg.New(token, prem)

	trialDays := config.Int("TRIAL_DAYS", 1)
	purgeDays := config.Float("PURGE_EXPIRED_DAYS", 30)
	// Pemain tetap .kk (KK_K / KK_B = "NAMA JUMLAH") dan tambahan di akhir tiap baris .list (LIST_SUFFIX)
	kkK := slot.ParseKK(config.String("KK_K", ""), "ALBOTZ 15")
	kkB := slot.ParseKK(config.String("KK_B", ""), "KWINGZ 15")
	listSuffix := "P"
	if v, ok := os.LookupEnv("LIST_SUFFIX"); ok {
		listSuffix = v // boleh kosong
	}
	ubCfg := ubot.Config{
		KK:         slot.KKCfg{K: &kkK, B: &kkB},
		ListSuffix: listSuffix,
		AppID:   config.Int("API_ID", 0),
		AppHash: config.String("API_HASH", ""),
		DataDir: dataDir,
		Sealer:  secure.New(dataDir, config.String("SESSION_KEY", "")),
	}
	mgr, err := manager.New(manager.Config{
		DataDir:    dataDir,
		OwnerID:    ownerID,
		TrialDays:  trialDays,
		RemindIn:   time.Duration(config.Float("REMIND_HOURS", 2) * float64(time.Hour)),
		PurgeAfter: time.Duration(purgeDays * float64(24*time.Hour)),
	}, ubot.NewFactory(ubCfg))
	if err != nil {
		log.Fatal(err)
	}

	pay, err := buy.New(buy.ConfigFromEnv(dataDir, util.ID(ownerID)), api, mgr)
	if err != nil {
		log.Fatal(err)
	}
	if !pay.Enabled() {
		log.Println("Info: KASERA_API_KEY belum diisi, /buy dimatikan (alur manual buy-manual.js belum diport).")
	}

	b := bot.New(bot.Config{
		OwnerID: ownerID, TrialDays: trialDays, DataDir: dataDir, PurgeDays: purgeDays,
	}, api, mgr, pay, func() bot.LoginFlow { return ubot.NewLogin(ubCfg) })

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	mgr.LoadAll(ctx)
	mgr.StartWatcher(ctx, b.Watcher())
	pay.Start(ctx)
	b.StartHousekeeping(ctx)
	b.Run(ctx) // blok sampai Ctrl+C / SIGTERM
	log.Println("berhenti")
}
