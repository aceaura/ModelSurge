package ir

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// Request.Clone 走 JSON 往返，而每个 EncodeRequest 的第一句都是 Clone：
// nil 的 json.RawMessage 会被序列化成字面 null，解回来是 4 字节的非空 "null"，
// 「客户端没发这个字段」就此变成「客户端发了个 null」并直接上 wire——工具参数
// 变 arguments:"null"，工具定义变 input_schema:null 被上游 400 拒整轮。
// 下面两组测试一组钉行为（Clone 前后 marshal 字节必须一致），一组钉结构
// （可达图里每个 RawMessage 字段必须带 omitempty，新增字段漏标签当场红）。

// rawNilFixture 覆盖 Clone 可达图里全部未建模为具体类型的原文槽位，且全部
// 保持 nil——正是客户端「没发这个字段」的常态形状。
func rawNilFixture() *Request {
	return &Request{
		Model:     "m",
		MaxTokens: 16,
		Messages: []Message{{
			Role: RoleUser,
			Content: []Block{
				{Type: BlockText, Text: "hi"},
				{Type: BlockToolUse, ToolUse: &ToolUse{ID: "t1", Name: "f"}},
				{Type: BlockServerToolUse, ServerToolUse: &ServerToolUse{ID: "s1", Name: "web_search"}},
				{Type: BlockOpaque, Opaque: &Opaque{WireType: "web_fetch_tool_result", From: "anthropic"}},
				{Type: BlockText, Text: "cite", Citations: []Citation{{CitedText: "晴", WireType: "char_location"}}},
			},
		}},
		Tools: []Tool{
			{Name: "f", Description: "d"},
			{Name: "g", Kind: ToolCustom},
		},
		ResponseFormat:     &ResponseFormat{Name: "n"},
		Prompt:             &PromptRef{ID: "p", Version: "1"},
		Thinking:           &ThinkingConfig{Enabled: true},
		Moderation:         nil,
		PromptCacheOptions: nil,
		Prediction:         nil,
		WebSearchOptions:   nil,
	}
}

func TestCloneDoesNotFabricateNullRawFields(t *testing.T) {
	req := rawNilFixture()
	before, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal 原请求失败: %v", err)
	}
	clone := req.Clone()
	after, err := json.Marshal(clone)
	if err != nil {
		t.Fatalf("marshal 克隆失败: %v", err)
	}
	if string(before) != string(after) {
		t.Errorf("Clone 改变了请求的 JSON 形状（nil 原文槽位被伪造成 null）：\n前 %s\n后 %s", before, after)
	}
	// 只查原文槽位：指针/切片字段本来就会 marshal 成 null 再解回 nil，那是
	// 无损往返，不是伪造。伪造的特征是「没发的字段变成一个非空的 null 值」。
	for _, key := range []string{
		`"Input":null`, `"InputSchema":null`, `"Body":null`, `"Schema":null`,
		`"Variables":null`, `"Format":null`, `"Raw":null`, `"Context":null`,
		`"Mode":null`, `"Moderation":null`, `"Prediction":null`,
	} {
		if strings.Contains(string(after), key) {
			t.Errorf("克隆后出现伪造的 %s：%s", key, after)
		}
	}
	// 逐字段复核：伪造值不一定体现在顶层 marshal 上（omitempty 会把它藏起来），
	// 编码器读的是字段本身。
	if clone.Messages[0].Content[1].ToolUse.Input != nil {
		t.Errorf("ToolUse.Input 被伪造成 %s", clone.Messages[0].Content[1].ToolUse.Input)
	}
	if clone.Messages[0].Content[2].ServerToolUse.Input != nil {
		t.Errorf("ServerToolUse.Input 被伪造成 %s", clone.Messages[0].Content[2].ServerToolUse.Input)
	}
	if clone.Messages[0].Content[3].Opaque.Body != nil {
		t.Errorf("Opaque.Body 被伪造成 %s", clone.Messages[0].Content[3].Opaque.Body)
	}
	if clone.Messages[0].Content[4].Citations[0].Raw != nil {
		t.Errorf("Citation.Raw 被伪造成 %s", clone.Messages[0].Content[4].Citations[0].Raw)
	}
	if clone.Tools[0].InputSchema != nil {
		t.Errorf("Tool.InputSchema 被伪造成 %s", clone.Tools[0].InputSchema)
	}
	if clone.Tools[1].Format != nil {
		t.Errorf("Tool.Format 被伪造成 %s", clone.Tools[1].Format)
	}
	if clone.Tools[0].InputExamples != nil {
		t.Errorf("Tool.InputExamples 被伪造成 %v", clone.Tools[0].InputExamples)
	}
	if clone.ResponseFormat.Schema != nil {
		t.Errorf("ResponseFormat.Schema 被伪造成 %s", clone.ResponseFormat.Schema)
	}
	if clone.Prompt.Variables != nil {
		t.Errorf("PromptRef.Variables 被伪造成 %s", clone.Prompt.Variables)
	}
	if clone.Thinking.Context != nil || clone.Thinking.Mode != nil {
		t.Errorf("Thinking.Context/Mode 被伪造成 %s / %s", clone.Thinking.Context, clone.Thinking.Mode)
	}
}

