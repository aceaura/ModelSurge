package proto_test

import (
	"strings"
	"testing"

	"github.com/aceaura/ModelSurge/agent/ir"
	"github.com/aceaura/ModelSurge/agent/normalize"
	"github.com/aceaura/ModelSurge/agent/proto"
	_ "github.com/aceaura/ModelSurge/agent/proto/anthropic"
	_ "github.com/aceaura/ModelSurge/agent/proto/gemini"
	_ "github.com/aceaura/ModelSurge/agent/proto/openaichat"
	_ "github.com/aceaura/ModelSurge/agent/proto/openairesponses"
)

// R99：图片部件的三类保真缺口，都是实测出来的（探针日志 r99_probe.log /
// r99_probe2.log），不是照参考仓臆测：
//
//  1. image_url 的另一种线体形态解码失败，整条消息被打成占位文本。
//     Chat 侧只认对象 {"url":…}，客户端发裸字符串 "https://…" 时整个 part 的
//     json.Unmarshal 失败被 continue 丢掉；Responses 侧只认裸字符串，客户端发
//     Chat 形态的对象时同样整块丢。若它是消息里唯一的部件，normalize 随后把整条
//     消息改写成 "(empty)"——图片连同所在消息一起消失，上游只看到一句占位文本，
//     客户端拿不到任何注记。两种形态都是真实存在的：new-api 把 image_url 声明成
//     any 再按 string / map 两路取值，sub2api 的 responses 桥同样 switch 两种类型。
//
//  2. detail（分辨率档位）在两族之间静默丢失，连同族往返也丢。
//     这一维直接决定上游怎么切图、进而决定输入 token 计费：low 固定 85 token，
//     high 按原图分块，量级差一个数量级。丢掉之后上游一律按自己的默认档处理，
//     客户端指定的成本控制在账单上看得出、在请求里看不出。
//
//  3. 没有载荷的图片仍被编成图片部件，写出一个上游必 400 的形状。
//     三个载体（base64 / URL / file_id）全空时，anthropic 编出
//     {"type":"image","source":{"type":"base64"}}（官方 media_type 与 data 都是
//     Required），chat 编出 {"url":""}，responses 编出连 image_url 键都没有的
//     {"type":"input_image"}。触发形态实测有两种：Responses 的 input_image 只给了
//     file_id，以及客户端用了本层没建模的键名。报错点还落在这三种形状上，读者
//     看不出是哪一段输入害的。
//
// 修法：ir.Image 增 Detail 与 FileID 两维；两族解码器兼容 image_url 的双形态；
// 编码侧无载荷即整个部件跳过，消息因此变空时落约定占位（三族同口径）；丢失由
// relay.Diagnose 按新增的 ImageDetail / ImageFileRef 两个能力位报出。

// r99Outbound agent 侧的四个出站（codex 与 openai-responses 同形，各自独立断言）。
var r99Outbound = []string{"anthropic", "codex", "openai-chat", "openai-responses"}

func r99ChatReq(t *testing.T, parts ...string) *ir.Request {
	t.Helper()
	body := `{"model":"m","messages":[{"role":"user","content":[` + strings.Join(parts, ",") + `]}]}`
	req, err := proto.MustInbound("openai-chat").DecodeRequest([]byte(body))
	if err != nil {
		t.Fatalf("chat DecodeRequest: %v", err)
	}
	return req
}

func r99RespReq(t *testing.T, parts ...string) *ir.Request {
	t.Helper()
	body := `{"model":"m","input":[{"role":"user","content":[` + strings.Join(parts, ",") + `]}]}`
	req, err := proto.MustInbound("openai-responses").DecodeRequest([]byte(body))
	if err != nil {
		t.Fatalf("responses DecodeRequest: %v", err)
	}
	return req
}

func r99Encode(t *testing.T, outbound string, req *ir.Request) string {
	t.Helper()
	body, err := proto.MustOutbound(outbound).EncodeRequest(req)
	if err != nil {
		t.Fatalf("%s EncodeRequest: %v", outbound, err)
	}
	return string(body)
}

