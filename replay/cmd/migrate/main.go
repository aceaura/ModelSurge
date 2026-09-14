// migrate 模式一→模式二一次性迁移 CLI：SQLite replay.db 只读源 →
// 目标库（通常是 postgres）。目标数据源取 -driver/-dsn，缺省回落
// MODELSURGE_DB_DRIVER / MODELSURGE_REPLAY_DB_DSN 环境变量。
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/aceaura/ModelSurge/replay/relaystore"
	"github.com/aceaura/ModelSurge/upstream/dialect"
)

func main() {
	source := flag.String("source", "", "mode-one SQLite replay.db path (required)")
	driver := flag.String("driver", "", "target driver: sqlite|postgres (default $MODELSURGE_DB_DRIVER, else postgres)")
	dsn := flag.String("dsn", "", "target DSN (default $MODELSURGE_REPLAY_DB_DSN)")
	flag.Parse()

	if *source == "" {
		flag.Usage()
		os.Exit(2)
	}
	d := *driver
	if d == "" {
		d = os.Getenv("MODELSURGE_DB_DRIVER")
	}
	if d == "" {
		d = dialect.Postgres
	}
	if !dialect.Valid(d) {
		log.Fatalf("unsupported driver %q", d)
	}
	target := *dsn
	if target == "" {
		target = os.Getenv("MODELSURGE_REPLAY_DB_DSN")
	}
	if target == "" {
		log.Fatal("target dsn required: -dsn or MODELSURGE_REPLAY_DB_DSN")
	}

	store, err := relaystore.Open(d, target)
	if err != nil {
		log.Fatal(err)
	}
	defer store.Close()
	res, err := store.Migrate(context.Background(), *source)
	if err != nil {
		log.Fatalf("migrate: %v", err)
	}
	fmt.Printf("migrated %s -> %s: user_models=%d groups=%d members=%d reports=%d\n",
		*source, d, res.UserModels, res.Groups, res.Members, res.Reports)
}
