// tool_alias.go Kiro 64 字符工具名限制的运行时别名
// （KiroaaS extensions/tool_name_alias.py 的 Go 翻译）。
// 别名确定性生成（sha256 前缀），全局双向字典跨请求复用；
// 编码侧（工具定义/toolUses）注册，解码侧（响应工具名）还原。
package kiro

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
	"sync"
)

// MaxKiroToolNameLength Kiro 工具名长度上限。
const MaxKiroToolNameLength = 64

var kiroToolNameRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
var unsafeSuffixRe = regexp.MustCompile(`[^A-Za-z0-9_-]+`)

var aliasMu sync.RWMutex
var aliasToOriginal = map[string]string{}
var originalToAlias = map[string]string{}
var reservedKiroNames = map[string]bool{}

// NeedsAlias 工具名是否非法（超长或含非法字符）需别名。
func NeedsAlias(name string) bool {
	return !kiroToolNameRe.MatchString(name)
}

// buildAlias 确定性别名：t_{sha256[:digestLen]}_{安全化后缀}，总长不超上限。
func buildAlias(name string, digestLen int) string {
	sum := sha256.Sum256([]byte(name))
	digest := hex.EncodeToString(sum[:])[:digestLen]
	suffixSource := name
	if i := strings.LastIndex(name, "__"); i >= 0 {
		suffixSource = name[i+2:]
	}
	suffix := unsafeSuffixRe.ReplaceAllString(suffixSource, "_")
	suffix = strings.Trim(suffix, "_-")
	if suffix == "" {
		suffix = "tool"
	}
	prefix := "t_" + digest + "_"
	maxSuffix := MaxKiroToolNameLength - len(prefix)
	if len(suffix) > maxSuffix {
		suffix = suffix[len(suffix)-maxSuffix:]
	}
	return prefix + suffix
}

// AliasForToolName 取 Kiro 侧工具名（合法名原样；非法名生成确定性别名）。
func AliasForToolName(name string) string {
	if !NeedsAlias(name) {
		return name
	}
	aliasMu.RLock()
	if a, ok := originalToAlias[name]; ok {
		aliasMu.RUnlock()
		return a
	}
	aliasMu.RUnlock()

	aliasMu.Lock()
	defer aliasMu.Unlock()
	if a, ok := originalToAlias[name]; ok { // 双检
		return a
	}
	alias := buildAlias(name, 12)
	digestLen := 16
	// 防碰撞：撞上保留名或别人的别名则加长摘要
	for reservedKiroNames[alias] || (aliasToOriginal[alias] != "" && aliasToOriginal[alias] != name) {
		if digestLen > 64 {
			break // sha256 hex 上限，实际不可能到这
		}
		alias = buildAlias(name, digestLen)
		digestLen += 4
	}
	originalToAlias[name] = alias
	aliasToOriginal[alias] = name
	return alias
}

// OriginalForToolName 还原客户端侧工具名（未知名原样返回）。
func OriginalForToolName(name string) string {
	aliasMu.RLock()
	defer aliasMu.RUnlock()
	if orig, ok := aliasToOriginal[name]; ok {
		return orig
	}
	return name
}

// RegisterToolNames 登记一组工具名：合法名进保留集（防别名碰撞），非法名注册别名。
func RegisterToolNames(names []string) {
	if len(names) == 0 {
		return
	}
	for _, n := range names {
		if n != "" && !NeedsAlias(n) {
			aliasMu.Lock()
			reservedKiroNames[n] = true
			aliasMu.Unlock()
		}
	}
	for _, n := range names {
		if n != "" {
			AliasForToolName(n)
		}
	}
}

// ResetToolAliases 清空别名表（仅测试用）。
func ResetToolAliases() {
	aliasMu.Lock()
	defer aliasMu.Unlock()
	aliasToOriginal = map[string]string{}
	originalToAlias = map[string]string{}
	reservedKiroNames = map[string]bool{}
}