func r99EncodeAll(t *testing.T, req *ir.Request) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, name := range r99Outbound {
		out[name] = r99Encode(t, name, req)
		t.Logf("%-16s %s", name, out[name])
	}
	return out
}

// r99AssertAbsent 断言四个出站的线上都不含 substr，并给出统一的失败措辞。
func r99AssertAbsent(t *testing.T, out map[string]string, substr, why string) {
	t.Helper()
	for _, name := range r99Outbound {
		if strings.Contains(out[name], substr) {
			t.Errorf("%s 出站出现 %s（%s）：%s", name, substr, why, out[name])
		}
	}
}

// ---- 1. image_url 双形态 ----

// 裸字符串形态此前让整个 part 解析失败被丢掉；它是消息里唯一的部件，于是
// normalize 把整条消息改写成占位文本。四个出站都必须真的拿到这张图。
func TestChatImageURLStringFormReachesEveryOutbound(t *testing.T) {
	req := r99ChatReq(t, `{"type":"image_url","image_url":"https://example.com/a.png"}`)
	img := r99SoleImage(t, req)
	if img.URL != "https://example.com/a.png" {
		t.Fatalf("字符串形态没解出 URL：Image=%+v", img)
	}
	out := r99EncodeAll(t, req)
	r99AssertAbsent(t, out, normalize.Placeholder, "图片已解出，不该退化成占位消息")
	for name, body := range out {
		if !strings.Contains(body, "https://example.com/a.png") {
			t.Errorf("%s 出站丢了图片地址：%s", name, body)
		}
	}
	if !strings.Contains(out["openai-chat"], `{"url":"https://example.com/a.png"}`) {
		t.Errorf("chat 出站没写回对象形态：%s", out["openai-chat"])
	}
	// Responses 一族的图片槽位只认裸字符串，不能把 Chat 的对象形态带过去。
	if !strings.Contains(out["openai-responses"], `"image_url":"https://example.com/a.png"`) {
		t.Errorf("responses 出站没写回裸字符串形态：%s", out["openai-responses"])
	}
}

// 对象形态是 Chat 的规范线体，字符串形态修好后不得回退。
func TestChatImageURLObjectFormStillDecodes(t *testing.T) {
	req := r99ChatReq(t, `{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}`)
	if got := r99SoleImage(t, req).URL; got != "https://example.com/a.png" {
		t.Fatalf("对象形态解出的 URL = %q", got)
	}
}

// Responses 侧的镜像缺口：本族规范是裸字符串，客户端发 Chat 形态的对象时
// 整个 part 解析失败，整条消息同样被打成占位文本。
func TestResponsesImageURLObjectFormReachesEveryOutbound(t *testing.T) {
	req := r99RespReq(t, `{"type":"input_image","image_url":{"url":"https://example.com/a.png"}}`)
	img := r99SoleImage(t, req)
	if img.URL != "https://example.com/a.png" {
		t.Fatalf("嵌套对象形态没解出 URL：Image=%+v", img)
	}
	out := r99EncodeAll(t, req)
	r99AssertAbsent(t, out, normalize.Placeholder, "图片已解出，不该退化成占位消息")
	for name, body := range out {
		if !strings.Contains(body, "https://example.com/a.png") {
			t.Errorf("%s 出站丢了图片地址：%s", name, body)
		}
	}
}

// 裸字符串是 Responses 的规范形态，对象形态修好后不得回退。
func TestResponsesImageURLStringFormStillDecodes(t *testing.T) {
	req := r99RespReq(t, `{"type":"input_image","image_url":"https://example.com/a.png"}`)
	if got := r99SoleImage(t, req).URL; got != "https://example.com/a.png" {
		t.Fatalf("裸字符串形态解出的 URL = %q", got)
	}
}

