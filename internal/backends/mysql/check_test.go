package mysql

import (
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	_ "github.com/go-sql-driver/mysql"

	"github.com/girimi/unredo/internal/config"
)

func TestDSNTimezone(t *testing.T) {
	// The test depends on the env var UNREDO_EXECUTOR_PASSWORD; if
	// it's missing we skip rather than fail.
	if os.Getenv("UNREDO_EXECUTOR_PASSWORD") == "" {
		t.Skip("UNREDO_EXECUTOR_PASSWORD not set")
	}
	tz, now, utc, err := readSessionTimeZone("127.0.0.1:3306", "unredo_executor")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("session time_zone: %s, NOW: %s, UTC: %s", tz, now, utc)
}

func readSessionTimeZone(addr, user string) (string, string, string, error) {
	cfg := mysql.NewConfig()
	cfg.User = user
	cfg.Passwd = os.Getenv("UNREDO_EXECUTOR_PASSWORD")
	cfg.Net = "tcp"
	cfg.Addr = addr
	cfg.ParseTime = false
	cfg.Loc = time.UTC
	cfg.InterpolateParams = false
	cfg.Params = map[string]string{
		"time_zone": "'+00:00'",
	}
	db, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		return "", "", "", err
	}
	defer db.Close()
	var tz, now, utc string
	if err := db.QueryRow("SELECT @@session.time_zone, NOW(), UTC_TIMESTAMP()").Scan(&tz, &now, &utc); err != nil {
		return "", "", "", err
	}
	return tz, now, utc, nil
}

// silence unused
var _ = fmt.Sprintf
var _ = config.Policy{}
