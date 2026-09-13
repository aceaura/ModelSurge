// kiro_inline.go kiro 凭据内联载体（creds_text/creds_b64）：三种 source 的
// 路径形态等价替代。内容按 source 解释，加载走与路径形态相同的解析链；
// 轮转不回写内联字段（token_state 列兜底持久化）。
// 载体三选一：路径字段 XOR creds_text XOR creds_b64（管理面校验保证）。
package account

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
)

const (
	// maxInlineCredsBytes 内联凭据解码后上限（超大 SQLite 库不该走文本区录入）。
	maxInlineCredsBytes = 8 << 20
	// sqliteMagic SQLite 文件头魔数（cli_db b64 形态识别）。
	sqliteMagic = "SQLite format 3\x00"
)

// ValidateKiroCreds kiro 凭据载体校验（管理面建号/更新共用）：三选一互斥 +
// 内联内容形态预检（只验形态不做网络探测）。错误文案以 "kiro.<field>: " 开头，
// 不回显内容明文。
func ValidateKiroCreds(k *KiroAccount) error {
	var path, pathField string
	switch k.Source {
	case SourceRefreshToken:
		path, pathField = k.RefreshToken, "kiro.refresh_token"
	case SourceCredsFile:
		path, pathField = k.CredsFile, "kiro.creds_file"
	case SourceCliDB:
		path, pathField = k.CliDB, "kiro.cli_db"
	default:
		return errors.New("kiro.source: must be one of refresh_token/creds_file/cli_db")
	}
	var carriers []string
	if path != "" {
		carriers = append(carriers, pathField)
	}
	if k.CredsText != "" {
		carriers = append(carriers, "kiro.creds_text")
	}
	if k.CredsB64 != "" {
		carriers = append(carriers, "kiro.creds_b64")
	}
	switch {
	case len(carriers) == 0:
		return fmt.Errorf("%s: required when source is %s (path or inline creds_text/creds_b64)", pathField, k.Source)
	case len(carriers) > 1:
		return fmt.Errorf("kiro: credentials carrier conflict: %s (choose one)", strings.Join(carriers, " + "))
	}
	if path != "" {
		return nil // 路径形态：内容校验交由认证层加载时进行
	}

	carrierField := "kiro.creds_text"
	if k.CredsText == "" {
		carrierField = "kiro.creds_b64"
	}
	b, err := inlineContent(k)
	if err != nil {
		return fmt.Errorf("%s: %v", carrierField, err)
	}
	switch k.Source {
	case SourceRefreshToken:
		trimmed := strings.TrimSpace(string(b))
		if trimmed == "" {
			return fmt.Errorf("%s: refresh token must be non-empty", carrierField)
		}
		if strings.HasPrefix(trimmed, "{") {
			var probe struct {
				RefreshToken string `json:"refreshToken"`
			}
			if err := json.Unmarshal(b, &probe); err != nil || probe.RefreshToken == "" {
				return fmt.Errorf(`%s: JSON missing "refreshToken" key`, carrierField)
			}
		}
	case SourceCredsFile:
		if !json.Valid(b) {
			return fmt.Errorf("%s: not valid JSON (credentials.json content expected)", carrierField)
		}
	case SourceCliDB:
		if k.CredsText != "" {
			if !json.Valid(b) {
				return fmt.Errorf("%s: not valid JSON (extracted credentials JSON expected)", carrierField)
			}
			return nil
		}
		if !bytes.HasPrefix(b, []byte(sqliteMagic)) {
			return fmt.Errorf("%s: not a SQLite database (magic mismatch)", carrierField)
		}
	}
	return nil
}

// inlineContent 内联载体内容（调用前已互斥校验：text 与 b64 至多一非空）。
func inlineContent(k *KiroAccount) ([]byte, error) {
	if k.CredsText != "" {
		return []byte(k.CredsText), nil
	}
	return decodeInlineB64(k.CredsB64)
}

// decodeInlineB64 剥空白（文本区粘贴常见换行/空格）后 StdEncoding 解码，
// 超限拒绝。
func decodeInlineB64(s string) ([]byte, error) {
	clean := strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\n', '\r', '\t':
			return -1
		}
		return r
	}, s)
	if clean == "" {
		return nil, errors.New("empty base64")
	}
	b, err := base64.StdEncoding.DecodeString(clean)
	if err != nil {
		return nil, fmt.Errorf("invalid base64: %v", err)
	}
	if len(b) > maxInlineCredsBytes {
		return nil, fmt.Errorf("decoded content exceeds %d MB", maxInlineCredsBytes>>20)
	}
	return b, nil
}

// loadInline 内联载体加载（loadCredentials 分派用）：存在内联载体时按
// source 解释内容并填充 token 字段，返回 true；无内联载体返回 false 由
// 调用方走路径形态。三选一互斥由管理面校验保证，此处 text 优先。
func (s *AuthService) loadInline() bool {
	if s.k.CredsText != "" {
		switch s.k.Source {
		case SourceRefreshToken:
			s.applyInlineRefreshToken([]byte(s.k.CredsText))
		default: // creds_file 原文 / cli_db 提取 JSON：同一 camelCase 解析链
			if err := s.applyCredsJSON([]byte(s.k.CredsText)); err != nil {
				log.Printf("kiro auth %s: inline creds parse: %v", s.name, err)
			}
		}
		return true
	}
	if s.k.CredsB64 != "" {
		b, err := decodeInlineB64(s.k.CredsB64)
		if err != nil {
			log.Printf("kiro auth %s: inline creds decode: %v", s.name, err)
			return true
		}
		switch s.k.Source {
		case SourceRefreshToken:
			s.applyInlineRefreshToken(b)
		case SourceCliDB:
			s.loadCliDBBytes(b)
		default:
			if err := s.applyCredsJSON(b); err != nil {
				log.Printf("kiro auth %s: inline creds parse: %v", s.name, err)
			}
		}
		return true
	}
	return false
}

// applyInlineRefreshToken refresh_token 内联内容：裸串直配或
// {"refreshToken":...} JSON（自动识别）。
func (s *AuthService) applyInlineRefreshToken(b []byte) {
	trimmed := strings.TrimSpace(string(b))
	if strings.HasPrefix(trimmed, "{") {
		var f credsFileJSON
		if err := json.Unmarshal(b, &f); err != nil || f.RefreshToken == "" {
			log.Printf("kiro auth %s: inline refresh token json missing refreshToken", s.name)
			return
		}
		if s.token.RefreshToken == "" {
			s.token.RefreshToken = f.RefreshToken
		}
		return
	}
	if s.token.RefreshToken == "" {
		s.token.RefreshToken = trimmed
	}
}