// 两种形态混在一轮里：字符串那条不得牵连对象那条（单 part 解析失败只丢它自己）。
func TestBothImageURLFormsCoexistInOneMessage(t *testing.T) {
	req := r99ChatReq(t,
		`{"type":"image_url","image_url":"https://example.com/a.png"}`,
		`{"type":"image_url","image_url":{"url":"https://example.com/b.png"}}`,
		`{"type":"text","text":"compare"}`)
	blocks := req.Messages[0].Content
	images := 0
	for _, b := range blocks {
		if b.Type == ir.BlockImage {
			images++
		}
	}
	if images != 2 {
		t.Fatalf("解出图片块 %d 个, want 2；blocks=%+v", images, blocks)
	}
	out := r99EncodeAll(t, req)
	for name, body := range out {
		for _, want := range []string{"https://example.com/a.png", "https://example.com/b.png", "compare"} {
			if !strings.Contains(body, want) {
				t.Errorf("%s 出站丢了 %q：%s", name, want, body)
			}
		}
	}
}

// ---- 2. detail 贯通 ----

// 同族往返必须无损：Chat 客户端发 detail、路由到 Chat 上游，此前档位被丢掉。
func TestImageDetailSurvivesSameFamilyRoundTrip(t *testing.T) {
	for _, tc := range []struct{ inbound, outbound, want string }{
		{"openai-chat", "openai-chat", `"detail":"low"`},
		{"openai-responses", "openai-responses", `"detail":"low"`},
		{"openai-responses", "codex", `"detail":"low"`},
		{"openai-chat", "openai-responses", `"detail":"low"`},
		{"openai-responses", "openai-chat", `"detail":"low"`},
	} {
		t.Run(tc.inbound+"->"+tc.outbound, func(t *testing.T) {
			var req *ir.Request
			part := `{"type":"image_url","image_url":{"url":"https://example.com/a.png","detail":"low"}}`
			if tc.inbound == "openai-responses" {
				// Responses 的规范位置是 part 顶层，与 image_url 平级。
				req = r99RespReq(t, `{"type":"input_image","image_url":"https://example.com/a.png","detail":"low"}`)
			} else {
				req = r99ChatReq(t, part)
			}
			if got := r99SoleImage(t, req).Detail; got != "low" {
				t.Fatalf("IR 里的 Detail = %q, want low", got)
			}
			if body := r99Encode(t, tc.outbound, req); !strings.Contains(body, tc.want) {
				t.Errorf("%s 出站丢了 detail：%s", tc.outbound, body)
			}
		})
	}
}

// detail 缺省时出站必须保持缺省。替客户端补一个值就是改写请求：补 "auto" 是把
// 「未指定」说成「明确要求 auto」，补 "high"（new-api 的做法）更糟——凭空按全
// 分辨率切图，输入 token 消耗直接抬上去。
func TestAbsentImageDetailIsNotFabricated(t *testing.T) {
	chat := r99ChatReq(t, `{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}`)
	if got := r99SoleImage(t, chat).Detail; got != "" {
		t.Fatalf("客户端没发 detail，IR 里却是 %q", got)
	}
	resp := r99RespReq(t, `{"type":"input_image","image_url":"https://example.com/a.png"}`)
	if got := r99SoleImage(t, resp).Detail; got != "" {
		t.Fatalf("客户端没发 detail，IR 里却是 %q", got)
	}
	for _, req := range []*ir.Request{chat, resp} {
		out := r99EncodeAll(t, req)
		r99AssertAbsent(t, out, "detail", "客户端没指定档位，出站不得凭空补一个")
	}
}

// Anthropic 的 image source 只有 base64 / url / text 三种，没有档位这一维：
// 出站不得写出一个上游不认识的 detail 键（丢失由 Diagnose 报，见 relay 侧测试）。
func TestImageDetailNotWrittenToAnthropic(t *testing.T) {
	req := r99ChatReq(t, `{"type":"image_url","image_url":{"url":"https://example.com/a.png","detail":"low"}}`)
	body := r99Encode(t, "anthropic", req)
	if strings.Contains(body, "detail") {
		t.Errorf("anthropic 出站出现 detail 键（官方没有这一维，会被按未知字段拒）：%s", body)
	}
	if !strings.Contains(body, `"type":"url","url":"https://example.com/a.png"`) {
		t.Errorf("图片本体不该跟着 detail 一起丢：%s", body)
	}
}