// 反向：非空原文必须逐字节活着穿过 Clone，否则修 nil 就成了丢内容。
func TestClonePreservesNonEmptyRawFields(t *testing.T) {
	const (
		schema = `{"type":"object","properties":{"a":{"type":"string"}}}`
		args   = `{"a":"b"}`
		vars   = `{"who":"world"}`
	)
	req := &Request{
		Model:     "m",
		MaxTokens: 16,
		Messages: []Message{{Role: RoleAssistant, Content: []Block{
			{Type: BlockToolUse, ToolUse: &ToolUse{ID: "t1", Name: "f", Input: json.RawMessage(args)}},
			{Type: BlockServerToolUse, ServerToolUse: &ServerToolUse{ID: "s1", Name: "web_search", Input: json.RawMessage(args)}},
			{Type: BlockOpaque, Opaque: &Opaque{WireType: "w", Body: json.RawMessage(args), From: "anthropic"}},
		}}},
		Tools: []Tool{{
			Name:          "f",
			InputSchema:   json.RawMessage(schema),
			Format:        json.RawMessage(`{"type":"text"}`),
			InputExamples: []json.RawMessage{json.RawMessage(args)},
		}},
		ResponseFormat: &ResponseFormat{Name: "n", Schema: json.RawMessage(schema)},
		Prompt:         &PromptRef{ID: "p", Version: "1", Variables: json.RawMessage(vars)},
	}
	clone := req.Clone()
	if clone == req {
		t.Fatal("Clone 返回了原指针（marshal 失败会走这条路），深拷贝没发生")
	}
	b := clone.Messages[0].Content
	for _, c := range []struct{ name, got, want string }{
		{"ToolUse.Input", string(b[0].ToolUse.Input), args},
		{"ServerToolUse.Input", string(b[1].ServerToolUse.Input), args},
		{"Opaque.Body", string(b[2].Opaque.Body), args},
		{"Tool.InputSchema", string(clone.Tools[0].InputSchema), schema},
		{"Tool.Format", string(clone.Tools[0].Format), `{"type":"text"}`},
		{"Tool.InputExamples[0]", string(clone.Tools[0].InputExamples[0]), args},
		{"ResponseFormat.Schema", string(clone.ResponseFormat.Schema), schema},
		{"PromptRef.Variables", string(clone.Prompt.Variables), vars},
	} {
		if c.got != c.want {
			t.Errorf("%s = %s, want %s", c.name, c.got, c.want)
		}
	}
	// 深拷贝：改克隆不能回写原请求。
	clone.Tools[0].InputSchema[0] = 'X'
	if req.Tools[0].InputSchema[0] == 'X' {
		t.Error("Clone 与原请求共享底层数组")
	}
}

