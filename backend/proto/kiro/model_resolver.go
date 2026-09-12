// model_resolver.go 模型名规范化与四层解析管线
// （KiroaaS model_resolver.py 的 Go 翻译）。
// 原则：网关不是守门人——未知模型直通，由 Kiro API 终裁。
package kiro

import (
	"regexp"
	"sort"
	"strings"
)

// ModelResolution 解析结果。
type ModelResolution struct {
	InternalID      string // 发给 Kiro 的 ID
	Source          string // "alias" | "cache" | "hidden" | "passthrough"
	OriginalRequest string // 客户端原始请求
	Normalized      string // 规范化后的名字
	IsVerified      bool   // cache/hidden 命中为 true；直通为 false
}

var (
	// 上下文窗口后缀（客户端指示，非模型 ID）：[1m] / [200k]
	ctxWindowSuffix = regexp.MustCompile(`(?i)\[\d+[mk]\]$`)

	// 形态 1：claude-{family}-{major}-{minor}(-日期/-latest/-数字)?
	// minor 限 1-2 位，8 位日期不落入此组。
	standardPattern = regexp.MustCompile(`^(claude-(?:haiku|sonnet|opus)-\d+)-(\d{1,2})(?:-(?:\d{8}|latest|\d+))?$`)

	// 形态 2：claude-{family}-{major}(-8位日期)?
	noMinorPattern = regexp.MustCompile(`^(claude-(?:haiku|sonnet|opus)-\d+)(?:-\d{8})?$`)

	// 形态 3（旧版）：claude-{major}-{minor}-{family}(-后缀)?
	legacyPattern = regexp.MustCompile(`^(claude)-(\d+)-(\d+)-(haiku|sonnet|opus)(?:-(?:\d{8}|latest|\d+))?$`)

	// 形态 4：已带点号但拖日期后缀：claude-haiku-4.5-20251001
	dotWithDatePattern = regexp.MustCompile(`^(claude-(?:\d+\.\d+-)?(?:haiku|sonnet|opus)(?:-\d+\.\d+)?)-\d{8}$`)

	// 形态 5（倒置带后缀）：claude-4.5-opus-high -> claude-opus-4.5。
	// 必须带后缀，避免误吞已规范化的 claude-3.7-sonnet。
	invertedWithSuffix = regexp.MustCompile(`^claude-(\d+)\.(\d+)-(haiku|sonnet|opus)-(.+)$`)

	familyRe = regexp.MustCompile(`(haiku|sonnet|opus)`)
)

// NormalizeModelName 客户端模型名 -> Kiro 规范形态（横线小数点化、去日期后缀、
// 旧版家族重排、倒置形态修正）。未匹配的原样返回（保留大小写直通）。
func NormalizeModelName(name string) string {
	if name == "" {
		return name
	}
	name = ctxWindowSuffix.ReplaceAllString(name, "")
	lower := strings.ToLower(name)

	if m := standardPattern.FindStringSubmatch(lower); m != nil {
		return m[1] + "." + m[2] // claude-haiku-4-5 -> claude-haiku-4.5
	}
	if m := noMinorPattern.FindStringSubmatch(lower); m != nil {
		return m[1] // claude-sonnet-4-20250514 -> claude-sonnet-4
	}
	if m := legacyPattern.FindStringSubmatch(lower); m != nil {
		return m[1] + "-" + m[2] + "." + m[3] + "-" + m[4] // claude-3-7-sonnet -> claude-3.7-sonnet
	}
	if m := dotWithDatePattern.FindStringSubmatch(lower); m != nil {
		return m[1] // claude-haiku-4.5-20251001 -> claude-haiku-4.5
	}
	if m := invertedWithSuffix.FindStringSubmatch(lower); m != nil {
		return "claude-" + m[3] + "-" + m[1] + "." + m[2] // claude-4.5-opus-high -> claude-opus-4.5
	}
	return name
}

// ModelCache 动态模型缓存的只读视图（account.ModelInfoCache 实现此接口，
// 避免 kiro 包反向依赖 account）。
type ModelCache interface {
	// IsValid 劤断模型是否在动态缓存中（已含隐藏模型）。
	IsValid(modelID string) bool
	// AllModelIDs 缓存中的全部模型 ID。
	AllModelIDs() []string
}

// ModelResolver 四层解析管线：别名 -> 规范化 -> 动态缓存/隐藏模型 -> 直通。
// resolve 永不报错——未知模型交给 Kiro 裁决。
type ModelResolver struct {
	cache         ModelCache
	hiddenModels  map[string]string
	aliases       map[string]string
	hiddenFromSet map[string]bool
}

// NewModelResolver 构造（cache 可为 nil：跳过缓存层）。
func NewModelResolver(cache ModelCache, hiddenModels, aliases map[string]string, hiddenFromList []string) *ModelResolver {
	if hiddenModels == nil {
		hiddenModels = HIDDEN_MODELS
	}
	if aliases == nil {
		aliases = MODEL_ALIASES
	}
	hf := map[string]bool{}
	for _, m := range hiddenFromList {
		hf[m] = true
	}
	return &ModelResolver{cache: cache, hiddenModels: hiddenModels, aliases: aliases, hiddenFromSet: hf}
}

// Resolve 解析外部模型名。四层：别名 -> 规范化 -> 缓存 -> 隐藏 -> 直通。
func (r *ModelResolver) Resolve(externalModel string) ModelResolution {
	// 层 0：别名
	resolved := externalModel
	if v, ok := r.aliases[externalModel]; ok {
		resolved = v
	}
	// 层 1：规范化
	normalized := NormalizeModelName(resolved)

	// 层 2：动态缓存
	if r.cache != nil && r.cache.IsValid(normalized) {
		return ModelResolution{
			InternalID: normalized, Source: "cache",
			OriginalRequest: externalModel, Normalized: normalized, IsVerified: true,
		}
	}
	// 层 3：隐藏模型（展示名 -> 内部 ID）
	if internal, ok := r.hiddenModels[normalized]; ok {
		return ModelResolution{
			InternalID: internal, Source: "hidden",
			OriginalRequest: externalModel, Normalized: normalized, IsVerified: true,
		}
	}
	// 层 4：直通——网关不是守门人
	return ModelResolution{
		InternalID: normalized, Source: "passthrough",
		OriginalRequest: externalModel, Normalized: normalized, IsVerified: false,
	}
}

// AvailableModels /v1/models 展示列表：缓存 ∪ 隐藏模型展示名 ∪ 别名 - 隐藏项。
func (r *ModelResolver) AvailableModels() []string {
	models := map[string]bool{}
	if r.cache != nil {
		for _, m := range r.cache.AllModelIDs() {
			models[m] = true
		}
	}
	for m := range r.hiddenModels {
		models[m] = true
	}
	for m := range r.hiddenFromSet {
		delete(models, m)
	}
	for a := range r.aliases {
		models[a] = true
	}
	out := make([]string, 0, len(models))
	for m := range models {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

// ExtractModelFamily 提取 Claude 家族名（haiku/sonnet/opus）；非 Claude 返回空。
func ExtractModelFamily(modelName string) string {
	if m := familyRe.FindStringSubmatch(strings.ToLower(modelName)); m != nil {
		return m[1]
	}
	return ""
}