// Responses 两处都能带 detail（顶层是规范位，嵌套是 Chat 形态漏进来的）：
// 两处都给了以顶层为准，那是本族自己的键位。
func TestResponsesTopLevelDetailWinsOverNested(t *testing.T) {
	req := r99RespReq(t, `{"type":"input_image","image_url":{"url":"https://example.com/a.png","detail":"high"},"detail":"low"}`)
	if got := r99SoleImage(t, req).Detail; got != "low" {
		t.Fatalf("Detail = %q, want low（顶层优先）", got)
	}
	if body := r99Encode(t, "openai-responses", req); !strings.Contains(body, `"detail":"low"`) || strings.Contains(body, `"high"`) {
		t.Errorf("responses 出站档位不对：%s", body)
	}
}

// 嵌套形态是唯一给出档位的地方时，不能因为「规范位在顶层」就把它丢掉。
func TestResponsesNestedDetailUsedWhenTopLevelAbsent(t *testing.T) {
	req := r99RespReq(t, `{"type":"input_image","image_url":{"url":"https://example.com/a.png","detail":"high"}}`)
	if got := r99SoleImage(t, req).Detail; got != "high" {
		t.Fatalf("Detail = %q, want high", got)
	}
}

// data URI 形态同样要带档位：base64 内联是 detail 最常配的组合（远程 URL 上游
// 自己会去取，内联才完全由档位决定切图方式）。
func TestImageDataURLKeepsDetail(t *testing.T) {
	req := r99RespReq(t, `{"type":"input_image","image_url":"data:image/png;base64,QUJD","detail":"high"}`)
	img := r99SoleImage(t, req)
	if img.MediaType != "image/png" || img.Data != "QUJD" || img.Detail != "high" {
		t.Fatalf("data URI 拆解或档位丢失：Image=%+v", img)
	}
	out := r99EncodeAll(t, req)
	if !strings.Contains(out["anthropic"], `"media_type":"image/png","data":"QUJD"`) {
		t.Errorf("anthropic 出站 base64 source 不完整：%s", out["anthropic"])
	}
	for _, name := range []string{"codex", "openai-chat", "openai-responses"} {
		if !strings.Contains(out[name], `"detail":"high"`) {
			t.Errorf("%s 出站丢了 detail：%s", name, out[name])
		}
	}
}

// ---- 3. 无载荷图片不得编成非法部件 ----

// 三种非法形状的原文都在探针里实测过，逐个钉死。
func TestPayloadlessImageNeverEncodedAsBrokenPart(t *testing.T) {
	req := r99RespReq(t, `{"type":"input_image","image_ref":"vendor-specific-handle"}`)
	img := r99SoleImage(t, req)
	if img.HasPayload() || img.FileID != "" {
		t.Fatalf("这张图本该是无载荷的：Image=%+v", img)
	}
	out := r99EncodeAll(t, req)
	// anthropic：base64 source 缺 media_type 与 data（官方两者都是 Required）
	r99AssertAbsent(t, out, `{"type":"base64"}`, "缺必填键的 base64 source")
	// chat：url 为空串
	r99AssertAbsent(t, out, `{"url":""}`, "空 url 的 image_url")
	// responses / codex：只有判别值、没有载荷键
	r99AssertAbsent(t, out, `{"type":"input_image"}`, "没有 image_url / file_id 的 input_image")
	// 同一批结论再按部件型独立查一遍。上面三条只认精确到键序的整段字面量，
	// 编码器换个字段顺序或补一个键就绕过去了；「这张图根本没有部件在线上」
	// 才是本轮修复的结论本身，得有一个不依赖字面量形状的见证。
	for name, body := range out {
		for _, partType := range []string{`"type":"image"`, `"type":"input_image"`, `"image_url"`} {
			if strings.Contains(body, partType) {
				t.Errorf("%s 出站仍写出了图片部件 %s：%s", name, partType, body)
			}
		}
	}
}