// 客户端显式发来的 null 不是「没发」：它是 wire 上的 4 字节字面量，Clone 后
// 必须还在，好让上游按自己的口径报错，而不是被我们悄悄吞掉。
func TestCloneKeepsExplicitNullDistinctFromAbsent(t *testing.T) {
	withNull := &Request{Model: "m", Tools: []Tool{{Name: "f", InputSchema: json.RawMessage(`null`)}}}
	absent := &Request{Model: "m", Tools: []Tool{{Name: "f"}}}

	gotNull := withNull.Clone().Tools[0].InputSchema
	if string(gotNull) != "null" {
		t.Errorf("显式 null 被改写为 %s", gotNull)
	}
	if gotAbsent := absent.Clone().Tools[0].InputSchema; gotAbsent != nil {
		t.Errorf("缺省被伪造成 %s", gotAbsent)
	}
	if reflect.DeepEqual(gotNull, absent.Clone().Tools[0].InputSchema) {
		t.Error("Clone 之后「显式 null」与「没发」不可区分")
	}
	// 这条区分是 ResponseFormat.IsSchema 里 `!= "null"` 那半句存在的前提。
	if (&ResponseFormat{Schema: json.RawMessage(`null`)}).IsSchema() {
		t.Error("显式 null 的 schema 被判成有 schema")
	}
}

// ---- 结构不变量：可达图里每个 json.RawMessage 字段都必须带 omitempty ----

var rawMessageType = reflect.TypeOf(json.RawMessage(nil))

const irPkgPath = "github.com/aceaura/ModelSurge/agent/ir"

func collectRawFields(t reflect.Type, path string, ancestors []reflect.Type, found map[string]bool) {
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Slice, reflect.Array:
		collectRawFields(t.Elem(), path+"[]", ancestors, found)
		return
	case reflect.Map:
		collectRawFields(t.Elem(), path+"{}", ancestors, found)
		return
	case reflect.Struct:
	default:
		return
	}
	if t.PkgPath() != irPkgPath {
		return
	}
	for _, a := range ancestors {
		if a == t {
			return
		}
	}
	ancestors = append(ancestors, t)
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" {
			continue
		}
		fp := path + "." + f.Name
		ft := f.Type
		for ft.Kind() == reflect.Ptr {
			ft = ft.Elem()
		}
		if ft == rawMessageType {
			found[fp] = hasOmitEmpty(f.Tag.Get("json"))
			continue
		}
		if ft.Kind() == reflect.Slice && ft.Elem() == rawMessageType {
			// 切片形态本身不会伪造（nil 切片 marshal 成 null、解回还是 nil），这条
			// 是一致性要求而不是安全要求：Clone 可达图里的原文槽位统一不带 null
			// 噪声，后来人不必逐个判断哪种形态需要标签。
			found[fp] = hasOmitEmpty(f.Tag.Get("json"))
			continue
		}
		collectRawFields(f.Type, fp, ancestors, found)
	}
}

func hasOmitEmpty(tag string) bool {
	for _, opt := range strings.Split(tag, ",")[1:] {
		if opt == "omitempty" {
			return true
		}
	}
	return false
}

func TestEveryRawMessageFieldInIRGraphHasOmitEmpty(t *testing.T) {
	found := map[string]bool{}
	for _, root := range []any{Request{}, Response{}, Event{}} {
		rt := reflect.TypeOf(root)
		collectRawFields(rt, rt.Name(), nil, found)
	}
	if len(found) == 0 {
		t.Fatal("反射 walker 一个 RawMessage 字段都没走到，测试是空转的")
	}
	// walker 必须真的走到已知槽位，否则漏标签的字段也可能因为没被访问而「通过」。
	for _, want := range []string{
		".Opaque.Body",
		".ServerToolUse.Input",
		".ToolUse.Input",
		".InputSchema",
		".Format",
		".InputExamples",
		".ResponseFormat.Schema",
		".Prompt.Variables",
		".Citations[].Raw",
		".Thinking.Context",
	} {
		ok := false
		for path := range found {
			if strings.HasSuffix(path, want) {
				ok = true
				break
			}
		}
		if !ok {
			t.Errorf("walker 没走到已知槽位 %s（覆盖不全）", want)
		}
	}
	for path, ok := range found {
		if !ok {
			t.Errorf("%s 是 json.RawMessage 但缺 json:\",omitempty\"——Clone 会把它从 nil 伪造成字面 null 并送上出站 wire", path)
		}
	}
}
