// migrate 模式一→模式二一次性迁移 CLI：SQLite upstream.db 只读源 →
// 目标库（通常是 postgres）。目标数据源取 -driver/-dsn，缺省回落
// MODELSURGE_DB_DRIVER / MODELSURGE_UPSTREAM_DB_DSN 环境变量。
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/aceaura/ModelSurge/upstream/dialect"
	"github.com/aceaura/ModelSurge/upstream/upstreamstore"
)

func main() {
	source := flag.String("source", "", "mode-one SQLite upstream.db path (required)")
	driver := flag.String("driver", "", "target driver: sqlite|postgres (default $MODELSURGE_DB_DRIVER, else postgres)")
	dsn := flag.String("dsn", "", "target DSN (default $MODELSURGE_UPSTREAM_DB_DSN)")
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
		target = os.Getenv("MODELSURGE_UPSTREAM_DB_DSN")
	}
	if target == "" {
		log.Fatal("target dsn required: -dsn or MODELSURGE_UPSTREAM_DB_DSN")
	}

	store, err := upstreamstore.Open(d, target)
	if err != nil {
		log.Fatal(err)
	}
	defer store.Close()
	res, err := store.Migrate(context.Background(), *source)
	if err != nil {
		log.Fatalf("migrate: %v", err)
	}
	fmt.Printf("migrated %s -> %s: accounts=%d states=%d quota=%d reports=%d usage=%d\n",
		*source, d, res.Accounts, res.States, res.QuotaStates, res.Reports, res.Usage)
}