// 图片被跳过后消息不能变空：三族此前处置不一致（anthropic 落占位、chat 落空
// 字符串 content、responses 整条消息从 input 里消失）。整条消失比空 content
// 更糟——若它是唯一的一条，input 会连键都没有，而上游把 input 当必填字段。
func TestMessageWhoseOnlyImageWasDroppedKeepsPlaceholder(t *testing.T) {
	req := r99RespReq(t, `{"type":"input_image","image_ref":"vendor-specific-handle"}`)
	out := r99EncodeAll(t, req)
	for _, name := range r99Outbound {
		body := out[name]
		if !strings.Contains(body, normalize.Placeholder) {
			t.Errorf("%s 出站没有约定占位，消息内容变空或被整条丢弃：%s", name, body)
		}
		// 占位必须是非空的约定字面量：空文本块等于给上游一条「用户什么都没说」
		// 的消息，客户端也无从分辨这是占位还是正文。
		if strings.Contains(body, `"content":""`) || strings.Contains(body, `"content":[]`) {
			t.Errorf("%s 出站出现空 content：%s", name, body)
		}
	}
	// 轮次结构不得被悄悄改写：客户端发了一条 user 消息，上游就得收到一条。
	if n := strings.Count(out["openai-responses"], `"role":"user"`); n != 1 {
		t.Errorf("responses 出站的 user 轮次数 = %d, want 1：%s", n, out["openai-responses"])
	}
	if n := strings.Count(out["openai-chat"], `"role":"user"`); n != 1 {
		t.Errorf("chat 出站的 user 轮次数 = %d, want 1：%s", n, out["openai-chat"])
	}
}

// 有效块不得被牵连：同一条消息里的文本照常投递，只有无载荷的图片被跳过。
func TestPayloadlessImageDoesNotDragDownSiblingBlocks(t *testing.T) {
	req := r99RespReq(t,
		`{"type":"input_image","image_ref":"vendor-specific-handle"}`,
		`{"type":"input_text","text":"describe it"}`)
	out := r99EncodeAll(t, req)
	for _, name := range r99Outbound {
		if !strings.Contains(out[name], "describe it") {
			t.Errorf("%s 出站丢了同轮的文本：%s", name, out[name])
		}
		if strings.Contains(out[name], normalize.Placeholder) {
			t.Errorf("%s 出站还有文本却落了占位：%s", name, out[name])
		}
	}
}

// ---- file_id：Responses 一族原生有这一维，其余三家没有 ----

// 同族往返必须无损。此前 file_id 被解出又丢掉，出站只剩一个空壳 input_image。
func TestFileIDOnlyImageSurvivesOnResponsesFamily(t *testing.T) {
	req := r99RespReq(t, `{"type":"input_image","file_id":"file_abc123"}`)
	img := r99SoleImage(t, req)
	if img.FileID != "file_abc123" {
		t.Fatalf("IR 里的 FileID = %q, want file_abc123", img.FileID)
	}
	if img.HasPayload() {
		t.Fatalf("只给 file_id 不该算有载荷（base64 与 URL 都空）：Image=%+v", img)
	}
	for _, name := range []string{"codex", "openai-responses"} {
		body := r99Encode(t, name, req)
		if !strings.Contains(body, `"file_id":"file_abc123"`) {
			t.Errorf("%s 出站丢了 file_id：%s", name, body)
		}
		if !strings.Contains(body, `"type":"input_image"`) {
			t.Errorf("%s 出站没保留图片部件：%s", name, body)
		}
		if strings.Contains(body, normalize.Placeholder) {
			t.Errorf("%s 出站把可投递的图片降级成了占位：%s", name, body)
		}
	}
}

