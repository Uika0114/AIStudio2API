//go:build windows

package chromeauth

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
)

// We support closed Chrome only. Never ignore committed data in a live WAL.
func ensureNoPendingWAL(databasePath string) error {
	p := databasePath + "-wal"
	info, err := os.Lstat(p)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("检查 Chrome Web Data WAL 失败")
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("不支持 Chrome WAL 目录别名")
	}
	if err := rejectChromeReparsePath(p); err != nil {
		return err
	}
	if info.Size() != 0 {
		return fmt.Errorf("检测到 Chrome Web Data WAL；请先正常退出专用 Chrome 后重试")
	}
	return nil
}

func tokenDatabaseURI(databasePath string) string {
	p := (&url.URL{Path: filepath.ToSlash(databasePath)}).EscapedPath()
	return "file:" + p + "?mode=ro&immutable=1"
}

func tokenServiceHasMTLS(database *sql.DB) (bool, error) {
	rows, err := database.Query("PRAGMA table_info(token_service)")
	if err != nil {
		return false, fmt.Errorf("读取 Chrome token_service schema 失败")
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var id, notNull, primaryKey int
		var name, dataType string
		var defaultValue any
		if err := rows.Scan(&id, &name, &dataType, &notNull, &defaultValue, &primaryKey); err != nil {
			return false, fmt.Errorf("解析 Chrome token_service schema 失败")
		}
		if name == "mtls_token_binding" {
			found = true
		}
	}
	return found, rows.Err()
}