// 投给没有这一维的目标：不得写出空壳部件，也不得把 file_id 涂进别的槽位。
func TestFileIDOnlyImageNotWrittenToSlotlessTargets(t *testing.T) {
	req := r99RespReq(t, `{"type":"input_image","file_id":"file_abc123"}`)
	for _, name := range []string{"anthropic", "openai-chat"} {
		body := r99Encode(t, name, req)
		if strings.Contains(body, "file_abc123") {
			t.Errorf("%s 出站出现了本族没有槽位的 file_id：%s", name, body)
		}
		if !strings.Contains(body, normalize.Placeholder) {
			t.Errorf("%s 出站既没投递也没落占位，消息内容变空：%s", name, body)
		}
	}
}

// file_id 与 detail 可以并存（都是 Responses 原生的一维），不得互相吃掉。
func TestFileIDAndDetailCoexist(t *testing.T) {
	req := r99RespReq(t, `{"type":"input_image","file_id":"file_abc123","detail":"low"}`)
	img := r99SoleImage(t, req)
	if img.FileID != "file_abc123" || img.Detail != "low" {
		t.Fatalf("Image=%+v, want FileID=file_abc123 Detail=low", img)
	}
	body := r99Encode(t, "openai-responses", req)
	if !strings.Contains(body, `"file_id":"file_abc123"`) || !strings.Contains(body, `"detail":"low"`) {
		t.Errorf("responses 出站两维没同时保住：%s", body)
	}
}

// 图片本体送达时随带的 file_id，在 chat / anthropic 无处安放：本体已经在了，
// 那个引用只是冗余载体，丢掉不值得报损耗。但「丢掉」不等于「换个键名塞进去」
// ——image_url 对象里没有 file_id 这个键，凭空写一个就是在发明协议：上游按
// 未知字段拒收是 400，宽容收下则是一个永远无人解释的幽灵引用。
//
// 这一态与上面那个 file_id-only 的用例互补：那边整个部件被跳过，没有键可查；
// 只有本体在场时，「发明键名」这条错路才走得通。
func TestRedundantFileIDNeverInventedAsAKeyOnSlotlessTargets(t *testing.T) {
	req := r99RespReq(t, `{"type":"input_image","image_url":"https://example.com/a.png","file_id":"file_abc123"}`)
	img := r99SoleImage(t, req)
	if !img.HasPayload() || img.FileID != "file_abc123" {
		t.Fatalf("夹具没构出「本体 + 冗余引用」这一态：Image=%+v", img)
	}
	for _, name := range []string{"anthropic", "openai-chat"} {
		body := r99Encode(t, name, req)
		if !strings.Contains(body, "https://example.com/a.png") {
			t.Errorf("%s 出站丢了图片本体：%s", name, body)
		}
		if strings.Contains(body, "file_id") || strings.Contains(body, "file_abc123") {
			t.Errorf("%s 出站把冗余的 file_id 发明成了一个键：%s", name, body)
		}
	}
	// 同族往返两维都在，这才是 file_id 唯一该出现的地方。
	if body := r99Encode(t, "openai-responses", req); !strings.Contains(body, `"file_id":"file_abc123"`) {
		t.Errorf("responses 出站丢了同族的 file_id：%s", body)
	}
}

// ---- 工具结果里的图片同样过这条路径 ----

// tool 结果里的图片被抽成紧随的 user 媒体消息；无载荷的那张不得写出空壳部件，
// 有载荷的照常投递。
func TestToolResultImagesGoThroughTheSameGuard(t *testing.T) {
	req := &ir.Request{Model: "m", MaxTokens: 16, Messages: []ir.Message{
		{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockText, Text: "go"}}},
		{Role: ir.RoleAssistant, Content: []ir.Block{{Type: ir.BlockToolUse,
			ToolUse: &ir.ToolUse{ID: "t1", Name: "shot"}}}},
		{Role: ir.RoleUser, Content: []ir.Block{{Type: ir.BlockToolResult, ToolResult: &ir.ToolResult{
			ToolUseID: "t1",
			Content: []ir.Block{
				{Type: ir.BlockText, Text: "here"},
				{Type: ir.BlockImage, Image: &ir.Image{URL: "https://example.com/ok.png", Detail: "low"}},
				{Type: ir.BlockImage, Image: &ir.Image{}},
				{Type: ir.BlockImage, Image: nil},
			},
		}}}},
	}}
	out := r99EncodeAll(t, req)
	r99AssertAbsent(t, out, `{"url":""}`, "空 url 的 image_url")
	r99AssertAbsent(t, out, `{"type":"base64"}`, "缺必填键的 base64 source")
	for _, name := range r99Outbound {
		if !strings.Contains(out[name], "https://example.com/ok.png") {
			t.Errorf("%s 出站丢了工具结果里有载荷的图片：%s", name, out[name])
		}
	}
	for _, name := range []string{"codex", "openai-chat", "openai-responses"} {
		if !strings.Contains(out[name], `"detail":"low"`) {
			t.Errorf("%s 出站丢了工具结果图片的 detail：%s", name, out[name])
		}
	}
}

// nil 的 Image 指针不得让编码器 panic，也不得写出只有判别值的空壳部件。
func TestNilImagePointerIsSkippedNotEmitted(t *testing.T) {
	req := &ir.Request{Model: "m", MaxTokens: 16, Messages: []ir.Message{
		{Role: ir.RoleUser, Content: []ir.Block{
			{Type: ir.BlockText, Text: "look"},
			{Type: ir.BlockImage, Image: nil},
		}},
	}}
	out := r99EncodeAll(t, req)
	for _, name := range r99Outbound {
		if !strings.Contains(out[name], "look") {
			t.Errorf("%s 出站丢了文本：%s", name, out[name])
		}
	}
	r99AssertAbsent(t, out, `{"type":"input_image"}`, "没有载荷键的 input_image")
	r99AssertAbsent(t, out, `{"url":""}`, "空 url 的 image_url")
}

// HasPayload 的三态判定本身：这是所有编码侧跳过分支的共同前提。
func TestImageHasPayloadCoversEveryCarrier(t *testing.T) {
	for _, tc := range []struct {
		name string
		img  *ir.Image
		want bool
	}{
		{"nil 指针", nil, false},
		{"全空", &ir.Image{}, false},
		{"只有 MIME", &ir.Image{MediaType: "image/png"}, false},
		{"只有 detail", &ir.Image{Detail: "low"}, false},
		// file_id 不算载荷：只有 Responses 一族的图片槽位认它，其余三家投不过去。
		// 这一条是刻意的——把它算成载荷会让 anthropic / chat 编出空壳部件。
		{"只有 file_id", &ir.Image{FileID: "f1"}, false},
		{"base64", &ir.Image{MediaType: "image/png", Data: "QUJD"}, true},
		{"URL", &ir.Image{URL: "https://example.com/a.png"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.img.HasPayload(); got != tc.want {
				t.Errorf("HasPayload() = %v, want %v（Image=%+v）", got, tc.want, tc.img)
			}
		})
	}
}

// r99SoleImage 取出请求里唯一的图片块。数量不对就直接失败：图片块被解码器整块
// 丢掉时，后续断言会拿着零值继续跑，报出来的症状与真正的缺口对不上。
func r99SoleImage(t *testing.T, req *ir.Request) *ir.Image {
	t.Helper()
	if len(req.Messages) != 1 {
		t.Fatalf("消息数 = %d, want 1", len(req.Messages))
	}
	var found *ir.Image
	n := 0
	for _, b := range req.Messages[0].Content {
		if b.Type == ir.BlockImage {
			found, n = b.Image, n+1
		}
	}
	if n != 1 {
		t.Fatalf("图片块数 = %d, want 1；blocks=%+v", n, req.Messages[0].Content)
	}
	if found == nil {
		t.Fatal("图片块的 Image 指针为 nil")
	}
	return found
}
